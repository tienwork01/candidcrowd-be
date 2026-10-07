//go:build integration

package retention

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/database"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestIntegrationPurgeLifecycle(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, sqlDB, err := database.Open(url, 5, 2, time.Minute)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	ctx := context.Background()

	hostID := uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO users (id, better_auth_user_id, email) VALUES (?, ?, ?)`,
		hostID, "ret-"+hostID.String(), hostID.String()+"@example.test").Error)
	plans := catalog.NewService(catalog.NewGormRepository(db))
	events := event.NewService(event.NewGormRepository(db, entitlement.NewProvisioner(plans)), 5<<30)

	expired, err := events.Create(ctx, hostID, event.CreateInput{Name: "expired"})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`UPDATE events SET trial_ended_at = now() WHERE id = ?`, expired.ID).Error)
	current, err := events.Create(ctx, hostID, event.CreateInput{Name: "current"})
	require.NoError(t, err)
	t.Cleanup(func() { cleanup(db, hostID, expired.ID, current.ID) })

	// Move the first event's storage period well into the past.
	past := time.Now().AddDate(0, -3, 0)
	require.NoError(t, db.Exec(`UPDATE event_plan_grants SET retention_expires_at = ?, upload_expires_at = ?
		WHERE event_id = ? AND status = 'active'`, past, past, expired.ID).Error)

	session := uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO guest_sessions (id, event_id, token_hash, expires_at) VALUES (?, ?, ?, ?)`,
		session, expired.ID, []byte(uuid.NewString()), time.Now().Add(time.Hour)).Error)
	mediaID := uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO media (id, event_id, guest_session_id, object_key, original_filename, mime_type, expected_size, actual_size, status)
		VALUES (?, ?, ?, ?, 'a.jpg', 'image/jpeg', 10, 10, 'ready')`,
		mediaID, expired.ID, session, "events/"+expired.ID.String()+"/media/"+mediaID.String()+"/original.jpg").Error)
	require.NoError(t, db.Exec(`INSERT INTO storage_replicas (media_id, provider, location_reference, state)
		VALUES (?, 'google_drive', 'drive-1', 'verified')`, mediaID).Error)
	require.NoError(t, db.Exec(`INSERT INTO event_exports (event_id, status, object_key) VALUES (?, 'ready', ?)`,
		expired.ID, "events/"+expired.ID.String()+"/exports/x.zip").Error)
	require.NoError(t, db.Exec(`UPDATE events SET used_media_bytes = 10, uploaded_media_items = 1, uploaded_photo_items = 1 WHERE id = ?`,
		expired.ID).Error)

	repo := NewGormRepository(db)
	cutoff := time.Now().Add(-DefaultGrace)
	candidates, err := repo.Candidates(ctx, cutoff, 1000)
	require.NoError(t, err)
	require.True(t, contains(candidates, expired.ID))
	require.False(t, contains(candidates, current.ID), "an event inside its storage period is never a candidate")

	replicas, err := repo.ArchivedReplicas(ctx, expired.ID)
	require.NoError(t, err)
	require.Equal(t, []Replica{{MediaID: mediaID, Location: "drive-1"}}, replicas)
	require.NoError(t, repo.MarkReplicaDeleted(ctx, replicas[0], time.Now()))

	purged, err := repo.MarkPurged(ctx, expired.ID, time.Now())
	require.NoError(t, err)
	require.Equal(t, Purged{Media: 1, Exports: 1}, purged)

	var row struct {
		Status             string
		UsedMediaBytes     int64
		UploadedMediaItems int64
		Purged             bool
	}
	require.NoError(t, db.Raw(`SELECT status, used_media_bytes, uploaded_media_items, media_purged_at IS NOT NULL AS purged
		FROM events WHERE id = ?`, expired.ID).Scan(&row).Error)
	require.Equal(t, "closed", row.Status, "a purged event no longer accepts uploads")
	require.Zero(t, row.UsedMediaBytes)
	require.EqualValues(t, 1, row.UploadedMediaItems, "lifetime upload counts are history, not storage")
	require.True(t, row.Purged)

	var mediaStatus, exportStatus, replicaState string
	require.NoError(t, db.Raw(`SELECT status FROM media WHERE id = ?`, mediaID).Scan(&mediaStatus).Error)
	require.Equal(t, "deleted", mediaStatus)
	require.NoError(t, db.Raw(`SELECT status FROM event_exports WHERE event_id = ?`, expired.ID).Scan(&exportStatus).Error)
	require.Equal(t, "failed", exportStatus)
	require.NoError(t, db.Raw(`SELECT state FROM storage_replicas WHERE media_id = ?`, mediaID).Scan(&replicaState).Error)
	require.Equal(t, "deleted", replicaState)

	candidates, err = repo.Candidates(ctx, cutoff, 1000)
	require.NoError(t, err)
	require.False(t, contains(candidates, expired.ID), "a purged event is not picked up again")
}

func contains(candidates []Candidate, id uuid.UUID) bool {
	for _, c := range candidates {
		if c.EventID == id {
			return true
		}
	}
	return false
}

func cleanup(db *gorm.DB, hostID uuid.UUID, eventIDs ...uuid.UUID) {
	for _, id := range eventIDs {
		db.Exec(`DELETE FROM storage_replicas WHERE media_id IN (SELECT id FROM media WHERE event_id = ?)`, id)
		db.Exec(`DELETE FROM media WHERE event_id = ?`, id)
		db.Exec(`DELETE FROM event_exports WHERE event_id = ?`, id)
		db.Exec(`DELETE FROM guest_sessions WHERE event_id = ?`, id)
		db.Exec(`DELETE FROM event_plan_grants WHERE event_id = ?`, id)
		db.Exec(`DELETE FROM events WHERE id = ?`, id)
	}
	db.Exec(`DELETE FROM users WHERE id = ?`, hostID)
}
