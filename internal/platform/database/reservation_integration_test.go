//go:build integration

// These tests exercise the storage-quota accounting against a real PostgreSQL
// schema, because that accounting lives in SQL and an in-memory fake would
// only assert that the fake agrees with itself.
//
// Run them against a migrated database:
//
//	make test-integration-db TEST_DATABASE_URL=postgres://...
package database

import (
	"context"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, sqlDB, err := Open(url, 10, 5, time.Minute)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// seedEvent creates an isolated host, event and guest session and removes them
// when the test ends, so runs never interfere with each other.
func seedEvent(t *testing.T, db *gorm.DB, quota int64) (uuid.UUID, uuid.UUID) {
	t.Helper()
	userID, eventID, sessionID := uuid.New(), uuid.New(), uuid.New()
	require.NoError(t, db.Exec(
		`INSERT INTO users (id, better_auth_user_id, email) VALUES (?, ?, ?)`,
		userID, "test-"+userID.String(), userID.String()+"@example.test").Error)
	require.NoError(t, db.Exec(
		`INSERT INTO events (id, host_id, name, slug, max_media_bytes) VALUES (?, ?, 'quota test', ?, ?)`,
		eventID, userID, "evt_"+uuid.NewString()[:12], quota).Error)
	require.NoError(t, db.Exec(
		`INSERT INTO guest_sessions (id, event_id, token_hash, expires_at) VALUES (?, ?, ?, ?)`,
		sessionID, eventID, []byte(uuid.NewString()), time.Now().Add(time.Hour)).Error)

	t.Cleanup(func() {
		db.Exec(`DELETE FROM media_jobs WHERE media_id IN (SELECT id FROM media WHERE event_id = ?)`, eventID)
		db.Exec(`DELETE FROM media WHERE event_id = ?`, eventID)
		db.Exec(`DELETE FROM guest_sessions WHERE event_id = ?`, eventID)
		db.Exec(`DELETE FROM events WHERE id = ?`, eventID)
		db.Exec(`DELETE FROM users WHERE id = ?`, userID)
	})
	return eventID, sessionID
}

// uniqueChecksum returns a distinct value that satisfies the schema's
// 64-lowercase-hex format constraint.
func uniqueChecksum() string {
	a, b := uuid.New(), uuid.New()
	return hex.EncodeToString(append(a[:], b[:]...))
}

func pendingRecord(eventID, sessionID uuid.UUID, size int64) media.Media {
	id := uuid.New()
	clientID := uuid.New()
	return media.Media{
		ID: id, EventID: eventID, GuestSessionID: sessionID,
		ObjectKey:        "events/" + eventID.String() + "/media/" + id.String() + "/original.jpg",
		OriginalFilename: "guest.jpg", MIMEType: "image/jpeg",
		ExpectedSize: size, ChecksumSHA256: uniqueChecksum(),
		ClientUploadID: &clientID, Status: media.StatusPending,
		LastActivityAt: time.Now().UTC(),
	}
}

// reservedCounter reads the denormalised counter and the value it is supposed
// to equal, so a drift shows up as a diff rather than a vague failure.
func reservedCounter(t *testing.T, db *gorm.DB, eventID uuid.UUID) (counter, truth int64) {
	t.Helper()
	var evt event.Event
	require.NoError(t, db.First(&evt, "id = ?", eventID).Error)
	require.NoError(t, db.Raw(`
		SELECT COALESCE(SUM(expected_size), 0) FROM media
		WHERE event_id = ? AND status IN ('pending', 'uploading', 'uploaded')`, eventID).Scan(&truth).Error)
	return evt.ReservedMediaBytes, truth
}

func TestReservedBytesSurviveTheFullUploadLifecycle(t *testing.T) {
	db := openTestDB(t)
	repo := NewMediaRepository(db)
	ctx := context.Background()
	eventID, sessionID := seedEvent(t, db, 1_000)

	completed := pendingRecord(eventID, sessionID, 400)
	abandoned := pendingRecord(eventID, sessionID, 400)
	require.NoError(t, repo.ReserveUpload(ctx, completed, 0))
	require.NoError(t, repo.ReserveUpload(ctx, abandoned, 0))

	counter, truth := reservedCounter(t, db, eventID)
	require.EqualValues(t, 800, counter)
	require.Equal(t, truth, counter)

	// A third upload does not fit beside the two outstanding reservations.
	require.Error(t, repo.ReserveUpload(ctx, pendingRecord(eventID, sessionID, 400), 0),
		"quota must account for bytes that are reserved but not yet uploaded")

	// Completing converts the reservation into usage at the real size.
	require.NoError(t, repo.MarkReady(ctx, eventID, completed.ID, 380, time.Now().UTC()))
	var evt event.Event
	require.NoError(t, db.First(&evt, "id = ?", eventID).Error)
	require.EqualValues(t, 380, evt.UsedMediaBytes)
	require.EqualValues(t, 400, evt.ReservedMediaBytes, "only the completed reservation is released")

	// Abandoning releases the rest.
	require.NoError(t, repo.Delete(ctx, abandoned.ID))
	counter, truth = reservedCounter(t, db, eventID)
	require.Zero(t, counter)
	require.Equal(t, truth, counter)

	// With the quota freed, a new upload is accepted again.
	require.NoError(t, repo.ReserveUpload(ctx, pendingRecord(eventID, sessionID, 400), 0))
	counter, truth = reservedCounter(t, db, eventID)
	require.Equal(t, truth, counter, "counter drifted from the outstanding reservations")
}

// The counter is the only thing bounding an event's storage, so concurrent
// reservations must not be able to oversubscribe it.
func TestConcurrentReservationsCannotExceedTheQuota(t *testing.T) {
	db := openTestDB(t)
	repo := NewMediaRepository(db)
	ctx := context.Background()
	eventID, sessionID := seedEvent(t, db, 1_000)

	const attempts = 20
	results := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			results <- repo.ReserveUpload(ctx, pendingRecord(eventID, sessionID, 100), 0)
		}()
	}
	accepted := 0
	for i := 0; i < attempts; i++ {
		if err := <-results; err == nil {
			accepted++
		}
	}

	require.Equal(t, 10, accepted, "exactly the quota's worth of reservations may be granted")
	counter, truth := reservedCounter(t, db, eventID)
	require.EqualValues(t, 1_000, counter)
	require.Equal(t, truth, counter)
}

// A completed upload must release exactly what it reserved even when several
// uploads finish at once.
func TestConcurrentCompletionsKeepTheCounterExact(t *testing.T) {
	db := openTestDB(t)
	repo := NewMediaRepository(db)
	ctx := context.Background()
	eventID, sessionID := seedEvent(t, db, 10_000)

	const uploads = 12
	records := make([]media.Media, 0, uploads)
	for i := 0; i < uploads; i++ {
		record := pendingRecord(eventID, sessionID, 100)
		require.NoError(t, repo.ReserveUpload(ctx, record, 0))
		records = append(records, record)
	}

	done := make(chan error, uploads)
	for _, record := range records {
		go func(record media.Media) {
			done <- repo.MarkReady(ctx, eventID, record.ID, 90, time.Now().UTC())
		}(record)
	}
	for range records {
		require.NoError(t, <-done)
	}

	var evt event.Event
	require.NoError(t, db.First(&evt, "id = ?", eventID).Error)
	require.EqualValues(t, uploads*90, evt.UsedMediaBytes)
	require.Zero(t, evt.ReservedMediaBytes, "every reservation must be released exactly once")
}

// Completing an image enqueues its thumbnail in the same transaction, so the
// job cannot be lost the way the previous in-process goroutine could.
func TestMarkReadyEnqueuesThumbnailWorkForImagesOnly(t *testing.T) {
	db := openTestDB(t)
	repo := NewMediaRepository(db)
	ctx := context.Background()
	eventID, sessionID := seedEvent(t, db, 10_000)

	image := pendingRecord(eventID, sessionID, 100)
	video := pendingRecord(eventID, sessionID, 100)
	video.MIMEType = "video/mp4"
	video.ObjectKey += ".mp4"
	require.NoError(t, repo.ReserveUpload(ctx, image, 0))
	require.NoError(t, repo.ReserveUpload(ctx, video, 0))

	require.NoError(t, repo.MarkReady(ctx, eventID, image.ID, 90, time.Now().UTC()))
	require.NoError(t, repo.MarkReady(ctx, eventID, video.ID, 90, time.Now().UTC()))

	var queued []uuid.UUID
	require.NoError(t, db.Raw(
		`SELECT media_id FROM media_jobs WHERE kind = 'thumbnail' AND media_id IN (?, ?)`,
		image.ID, video.ID).Scan(&queued).Error)
	require.Equal(t, []uuid.UUID{image.ID}, queued, "only images need a thumbnail")
}

// Archiving removes the original from hot storage. The listing has to learn
// that, or the gallery would sign a key whose object is gone.
func TestArchivingMarksMediaSoTheGalleryStopsSigningIt(t *testing.T) {
	db := openTestDB(t)
	repo := NewMediaRepository(db)
	archiveRepo := NewArchiveRepository(db)
	ctx := context.Background()
	eventID, sessionID := seedEvent(t, db, 10_000)

	record := pendingRecord(eventID, sessionID, 100)
	require.NoError(t, repo.ReserveUpload(ctx, record, 0))
	require.NoError(t, repo.MarkReady(ctx, eventID, record.ID, 90, time.Now().UTC()))

	before, err := repo.FindByID(ctx, record.ID)
	require.NoError(t, err)
	require.False(t, before.SourceArchived(), "a fresh upload is still in hot storage")

	// The archive job verifies the long-term copy, then deletes the original.
	require.NoError(t, archiveRepo.RecordVerified(ctx, record.ID, "drive-file-id", 90, uniqueChecksum(), time.Now().UTC()))
	require.NoError(t, archiveRepo.MarkSourceDeleted(ctx, record.ID, record.ObjectKey, time.Now().UTC()))

	after, err := repo.FindByID(ctx, record.ID)
	require.NoError(t, err)
	require.True(t, after.SourceArchived(), "the listing must know the original is gone")

	// The same record is still discoverable through the application route.
	location, err := repo.FindArchivedLocation(ctx, eventID, record.ID)
	require.NoError(t, err)
	require.Equal(t, "drive-file-id", location)

	// And a gallery listing carries the flag through to the view.
	listed, err := repo.ListReady(ctx, eventID, 10, nil)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.True(t, listed[0].SourceArchived())
}

// Cleanup removes abandoned uploads in one statement. The releases are summed
// per event first, so several abandoned uploads on the same event subtract
// once rather than visiting the event row repeatedly.
func TestDeleteManyUploadsReleasesQuotaPerEvent(t *testing.T) {
	db := openTestDB(t)
	repo := NewMediaRepository(db)
	ctx := context.Background()
	firstEvent, firstSession := seedEvent(t, db, 10_000)
	secondEvent, secondSession := seedEvent(t, db, 10_000)

	var abandoned []uuid.UUID
	for i := 0; i < 3; i++ {
		record := pendingRecord(firstEvent, firstSession, 100)
		require.NoError(t, repo.ReserveUpload(ctx, record, 0))
		abandoned = append(abandoned, record.ID)
	}
	other := pendingRecord(secondEvent, secondSession, 250)
	require.NoError(t, repo.ReserveUpload(ctx, other, 0))
	abandoned = append(abandoned, other.ID)

	// One that completed must not have its bytes released by the cleanup.
	kept := pendingRecord(firstEvent, firstSession, 100)
	require.NoError(t, repo.ReserveUpload(ctx, kept, 0))
	require.NoError(t, repo.MarkReady(ctx, firstEvent, kept.ID, 90, time.Now().UTC()))

	counter, truth := reservedCounter(t, db, firstEvent)
	require.EqualValues(t, 300, counter)
	require.Equal(t, truth, counter)

	require.NoError(t, repo.DeleteManyUploads(ctx, abandoned))

	counter, truth = reservedCounter(t, db, firstEvent)
	require.Zero(t, counter, "three abandoned uploads on one event release exactly their bytes")
	require.Equal(t, truth, counter)

	counter, truth = reservedCounter(t, db, secondEvent)
	require.Zero(t, counter, "the batch spanned two events and settled both")
	require.Equal(t, truth, counter)

	// The completed upload is untouched: its bytes live in used, not reserved.
	var evt event.Event
	require.NoError(t, db.First(&evt, "id = ?", firstEvent).Error)
	require.EqualValues(t, 90, evt.UsedMediaBytes)

	// An empty batch is a no-op rather than an error.
	require.NoError(t, repo.DeleteManyUploads(ctx, nil))
}
