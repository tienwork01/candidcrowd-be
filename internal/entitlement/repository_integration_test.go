//go:build integration

// These tests run the grant SQL against a real, migrated PostgreSQL: the
// one-active-grant index, row locking and quota copy only mean anything there.
//
//	make test-integration-db TEST_DATABASE_URL=postgres://...
package entitlement

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/database"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type harness struct {
	db       *gorm.DB
	plans    *catalog.Service
	repo     Repository
	service  *Service
	grants   *GrantService
	events   *event.Service
	hostID   uuid.UUID
	eventIDs []uuid.UUID
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, sqlDB, err := database.Open(url, 20, 5, time.Minute)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	h := &harness{db: db, hostID: uuid.New()}
	h.plans = catalog.NewService(catalog.NewGormRepository(db))
	h.repo = NewGormRepository(db)
	h.service = NewService(h.repo, h.plans)
	h.grants = NewGrantService(h.repo, h.plans)
	h.events = event.NewService(event.NewGormRepository(db, NewProvisioner(h.plans)), 5<<30)

	require.NoError(t, db.Exec(`INSERT INTO users (id, better_auth_user_id, email) VALUES (?, ?, ?)`,
		h.hostID, "test-"+h.hostID.String(), h.hostID.String()+"@example.test").Error)
	t.Cleanup(func() {
		// Audit rows are immutable by design and are left behind.
		for _, id := range h.eventIDs {
			db.Exec(`DELETE FROM media WHERE event_id = ?`, id)
			db.Exec(`DELETE FROM guest_sessions WHERE event_id = ?`, id)
			db.Exec(`DELETE FROM event_plan_grants WHERE event_id = ?`, id)
			db.Exec(`DELETE FROM events WHERE id = ?`, id)
		}
		db.Exec(`DELETE FROM users WHERE id = ?`, h.hostID)
	})
	return h
}

func (h *harness) create(t *testing.T, in event.CreateInput) event.Event {
	t.Helper()
	if in.Name == "" {
		in.Name = "plan test"
	}
	evt, err := h.events.Create(context.Background(), h.hostID, in)
	require.NoError(t, err)
	h.eventIDs = append(h.eventIDs, evt.ID)
	return evt
}

// legacyEvent inserts an event the way rows existed before plans: no grant.
func (h *harness) legacyEvent(t *testing.T, createdAt time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, h.db.Exec(`INSERT INTO events (id, host_id, name, slug, created_at) VALUES (?, ?, 'legacy', ?, ?)`,
		id, h.hostID, "evt_"+uuid.NewString()[:12], createdAt).Error)
	h.eventIDs = append(h.eventIDs, id)
	return id
}

func (h *harness) maxBytes(t *testing.T, id uuid.UUID) int64 {
	var v int64
	require.NoError(t, h.db.Raw(`SELECT max_media_bytes FROM events WHERE id = ?`, id).Scan(&v).Error)
	return v
}

func (h *harness) activeCount(t *testing.T, id uuid.UUID) int64 {
	var n int64
	require.NoError(t, h.db.Raw(`SELECT COUNT(*) FROM event_plan_grants WHERE event_id = ? AND status = 'active'`, id).Scan(&n).Error)
	return n
}

func TestIntegrationCatalogSeed(t *testing.T) {
	h := newHarness(t)
	plans, err := h.plans.List(context.Background(), "USD")
	require.NoError(t, err)
	require.Len(t, plans, 3)
	require.Equal(t, catalog.PlanFree, plans[0].Code)
	require.Equal(t, int64(45), *plans[0].Limits.MaxPhotoItems)
	require.Equal(t, int64(3900), plans[1].Price.AmountMinor)
	require.Equal(t, int64(8900), *plans[2].Price.CompareAtMinor)
	require.Equal(t, 730, plans[2].Limits.RetentionDays)
}

func TestIntegrationEventCreationProvisionsFree(t *testing.T) {
	h := newHarness(t)
	date := time.Date(2027, 2, 14, 0, 0, 0, 0, time.UTC)
	requestID := uuid.New()
	evt := h.create(t, event.CreateInput{EventDate: &date, ClientRequestID: &requestID})

	require.Equal(t, int64(500<<20), evt.MaxMediaBytes, "the returned event already carries the Free quota")
	require.Equal(t, int64(500<<20), h.maxBytes(t, evt.ID))

	ent, err := h.service.Resolve(context.Background(), evt.ID)
	require.NoError(t, err)
	require.Equal(t, catalog.PlanFree, ent.PlanCode)
	require.Equal(t, SourceSystem, ent.Source)
	require.Equal(t, time.Date(2027, 2, 22, 0, 0, 0, 0, time.UTC), ent.RetentionExpiresAt.UTC())
	require.Equal(t, int64(45), ent.Limits().MaxPhotoItems)

	again, err := h.events.Create(context.Background(), h.hostID, event.CreateInput{Name: "plan test", ClientRequestID: &requestID})
	require.NoError(t, err, "a retried create is answered, not rejected")
	require.Equal(t, evt.ID, again.ID)
	require.Equal(t, int64(1), h.activeCount(t, evt.ID))

	var audits int64
	require.NoError(t, h.db.Raw(`SELECT COUNT(*) FROM event_plan_audits WHERE event_id = ?`, evt.ID).Scan(&audits).Error)
	require.Equal(t, int64(1), audits)
}

func TestIntegrationActivationUpgradesAndAudits(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	evt := h.create(t, event.CreateInput{})
	ref := "qa-" + uuid.NewString()

	_, err := h.grants.Activate(ctx, ActivateGrantInput{EventID: evt.ID, TargetPlanCode: catalog.PlanSignature, Source: SourceManual, SourceRef: ref, Reason: "qa"})
	require.NoError(t, err)
	require.Equal(t, int64(150<<30), h.maxBytes(t, evt.ID))
	require.Equal(t, int64(1), h.activeCount(t, evt.ID))

	_, err = h.grants.Activate(ctx, ActivateGrantInput{EventID: evt.ID, TargetPlanCode: catalog.PlanExperience, Source: SourceManual, SourceRef: ref + "-2"})
	require.ErrorIs(t, err, ErrInvalidTransition)

	_, err = h.grants.Activate(ctx, ActivateGrantInput{EventID: evt.ID, TargetPlanCode: catalog.PlanSignature, Source: SourceManual, SourceRef: ref})
	require.ErrorIs(t, err, ErrDuplicateSourceReference)

	var previous string
	require.NoError(t, h.db.Raw(`SELECT previous_plan FROM event_plan_audits WHERE event_id = ? AND target_plan = 'signature'`, evt.ID).Scan(&previous).Error)
	require.Equal(t, "free", previous)

	err = h.db.Exec(`UPDATE event_plan_audits SET reason = 'edited' WHERE event_id = ?`, evt.ID).Error
	require.Error(t, err, "audit rows cannot be rewritten")
}

func TestIntegrationConcurrentActivationsLeaveOneActiveGrant(t *testing.T) {
	h := newHarness(t)
	evt := h.create(t, event.CreateInput{})

	var wg sync.WaitGroup
	results := make([]error, 6)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			plan := catalog.PlanExperience
			if i%2 == 1 {
				plan = catalog.PlanSignature
			}
			_, results[i] = h.grants.Activate(context.Background(), ActivateGrantInput{
				EventID: evt.ID, TargetPlanCode: plan, Source: SourceManual, SourceRef: uuid.NewString(),
			})
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		} else {
			require.ErrorIs(t, err, ErrInvalidTransition)
		}
	}
	require.GreaterOrEqual(t, succeeded, 1)
	require.LessOrEqual(t, succeeded, 2, "at most free → experience → signature")
	require.Equal(t, int64(1), h.activeCount(t, evt.ID))
}

// BackfillLegacy acts on every event in the database, including other
// packages' fixtures, which is why `make test-integration-db` runs packages
// one at a time.
func TestIntegrationBackfillLegacy(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cutoff := time.Now().UTC().Truncate(time.Second)

	legacy := h.legacyEvent(t, cutoff.Add(-30*24*time.Hour))
	shadow := h.create(t, event.CreateInput{}) // Free, created before the cutoff below
	later := cutoff.Add(time.Hour)

	report, err := h.grants.BackfillLegacy(ctx, later, false, 50)
	require.NoError(t, err)
	require.Empty(t, report.Failed)
	require.GreaterOrEqual(t, report.Upgraded, 2)

	for _, id := range []uuid.UUID{legacy, shadow.ID} {
		ent, err := h.service.Resolve(ctx, id)
		require.NoError(t, err)
		require.Equal(t, catalog.PlanExperience, ent.PlanCode)
		require.Equal(t, SourceMigration, ent.Source)
		require.True(t, ent.RetentionExpiresAt.After(time.Now().AddDate(0, 11, 0)), "a past event still gets a full year")
		require.Equal(t, int64(50<<30), h.maxBytes(t, id))
		require.Equal(t, int64(1), h.activeCount(t, id))
	}

	again, err := h.grants.BackfillLegacy(ctx, later, false, 50)
	require.NoError(t, err)
	require.Empty(t, again.Failed)
	for _, id := range []uuid.UUID{legacy, shadow.ID} {
		require.Equal(t, int64(1), h.activeCount(t, id))
	}
}

func TestIntegrationUsageAndHostOverview(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	evt := h.create(t, event.CreateInput{Name: "with media"})
	require.NoError(t, h.db.Exec(`UPDATE events SET trial_ended_at = now() WHERE id = ?`, evt.ID).Error)
	empty := h.create(t, event.CreateInput{Name: "empty"})

	// Usage reads the lifetime counters reservations maintain (exercised in
	// platform/database); in-flight reservations are not usage yet.
	require.NoError(t, h.db.Exec(`UPDATE events SET used_media_bytes = 1234,
		uploaded_media_items = 3, uploaded_photo_items = 2, uploaded_video_items = 1,
		reserved_media_items = 5, reserved_photo_items = 5 WHERE id = ?`, evt.ID).Error)

	usage, err := h.service.Usage(ctx, evt.ID)
	require.NoError(t, err)
	require.Equal(t, Usage{MediaItems: 3, PhotoItems: 2, VideoItems: 1, MediaBytes: 1234}, usage)

	page, err := h.service.HostEventPlans(ctx, h.hostID, 1, 10)
	require.NoError(t, err)
	require.Equal(t, int64(2), page.Total)
	byID := map[uuid.UUID]HostEventPlanSummary{}
	for _, item := range page.Items {
		byID[item.EventID] = item
	}
	require.Equal(t, catalog.PlanFree, *byID[evt.ID].PlanCode)
	require.Equal(t, int64(50), *byID[evt.ID].MediaItemsLimit)
	require.Equal(t, int64(3), byID[evt.ID].Usage.MediaItems)
	require.Zero(t, byID[empty.ID].Usage.MediaItems)
}

func TestIntegrationRevokeRestoresThePreviousGrant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	evt := h.create(t, event.CreateInput{})

	_, _, err := h.grants.Revoke(ctx, evt.ID, "nothing to undo")
	require.ErrorIs(t, err, ErrNothingToRestore)

	_, err = h.grants.Activate(ctx, ActivateGrantInput{EventID: evt.ID, TargetPlanCode: catalog.PlanSignature, Source: SourceManual, SourceRef: "rv-" + uuid.NewString()})
	require.NoError(t, err)
	require.Equal(t, int64(150<<30), h.maxBytes(t, evt.ID))

	revoked, restored, err := h.grants.Revoke(ctx, evt.ID, "granted by mistake")
	require.NoError(t, err)
	require.Equal(t, catalog.PlanSignature, revoked.PlanCode)
	require.Equal(t, catalog.PlanFree, restored.PlanCode)
	require.Equal(t, int64(1), h.activeCount(t, evt.ID))
	require.Equal(t, int64(500<<20), h.maxBytes(t, evt.ID), "the restored plan limits are back on the event")

	var items int64
	require.NoError(t, h.db.Raw(`SELECT max_photo_items FROM events WHERE id = ?`, evt.ID).Scan(&items).Error)
	require.EqualValues(t, 45, items)

	trail, err := h.service.AuditTrail(ctx, evt.ID, 10)
	require.NoError(t, err)
	require.Equal(t, "free", trail[0].TargetPlan)
	require.Equal(t, "signature", *trail[0].PreviousPlan)
	require.Contains(t, *trail[0].Reason, "granted by mistake")
}

func TestIntegrationReconcileFindsDrift(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	evt := h.create(t, event.CreateInput{})
	orphan := h.legacyEvent(t, time.Now())

	require.NoError(t, h.db.Exec(`UPDATE events SET uploaded_media_items = 7, max_photo_items = 99 WHERE id = ?`, evt.ID).Error)

	report, err := h.service.Reconcile(ctx, 30*24*time.Hour, 7*24*time.Hour, 100000)
	require.NoError(t, err)
	require.Contains(t, report.EventsWithoutGrant.Sample, orphan)
	require.Contains(t, report.LimitMismatch.Sample, evt.ID)
	require.Contains(t, report.CounterDrift.Sample, evt.ID)
}
