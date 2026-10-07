//go:build integration

package database

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type itemCounters struct {
	UploadedMediaItems, ReservedMediaItems int64
	UploadedPhotoItems, ReservedPhotoItems int64
	UploadedVideoItems, ReservedVideoItems int64
}

func readItemCounters(t *testing.T, db *gorm.DB, eventID uuid.UUID) itemCounters {
	t.Helper()
	var c []itemCounters
	require.NoError(t, db.Raw(`SELECT uploaded_media_items, reserved_media_items, uploaded_photo_items,
		reserved_photo_items, uploaded_video_items, reserved_video_items FROM events WHERE id = ?`, eventID).Scan(&c).Error)
	require.Len(t, c, 1)
	return c[0]
}

// seedFreeEvent is an event carrying the Free plan's item limits.
func seedFreeEvent(t *testing.T, db *gorm.DB) (uuid.UUID, uuid.UUID) {
	t.Helper()
	eventID, sessionID := seedEvent(t, db, 1<<40)
	require.NoError(t, db.Exec(`UPDATE events SET max_media_items = 33, max_photo_items = 30, max_video_items = 3 WHERE id = ?`, eventID).Error)
	return eventID, sessionID
}

func videoRecord(eventID, sessionID uuid.UUID) media.Media {
	record := pendingRecord(eventID, sessionID, 10)
	record.MIMEType = "video/mp4"
	record.ObjectKey = "events/" + eventID.String() + "/media/" + record.ID.String() + "/original.mp4"
	return record
}

var enforce = media.ReserveLimits{EnforceItems: true}

func TestConcurrentPhotoReservationsStopAtThePlanLimit(t *testing.T) {
	db := openTestDB(t)
	repo := NewMediaRepository(db)
	eventID, sessionID := seedFreeEvent(t, db)

	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted, refused := 0, 0
	for range 45 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := repo.ReserveUpload(context.Background(), pendingRecord(eventID, sessionID, 10), enforce)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				accepted++
				return
			}
			var quota *media.QuotaError
			require.True(t, errors.As(err, &quota), "unexpected error: %v", err)
			require.Equal(t, media.ResourcePhoto, quota.Resource)
			refused++
		}()
	}
	wg.Wait()
	require.Equal(t, 30, accepted, "exactly the Free photo limit is reserved under contention")
	require.Equal(t, 15, refused)
	c := readItemCounters(t, db, eventID)
	require.EqualValues(t, 30, c.ReservedPhotoItems)
	require.EqualValues(t, 30, c.ReservedMediaItems)
}

func TestVideoLimitAndLifecycleCounters(t *testing.T) {
	db := openTestDB(t)
	repo := NewMediaRepository(db)
	ctx := context.Background()
	eventID, sessionID := seedFreeEvent(t, db)

	videos := make([]media.Media, 3)
	for i := range videos {
		videos[i] = videoRecord(eventID, sessionID)
		require.NoError(t, repo.ReserveUpload(ctx, videos[i], enforce))
	}
	err := repo.ReserveUpload(ctx, videoRecord(eventID, sessionID), enforce)
	var quota *media.QuotaError
	require.True(t, errors.As(err, &quota))
	require.Equal(t, media.ResourceVideo, quota.Resource)
	require.EqualValues(t, 3, quota.Limit)

	// Completing moves the reservation into lifetime usage.
	require.NoError(t, repo.MarkReady(ctx, eventID, videos[0].ID, 10, time.Now().UTC()))
	// Abandoning gives the reservation back.
	require.NoError(t, repo.Delete(ctx, videos[1].ID))
	c := readItemCounters(t, db, eventID)
	require.Equal(t, itemCounters{UploadedMediaItems: 1, ReservedMediaItems: 1, UploadedVideoItems: 1, ReservedVideoItems: 1}, c)

	// An abandoned slot can be used again.
	require.NoError(t, repo.ReserveUpload(ctx, videoRecord(eventID, sessionID), enforce))

	// Deleting completed media frees storage but not upload allowance.
	require.NoError(t, repo.DeleteForEvent(ctx, eventID, videos[0].ID))
	c = readItemCounters(t, db, eventID)
	require.EqualValues(t, 1, c.UploadedVideoItems, "deletion does not refund an upload")
	require.Error(t, repo.ReserveUpload(ctx, videoRecord(eventID, sessionID), enforce))

	// Without enforcement the limits do not apply, but counting continues.
	require.NoError(t, repo.ReserveUpload(ctx, videoRecord(eventID, sessionID), media.ReserveLimits{}))
	c = readItemCounters(t, db, eventID)
	require.EqualValues(t, 3, c.ReservedVideoItems)
}

func TestTotalMediaLimitAppliesAcrossTypes(t *testing.T) {
	db := openTestDB(t)
	repo := NewMediaRepository(db)
	ctx := context.Background()
	eventID, sessionID := seedFreeEvent(t, db)
	require.NoError(t, db.Exec(`UPDATE events SET max_media_items = 2, max_photo_items = 0, max_video_items = 0 WHERE id = ?`, eventID).Error)

	require.NoError(t, repo.ReserveUpload(ctx, pendingRecord(eventID, sessionID, 10), enforce))
	require.NoError(t, repo.ReserveUpload(ctx, videoRecord(eventID, sessionID), enforce))
	err := repo.ReserveUpload(ctx, pendingRecord(eventID, sessionID, 10), enforce)
	var quota *media.QuotaError
	require.True(t, errors.As(err, &quota))
	require.Equal(t, media.ResourceMedia, quota.Resource)
}

func TestByteRefusalIsReportedAsStorage(t *testing.T) {
	db := openTestDB(t)
	repo := NewMediaRepository(db)
	eventID, sessionID := seedEvent(t, db, 100)

	err := repo.ReserveUpload(context.Background(), pendingRecord(eventID, sessionID, 101), enforce)
	var quota *media.QuotaError
	require.True(t, errors.As(err, &quota))
	require.Equal(t, media.ResourceBytes, quota.Resource)
	require.EqualValues(t, 100, quota.Limit)

	err = repo.ReserveUpload(context.Background(), pendingRecord(eventID, sessionID, 60), media.ReserveLimits{MaxEventBytes: 50})
	require.True(t, errors.As(err, &quota))
	require.EqualValues(t, 50, quota.Limit, "the legacy override is the limit reported")
}
