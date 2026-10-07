package entitlement

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/google/uuid"
)

// GrantActivator is the single entry point for changing an event's plan. An
// admin tool, a migration or a future payment integration all go through it;
// none of them touch grants directly.
type GrantActivator interface {
	Activate(ctx context.Context, input ActivateGrantInput) (Grant, error)
}

type ActivateGrantInput struct {
	EventID        uuid.UUID
	TargetPlanCode catalog.PlanCode
	Source         Source
	// SourceRef is an opaque, caller-chosen reference that makes activation
	// idempotent: the same reference never grants twice.
	SourceRef   string
	ActorID     *uuid.UUID
	Reason      string
	ActivatedAt time.Time
}

type GrantService struct {
	repo     Repository
	catalog  PlanCatalog
	now      func() time.Time
	notifier Notifier
}

// PlanChange is announced after a grant activates, so open host screens can
// refresh without a reload.
type PlanChange struct {
	EventID  uuid.UUID        `json:"event_id"`
	PlanCode catalog.PlanCode `json:"plan_code"`
	GrantID  uuid.UUID        `json:"grant_id"`
}

// Notifier receives plan changes. Implementations must return promptly.
type Notifier interface {
	PlanChanged(ctx context.Context, change PlanChange)
}

// UseNotifier announces every successful activation.
func (s *GrantService) UseNotifier(n Notifier) *GrantService {
	s.notifier = n
	return s
}

var _ GrantActivator = (*GrantService)(nil)

func NewGrantService(repo Repository, plans PlanCatalog) *GrantService {
	return &GrantService{repo: repo, catalog: plans, now: time.Now}
}

func (s *GrantService) Activate(ctx context.Context, in ActivateGrantInput) (Grant, error) {
	if !in.Source.Valid() || in.Source == SourceSystem {
		// System grants are only issued at event creation, by the Provisioner.
		return Grant{}, ErrInvalidSource
	}
	ref := strings.TrimSpace(in.SourceRef)
	if ref == "" {
		return Grant{}, ErrSourceReferenceRequired
	}
	version, err := s.catalog.ActiveVersion(ctx, in.TargetPlanCode)
	if errors.Is(err, catalog.ErrNotFound) {
		return Grant{}, ErrUnknownPlan
	}
	if err != nil {
		return Grant{}, err
	}
	activatedAt := in.ActivatedAt
	if activatedAt.IsZero() {
		activatedAt = s.now()
	}
	activatedAt = activatedAt.UTC()

	req := ActivationRequest{EventID: in.EventID, Source: in.Source, SourceRef: ref, ActorID: in.ActorID, Reason: in.Reason}
	grant, err := s.repo.Activate(ctx, req, func(facts EventFacts, current *ActiveGrant) (Grant, catalog.PlanCode, error) {
		var from *catalog.PlanCode
		if current != nil {
			from = &current.PlanCode
		}
		if err := CheckTransition(from, version.Code); err != nil {
			return Grant{}, "", err
		}
		upload, retention := Expiry(facts.EventDate, activatedAt, version.Entitlements.Limits.RetentionDays)
		if current != nil {
			// An upgrade never shortens what the host already had.
			upload = later(upload, current.UploadExpiresAt)
			retention = later(retention, current.RetentionExpiresAt)
		}
		return Grant{
			ID:                  uuid.New(),
			EventID:             in.EventID,
			PlanVersionID:       version.ID,
			Status:              StatusActive,
			Source:              in.Source,
			SourceReference:     &ref,
			ActorID:             in.ActorID,
			EntitlementSnapshot: version.Entitlements,
			ActivatedAt:         activatedAt,
			UploadExpiresAt:     upload,
			RetentionExpiresAt:  retention,
			CreatedAt:           activatedAt,
		}, version.Code, nil
	})
	if err != nil {
		return Grant{}, err
	}
	if s.notifier != nil {
		// The grant is committed; an announcement that fails to send only
		// means open screens refresh later.
		s.notifier.PlanChanged(ctx, PlanChange{EventID: grant.EventID, PlanCode: version.Code, GrantID: grant.ID})
	}
	return grant, nil
}

// LegacyBackfillRef is the source reference used when an existing event is
// moved onto Experience before enforcement starts.
func LegacyBackfillRef(eventID uuid.UUID) string { return "legacy-backfill:" + eventID.String() }

type BackfillFailure struct {
	EventID uuid.UUID
	Err     error
}

type BackfillReport struct {
	Upgraded int
	Skipped  int
	Failed   []BackfillFailure
}

// BackfillLegacy moves every event created before cutoff that has no grant, or
// only a Free one, onto Experience, so turning enforcement on never lowers an
// existing event's quota. It is safe to re-run: each event's reference can be
// granted once, and already-upgraded events are no longer candidates.
//
// One failing event is recorded and skipped rather than aborting the run.
func (s *GrantService) BackfillLegacy(ctx context.Context, cutoff time.Time, dryRun bool, batch int) (BackfillReport, error) {
	if batch < 1 {
		batch = 200
	}
	var report BackfillReport
	after := uuid.Nil
	for {
		ids, err := s.repo.BackfillCandidates(ctx, cutoff, after, batch)
		if err != nil {
			return report, err
		}
		if len(ids) == 0 {
			return report, nil
		}
		for _, id := range ids {
			if dryRun {
				report.Upgraded++
				continue
			}
			_, err := s.Activate(ctx, ActivateGrantInput{
				EventID:        id,
				TargetPlanCode: catalog.PlanExperience,
				Source:         SourceMigration,
				SourceRef:      LegacyBackfillRef(id),
				Reason:         "existing event moved to Experience before plan enforcement",
			})
			switch {
			case err == nil:
				report.Upgraded++
			case errors.Is(err, ErrDuplicateSourceReference), errors.Is(err, ErrEventNotFound):
				// Already granted by an earlier run, or removed since it was listed.
				report.Skipped++
			default:
				report.Failed = append(report.Failed, BackfillFailure{EventID: id, Err: err})
			}
		}
		after = ids[len(ids)-1]
	}
}

// Revoke undoes the event's latest plan activation, for a grant issued by
// mistake. The plan it replaced becomes active again with its own limits and
// expiry; the event's first grant cannot be revoked, so an event never ends
// up without a plan. Media already uploaded is kept.
func (s *GrantService) Revoke(ctx context.Context, eventID uuid.UUID, reason string) (revoked, restored ActiveGrant, err error) {
	if strings.TrimSpace(reason) == "" {
		return ActiveGrant{}, ActiveGrant{}, fmt.Errorf("entitlement: a reason is required to revoke a grant")
	}
	revoked, restored, err = s.repo.Revoke(ctx, eventID, strings.TrimSpace(reason), s.now().UTC())
	if err != nil {
		return ActiveGrant{}, ActiveGrant{}, err
	}
	if s.notifier != nil {
		s.notifier.PlanChanged(ctx, PlanChange{EventID: eventID, PlanCode: restored.PlanCode, GrantID: restored.ID})
	}
	return revoked, restored, nil
}
