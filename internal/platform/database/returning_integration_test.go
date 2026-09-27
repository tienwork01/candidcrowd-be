//go:build integration

package database

import (
	"context"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/insights"
	"github.com/candidcrowd/candidcrowd-backend/internal/livewall"
	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// These cover the repositories that were changed from "UPDATE then SELECT the
// row back" to a single statement with RETURNING. The saving is one round trip
// each, but the reason they are tested here is that GORM's scan-into-model
// behaviour for RETURNING is the kind of thing that silently returns a zero
// struct rather than failing loudly.

func hostOf(t *testing.T, db *gorm.DB, eventID uuid.UUID) uuid.UUID {
	t.Helper()
	// Scanned as text: GORM treats a bare uuid.UUID destination as [16]byte
	// and tries to convert the driver's string element by element.
	var raw string
	require.NoError(t, db.Raw(`SELECT host_id::text FROM events WHERE id = ?`, eventID).Scan(&raw).Error)
	hostID, err := uuid.Parse(raw)
	require.NoError(t, err)
	return hostID
}

func TestEventUpdateReturnsTheUpdatedRow(t *testing.T) {
	db := openTestDB(t)
	repo := event.NewGormRepository(db)
	ctx := context.Background()
	eventID, _ := seedEvent(t, db, 10_000)

	updated, err := repo.UpdateOwned(ctx, eventID, hostOf(t, db, eventID), map[string]interface{}{
		"name":            "renamed",
		"gallery_enabled": false,
	})
	require.NoError(t, err)
	require.Equal(t, eventID, updated.ID, "RETURNING must populate the model, not leave it zeroed")
	require.Equal(t, "renamed", updated.Name)
	require.False(t, updated.GalleryEnabled)
	require.NotZero(t, updated.CreatedAt, "columns not named in the update still come back")

	// A row outside the caller's ownership is still refused.
	_, err = repo.UpdateOwned(ctx, eventID, uuid.New(), map[string]interface{}{"name": "nope"})
	require.ErrorIs(t, err, event.ErrNotFound)
}

func TestQRSourceUpdateReturnsTheUpdatedRow(t *testing.T) {
	db := openTestDB(t)
	repo := insights.NewGormRepository(db)
	ctx := context.Background()
	eventID, _ := seedEvent(t, db, 10_000)

	created, err := repo.CreateSource(ctx, insights.QRSource{
		ID: uuid.New(), EventID: eventID, Code: "entrance", Name: "Entrance",
	})
	require.NoError(t, err)

	updated, err := repo.UpdateSource(ctx, eventID, created.ID, "Front door")
	require.NoError(t, err)
	require.Equal(t, created.ID, updated.ID)
	require.Equal(t, "Front door", updated.Name)
	require.Equal(t, "entrance", updated.Code, "untouched columns must still be returned")

	_, err = repo.UpdateSource(ctx, eventID, uuid.New(), "missing")
	require.ErrorIs(t, err, insights.ErrNotFound)
}

func TestMediaStatusUpdateReturnsTheModeratedRow(t *testing.T) {
	db := openTestDB(t)
	repo := NewMediaRepository(db)
	ctx := context.Background()
	eventID, sessionID := seedEvent(t, db, 10_000)

	record := pendingRecord(eventID, sessionID, 100)
	require.NoError(t, repo.ReserveUpload(ctx, record, 0))
	require.NoError(t, repo.MarkReady(ctx, eventID, record.ID, 90, time.Now().UTC()))

	updated, err := repo.UpdateStatus(ctx, eventID, record.ID, media.StatusFeatured)
	require.NoError(t, err)
	require.Equal(t, record.ID, updated.ID)
	require.Equal(t, media.StatusFeatured, updated.Status)
	require.Equal(t, record.ObjectKey, updated.ObjectKey, "the broadcast that follows needs the whole row")

	_, err = repo.UpdateStatus(ctx, eventID, uuid.New(), media.StatusHidden)
	require.ErrorIs(t, err, media.ErrNotFound)
}

func TestLiveWallCommandReturnsTheUpdatedSession(t *testing.T) {
	db := openTestDB(t)
	repo := livewall.NewGormRepository(db)
	ctx := context.Background()
	eventID, _ := seedEvent(t, db, 10_000)
	t.Cleanup(func() { db.Exec(`DELETE FROM live_wall_sessions WHERE event_id = ?`, eventID) })

	now := time.Now().UTC()
	created, err := repo.Create(ctx, livewall.Session{
		ID: uuid.New(), EventID: eventID, EventName: "W", EventSlug: "evt_lw",
		TokenHash: uuid.NewString(), Status: livewall.StatusLive,
		ContentPolicy: livewall.ContentPolicyAutoApproved,
		CTAEveryMedia: livewall.DefaultCTAEveryMedia,
		ExpiresAt:     now.Add(time.Hour), CreatedAt: now,
	})
	require.NoError(t, err)
	require.True(t, created.IsPlaying)

	// Every press of the host's remote goes through this path.
	paused, err := repo.Command(ctx, eventID, created.ID, livewall.CommandPause)
	require.NoError(t, err)
	require.Equal(t, created.ID, paused.ID, "RETURNING must populate the session")
	require.False(t, paused.IsPlaying)
	require.EqualValues(t, created.Revision+1, paused.Revision)
	require.Equal(t, "W", paused.EventName, "untouched columns still come back")

	// Revision must advance on every command, including ones that change no
	// stored field, because players use it to detect transport events.
	next, err := repo.Command(ctx, eventID, created.ID, livewall.CommandNext)
	require.NoError(t, err)
	require.EqualValues(t, paused.Revision+1, next.Revision)

	policy, err := repo.UpdateContentPolicy(ctx, eventID, created.ID, livewall.ContentPolicyFeaturedOnly)
	require.NoError(t, err)
	require.Equal(t, livewall.ContentPolicyFeaturedOnly, policy.ContentPolicy)
	require.EqualValues(t, next.Revision+1, policy.Revision)

	cadence, err := repo.UpdateCTAEveryMedia(ctx, eventID, created.ID, 12)
	require.NoError(t, err)
	require.Equal(t, 12, cadence.CTAEveryMedia)

	ended, err := repo.End(ctx, eventID, created.ID, livewall.StatusEnded, time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, livewall.StatusEnded, ended.Status)
	require.NotNil(t, ended.EndedAt)

	// An ended session no longer accepts commands.
	_, err = repo.Command(ctx, eventID, created.ID, livewall.CommandPlay)
	require.ErrorIs(t, err, livewall.ErrNotFound)
}
