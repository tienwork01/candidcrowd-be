package entitlement

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var testPlans = map[catalog.PlanCode]catalog.Version{
	catalog.PlanFree: {ID: uuid.New(), Code: catalog.PlanFree, Version: 1, DisplayName: "Free",
		Entitlements: catalog.Entitlements{Limits: catalog.Limits{MaxMediaItems: 33, MaxPhotoItems: 30, MaxVideoItems: 3, MaxMediaBytes: 500 << 20, RetentionDays: 7}}},
	catalog.PlanExperience: {ID: uuid.New(), Code: catalog.PlanExperience, Version: 1, DisplayName: "Experience",
		Entitlements: catalog.Entitlements{FairUse: true, Features: []catalog.Feature{catalog.FeatureZIPExport},
			Limits: catalog.Limits{MaxMediaItems: 2000, MaxMediaBytes: 50 << 30, RetentionDays: 365}}},
	catalog.PlanSignature: {ID: uuid.New(), Code: catalog.PlanSignature, Version: 1, DisplayName: "Signature",
		Entitlements: catalog.Entitlements{FairUse: true, Features: []catalog.Feature{catalog.FeatureZIPExport, catalog.FeatureThroughTheMoment},
			Limits: catalog.Limits{MaxMediaItems: 5000, MaxMediaBytes: 150 << 30, RetentionDays: 730}}},
}

type fakeCatalog struct{}

func (fakeCatalog) ActiveVersion(_ context.Context, code catalog.PlanCode) (catalog.Version, error) {
	v, ok := testPlans[code]
	if !ok {
		return catalog.Version{}, catalog.ErrNotFound
	}
	return v, nil
}

func (fakeCatalog) LowestPlanWith(_ context.Context, f catalog.Feature) (catalog.PlanCode, error) {
	for _, code := range []catalog.PlanCode{catalog.PlanFree, catalog.PlanExperience, catalog.PlanSignature} {
		if testPlans[code].Entitlements.Has(f) {
			return code, nil
		}
	}
	return "", nil
}

type memoryEvent struct {
	date      *time.Time
	createdAt time.Time
	maxBytes  int64
}

type memoryRepository struct {
	mu     sync.Mutex
	events map[uuid.UUID]*memoryEvent
	grants []ActiveGrant
	audits []audit
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{events: map[uuid.UUID]*memoryEvent{}}
}

func (r *memoryRepository) addEvent(date *time.Time, createdAt time.Time) uuid.UUID {
	id := uuid.New()
	r.events[id] = &memoryEvent{date: date, createdAt: createdAt}
	return id
}

func (r *memoryRepository) active(eventID uuid.UUID) *ActiveGrant {
	for i := range r.grants {
		if r.grants[i].EventID == eventID && r.grants[i].Status == StatusActive {
			return &r.grants[i]
		}
	}
	return nil
}

func (r *memoryRepository) ActiveGrant(_ context.Context, eventID uuid.UUID) (ActiveGrant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g := r.active(eventID); g != nil {
		return *g, nil
	}
	return ActiveGrant{}, ErrNoActiveGrant
}

func (r *memoryRepository) Activate(_ context.Context, req ActivationRequest, build BuildFunc) (Grant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	evt, ok := r.events[req.EventID]
	if !ok {
		return Grant{}, ErrEventNotFound
	}
	for _, g := range r.grants {
		if g.Source == req.Source && g.SourceReference != nil && *g.SourceReference == req.SourceRef {
			return Grant{}, ErrDuplicateSourceReference
		}
	}
	current := r.active(req.EventID)
	var snapshot *ActiveGrant
	if current != nil {
		c := *current
		snapshot = &c
	}
	grant, target, err := build(EventFacts{EventDate: evt.date}, snapshot)
	if err != nil {
		return Grant{}, err
	}
	if current != nil {
		current.Status = StatusSuperseded
	}
	v := testPlans[target]
	r.grants = append(r.grants, ActiveGrant{Grant: grant, PlanCode: target, PlanName: v.DisplayName, PlanVersion: v.Version})
	evt.maxBytes = grant.EntitlementSnapshot.Limits.MaxMediaBytes
	r.audits = append(r.audits, audit{EventID: grant.EventID, GrantID: grant.ID, TargetPlan: string(target), Source: grant.Source})
	return grant, nil
}

func (r *memoryRepository) Usage(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]Usage, error) {
	out := map[uuid.UUID]Usage{}
	for _, id := range ids {
		out[id] = Usage{MediaItems: 1}
	}
	return out, nil
}

func (r *memoryRepository) HostEventPlans(context.Context, uuid.UUID, int, int) ([]HostEventPlan, int64, error) {
	return nil, 0, nil
}

func (r *memoryRepository) AuditTrail(_ context.Context, eventID uuid.UUID, limit int) ([]AuditEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []AuditEntry
	for i := len(r.audits) - 1; i >= 0 && len(out) < limit; i-- {
		a := r.audits[i]
		if a.EventID == eventID {
			out = append(out, AuditEntry{GrantID: a.GrantID, TargetPlan: a.TargetPlan, Source: a.Source})
		}
	}
	return out, nil
}

func (r *memoryRepository) Revoke(_ context.Context, eventID uuid.UUID, _ string, _ time.Time) (ActiveGrant, ActiveGrant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.active(eventID)
	if current == nil {
		return ActiveGrant{}, ActiveGrant{}, ErrNoActiveGrant
	}
	for i := len(r.grants) - 1; i >= 0; i-- {
		if r.grants[i].EventID == eventID && r.grants[i].Status == StatusSuperseded {
			current.Status = StatusRevoked
			r.grants[i].Status = StatusActive
			r.events[eventID].maxBytes = r.grants[i].EntitlementSnapshot.Limits.MaxMediaBytes
			return *current, r.grants[i], nil
		}
	}
	return ActiveGrant{}, ActiveGrant{}, ErrNothingToRestore
}

func (r *memoryRepository) Reconcile(context.Context, ReconcileOptions) (ReconcileReport, error) {
	return ReconcileReport{}, nil
}

func (r *memoryRepository) BackfillCandidates(_ context.Context, cutoff time.Time, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []uuid.UUID
	for id, evt := range r.events {
		if !evt.createdAt.Before(cutoff) || id.String() <= after.String() {
			continue
		}
		if g := r.active(id); g == nil || g.PlanCode == catalog.PlanFree {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

func newGrants(repo *memoryRepository, now time.Time) *GrantService {
	s := NewGrantService(repo, fakeCatalog{})
	s.now = func() time.Time { return now }
	return s
}

func manual(eventID uuid.UUID, plan catalog.PlanCode, ref string) ActivateGrantInput {
	return ActivateGrantInput{EventID: eventID, TargetPlanCode: plan, Source: SourceManual, SourceRef: ref, Reason: "qa"}
}

func day(y int, m time.Month, d int) *time.Time {
	t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return &t
}

func TestExpiryAnchorsOnDayAfterEventButNeverBeforeActivation(t *testing.T) {
	activated := time.Date(2026, 10, 4, 15, 30, 0, 0, time.UTC)

	upload, retention := Expiry(day(2027, 2, 14), activated, 7)
	require.Equal(t, time.Date(2027, 2, 22, 0, 0, 0, 0, time.UTC), retention, "future event: day after the event plus retention")
	require.Equal(t, retention, upload, "uploads stay open for the whole retention period")

	_, retention = Expiry(nil, activated, 7)
	require.Equal(t, activated.AddDate(0, 0, 7), retention, "no event date: activation plus retention")

	_, retention = Expiry(day(2026, 9, 1), activated, 7)
	require.Equal(t, activated.AddDate(0, 0, 7), retention, "an event in the past never starts already expired")

	local := time.Date(2027, 2, 14, 0, 0, 0, 0, time.FixedZone("ICT", 7*3600))
	_, retention = Expiry(&local, activated, 7)
	require.Equal(t, time.Date(2027, 2, 22, 0, 0, 0, 0, time.UTC), retention, "the calendar date is taken as given, whatever its zone")
}

func TestCheckTransitionAllowsOnlyUpgrades(t *testing.T) {
	code := func(c catalog.PlanCode) *catalog.PlanCode { return &c }
	require.NoError(t, CheckTransition(nil, catalog.PlanExperience))
	require.NoError(t, CheckTransition(code(catalog.PlanFree), catalog.PlanExperience))
	require.NoError(t, CheckTransition(code(catalog.PlanFree), catalog.PlanSignature))
	require.NoError(t, CheckTransition(code(catalog.PlanExperience), catalog.PlanSignature))

	for _, tc := range []struct{ from, to catalog.PlanCode }{
		{catalog.PlanExperience, catalog.PlanFree},
		{catalog.PlanSignature, catalog.PlanExperience},
		{catalog.PlanSignature, catalog.PlanFree},
		{catalog.PlanSignature, catalog.PlanSignature},
	} {
		require.ErrorIs(t, CheckTransition(code(tc.from), tc.to), ErrInvalidTransition, "%s → %s", tc.from, tc.to)
	}
	require.ErrorIs(t, CheckTransition(nil, "gold"), ErrUnknownPlan)
}

func TestActivateUpgradesAndSnapshotsEntitlements(t *testing.T) {
	repo := newMemoryRepository()
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	eventID := repo.addEvent(day(2027, 2, 14), now)
	grants := newGrants(repo, now)

	_, err := grants.Activate(context.Background(), manual(eventID, catalog.PlanExperience, "ticket-1"))
	require.NoError(t, err)
	ent, err := NewService(repo, fakeCatalog{}).Resolve(context.Background(), eventID)
	require.NoError(t, err)
	require.Equal(t, catalog.PlanExperience, ent.PlanCode)
	require.True(t, ent.Has(catalog.FeatureZIPExport))
	require.Equal(t, []catalog.PlanCode{catalog.PlanSignature}, ent.UpgradeOptions())
	require.Equal(t, int64(50<<30), repo.events[eventID].maxBytes, "hot quota follows the grant")

	_, err = grants.Activate(context.Background(), manual(eventID, catalog.PlanSignature, "ticket-2"))
	require.NoError(t, err)
	active := 0
	for _, g := range repo.grants {
		if g.Status == StatusActive {
			active++
		}
	}
	require.Equal(t, 1, active, "the previous grant is superseded")
	require.Len(t, repo.audits, 2)
}

func TestActivateRejectsDowngradeDuplicateAndBadInput(t *testing.T) {
	repo := newMemoryRepository()
	now := time.Now()
	eventID := repo.addEvent(nil, now)
	grants := newGrants(repo, now)
	ctx := context.Background()

	_, err := grants.Activate(ctx, manual(eventID, catalog.PlanSignature, "ticket-1"))
	require.NoError(t, err)

	_, err = grants.Activate(ctx, manual(eventID, catalog.PlanExperience, "ticket-2"))
	require.ErrorIs(t, err, ErrInvalidTransition)

	other := repo.addEvent(nil, now)
	_, err = grants.Activate(ctx, manual(other, catalog.PlanExperience, "ticket-1"))
	require.ErrorIs(t, err, ErrDuplicateSourceReference, "one reference grants once, across events")

	_, err = grants.Activate(ctx, manual(other, catalog.PlanExperience, "  "))
	require.ErrorIs(t, err, ErrSourceReferenceRequired)

	in := manual(other, catalog.PlanExperience, "ticket-3")
	in.Source = SourceSystem
	_, err = grants.Activate(ctx, in)
	require.ErrorIs(t, err, ErrInvalidSource, "system grants only come from event creation")

	_, err = grants.Activate(ctx, manual(other, "gold", "ticket-4"))
	require.ErrorIs(t, err, ErrUnknownPlan)

	_, err = grants.Activate(ctx, manual(uuid.New(), catalog.PlanExperience, "ticket-5"))
	require.ErrorIs(t, err, ErrEventNotFound)
}

func TestUpgradeNeverShortensExpiry(t *testing.T) {
	repo := newMemoryRepository()
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	eventID := repo.addEvent(nil, created)

	first, err := newGrants(repo, created).Activate(context.Background(), manual(eventID, catalog.PlanExperience, "a"))
	require.NoError(t, err)
	require.Equal(t, created.AddDate(0, 0, 365), first.RetentionExpiresAt)

	// A Signature grant computed from an earlier instant would expire before
	// the Experience one; the existing expiry wins.
	in := manual(eventID, catalog.PlanSignature, "b")
	in.ActivatedAt = created.AddDate(-2, 0, 0)
	second, err := newGrants(repo, created).Activate(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, first.RetentionExpiresAt, second.RetentionExpiresAt)
	require.Equal(t, first.UploadExpiresAt, second.UploadExpiresAt)
}

func TestRequireNamesTheCheapestPlanWithTheFeature(t *testing.T) {
	repo := newMemoryRepository()
	eventID := repo.addEvent(nil, time.Now())
	_, err := newGrants(repo, time.Now()).Activate(context.Background(), manual(eventID, catalog.PlanExperience, "x"))
	require.NoError(t, err)
	svc := NewService(repo, fakeCatalog{}, WithEnforcement(true))

	require.NoError(t, svc.Require(context.Background(), eventID, catalog.FeatureZIPExport))

	err = svc.Require(context.Background(), eventID, catalog.FeatureThroughTheMoment)
	var featureErr *FeatureError
	require.True(t, errors.As(err, &featureErr))
	require.Equal(t, catalog.PlanSignature, featureErr.RequiredPlan)
	api := featureErr.APIError()
	require.Equal(t, 403, api.Status)
	require.Equal(t, "feature_not_in_plan", api.Code)
	require.Equal(t, catalog.PlanSignature, api.Details["required_plan"])

	err = svc.Require(context.Background(), uuid.New(), catalog.FeatureZIPExport)
	require.True(t, errors.As(err, &featureErr), "an event without a grant is treated as not entitled")
}

func TestShadowModeLogsButAllows(t *testing.T) {
	repo := newMemoryRepository()
	eventID := repo.addEvent(nil, time.Now())
	_, err := newGrants(repo, time.Now()).Activate(context.Background(), manual(eventID, catalog.PlanExperience, "x"))
	require.NoError(t, err)

	shadow := NewService(repo, fakeCatalog{})
	require.False(t, shadow.Enforcing())
	require.NoError(t, shadow.Require(context.Background(), eventID, catalog.FeatureThroughTheMoment))
	require.NoError(t, shadow.Require(context.Background(), uuid.New(), catalog.FeatureZIPExport))
}

func TestUploadsClosedRespectsModeAndFailsOpen(t *testing.T) {
	repo := newMemoryRepository()
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	eventID := repo.addEvent(nil, created)
	_, err := newGrants(repo, created).Activate(context.Background(), manual(eventID, catalog.PlanExperience, "x"))
	require.NoError(t, err)

	enforcing := NewService(repo, fakeCatalog{}, WithEnforcement(true))
	enforcing.now = func() time.Time { return created.AddDate(0, 6, 0) }
	require.False(t, enforcing.UploadsClosed(context.Background(), eventID))
	enforcing.now = func() time.Time { return created.AddDate(2, 0, 0) }
	require.True(t, enforcing.UploadsClosed(context.Background(), eventID))
	require.False(t, enforcing.UploadsClosed(context.Background(), uuid.New()), "unknown plan: guest uploads fail open")

	shadow := NewService(repo, fakeCatalog{})
	shadow.now = enforcing.now
	require.False(t, shadow.UploadsClosed(context.Background(), eventID))
}

func TestEntitlementExpiryFlags(t *testing.T) {
	expires := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	ent := EventEntitlement{PlanCode: catalog.PlanFree, UploadExpiresAt: expires, RetentionExpiresAt: expires}
	require.False(t, ent.UploadClosed(expires.Add(-time.Second)))
	require.True(t, ent.UploadClosed(expires))
	require.True(t, ent.RetentionExpired(expires.Add(time.Hour)))
	require.Equal(t, []catalog.PlanCode{catalog.PlanExperience, catalog.PlanSignature}, ent.UpgradeOptions())
}

func TestBackfillLegacyUpgradesOnlyOldEventsAndIsRepeatable(t *testing.T) {
	repo := newMemoryRepository()
	cutoff := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	grants := newGrants(repo, cutoff)
	ctx := context.Background()

	noGrant := repo.addEvent(nil, cutoff.Add(-48*time.Hour))
	onFree := repo.addEvent(nil, cutoff.Add(-time.Hour))
	repo.grants = append(repo.grants, ActiveGrant{Grant: Grant{ID: uuid.New(), EventID: onFree, Status: StatusActive, Source: SourceSystem}, PlanCode: catalog.PlanFree})
	onSignature := repo.addEvent(nil, cutoff.Add(-time.Hour))
	_, err := grants.Activate(ctx, manual(onSignature, catalog.PlanSignature, "vip"))
	require.NoError(t, err)
	newer := repo.addEvent(nil, cutoff.Add(time.Minute))

	dry, err := grants.BackfillLegacy(ctx, cutoff, true, 1)
	require.NoError(t, err)
	require.Equal(t, 2, dry.Upgraded)
	require.Nil(t, repo.active(noGrant), "a dry run writes nothing")

	report, err := grants.BackfillLegacy(ctx, cutoff, false, 1)
	require.NoError(t, err)
	require.Equal(t, 2, report.Upgraded)
	require.Empty(t, report.Failed)
	require.Equal(t, catalog.PlanExperience, repo.active(noGrant).PlanCode)
	require.Equal(t, catalog.PlanExperience, repo.active(onFree).PlanCode)
	require.Equal(t, catalog.PlanSignature, repo.active(onSignature).PlanCode, "a higher plan is left alone")
	require.Nil(t, repo.active(newer), "events after the cutoff are not touched")

	again, err := grants.BackfillLegacy(ctx, cutoff, false, 1)
	require.NoError(t, err)
	require.Zero(t, again.Upgraded)
	require.Empty(t, again.Failed)
}

type recordingNotifier struct{ changes []PlanChange }

func (n *recordingNotifier) PlanChanged(_ context.Context, change PlanChange) {
	n.changes = append(n.changes, change)
}

func TestActivateAnnouncesOnlySuccessfulChanges(t *testing.T) {
	repo := newMemoryRepository()
	eventID := repo.addEvent(nil, time.Now())
	notifier := &recordingNotifier{}
	grants := newGrants(repo, time.Now()).UseNotifier(notifier)

	grant, err := grants.Activate(context.Background(), manual(eventID, catalog.PlanSignature, "a"))
	require.NoError(t, err)
	_, err = grants.Activate(context.Background(), manual(eventID, catalog.PlanExperience, "b"))
	require.ErrorIs(t, err, ErrInvalidTransition)

	require.Equal(t, []PlanChange{{EventID: eventID, PlanCode: catalog.PlanSignature, GrantID: grant.ID}}, notifier.changes)

	trail, err := NewService(repo, fakeCatalog{}).AuditTrail(context.Background(), eventID, 10)
	require.NoError(t, err)
	require.Len(t, trail, 1)
	require.Equal(t, "signature", trail[0].TargetPlan)
}

func TestRevokeRestoresThePreviousPlanAndAnnouncesIt(t *testing.T) {
	repo := newMemoryRepository()
	eventID := repo.addEvent(nil, time.Now())
	notifier := &recordingNotifier{}
	grants := newGrants(repo, time.Now()).UseNotifier(notifier)
	ctx := context.Background()

	_, _, err := grants.Revoke(ctx, eventID, "mistake")
	require.ErrorIs(t, err, ErrNoActiveGrant)

	_, err = grants.Activate(ctx, manual(eventID, catalog.PlanExperience, "a"))
	require.NoError(t, err)
	_, _, err = grants.Revoke(ctx, eventID, "mistake")
	require.ErrorIs(t, err, ErrNothingToRestore, "the first grant cannot be revoked")

	_, err = grants.Activate(ctx, manual(eventID, catalog.PlanSignature, "b"))
	require.NoError(t, err)
	_, _, err = grants.Revoke(ctx, eventID, "  ")
	require.Error(t, err, "a reason is required")

	revoked, restored, err := grants.Revoke(ctx, eventID, "granted to the wrong event")
	require.NoError(t, err)
	require.Equal(t, catalog.PlanSignature, revoked.PlanCode)
	require.Equal(t, catalog.PlanExperience, restored.PlanCode)
	require.Equal(t, catalog.PlanExperience, repo.active(eventID).PlanCode)
	require.Equal(t, int64(50<<30), repo.events[eventID].maxBytes)
	require.Equal(t, catalog.PlanExperience, notifier.changes[len(notifier.changes)-1].PlanCode)
}
