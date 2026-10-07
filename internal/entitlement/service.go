package entitlement

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/google/uuid"
)

// PlanCatalog is the slice of the catalog the entitlement domain reads.
type PlanCatalog interface {
	ActiveVersion(ctx context.Context, code catalog.PlanCode) (catalog.Version, error)
	LowestPlanWith(ctx context.Context, f catalog.Feature) (catalog.PlanCode, error)
}

// FeatureAuthorizer is the port feature services depend on to enforce plans.
type FeatureAuthorizer interface {
	Resolve(ctx context.Context, eventID uuid.UUID) (EventEntitlement, error)
	Require(ctx context.Context, eventID uuid.UUID, feature catalog.Feature) error
}

// Service answers what an event is entitled to and what it has used. It never
// changes grants; GrantService does.
//
// Enforcement is a rollout switch. While it is off the service runs in shadow
// mode: every check is still evaluated and a would-be denial is logged, but
// the caller is allowed through. That lets the rollout compare decisions with
// real traffic before any host is blocked.
type Service struct {
	repo    Repository
	catalog PlanCatalog
	now     func() time.Time
	enforce bool
	log     *slog.Logger
}

var _ FeatureAuthorizer = (*Service)(nil)

type Option func(*Service)

// WithEnforcement makes denials real. Without it the service is in shadow mode.
func WithEnforcement(enforce bool) Option { return func(s *Service) { s.enforce = enforce } }

func WithLogger(logger *slog.Logger) Option { return func(s *Service) { s.log = logger } }

func NewService(repo Repository, plans PlanCatalog, opts ...Option) *Service {
	s := &Service{repo: repo, catalog: plans, now: time.Now, log: slog.Default()}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Service) Now() time.Time { return s.now() }

// Enforcing reports whether denials are applied rather than only logged.
func (s *Service) Enforcing() bool { return s.enforce }

func (s *Service) Resolve(ctx context.Context, eventID uuid.UUID) (EventEntitlement, error) {
	g, err := s.repo.ActiveGrant(ctx, eventID)
	if err != nil {
		return EventEntitlement{}, err
	}
	return fromActiveGrant(g), nil
}

// Require returns a *FeatureError when the event's plan lacks feature. In
// shadow mode the denial is logged and nil is returned.
func (s *Service) Require(ctx context.Context, eventID uuid.UUID, feature catalog.Feature) error {
	ent, err := s.Resolve(ctx, eventID)
	if errors.Is(err, ErrNoActiveGrant) {
		// Every event is created with a grant, so this is a data problem. It
		// is reported loudly and treated as "not entitled" rather than as a
		// server error the host cannot act on.
		s.log.ErrorContext(ctx, "plan.grant_missing", "event_id", eventID, "feature", feature)
		ent = EventEntitlement{EventID: eventID}
	} else if err != nil {
		return err
	}
	return s.Check(ctx, ent, feature)
}

// Check is Require for an entitlement the caller already resolved, so one
// request that gates several features reads the grant once.
func (s *Service) Check(ctx context.Context, ent EventEntitlement, feature catalog.Feature) error {
	if ent.Has(feature) {
		return nil
	}
	required, err := s.catalog.LowestPlanWith(ctx, feature)
	if err != nil {
		return err
	}
	s.log.InfoContext(ctx, "plan.entitlement_denied",
		"event_id", ent.EventID, "plan", ent.PlanCode, "plan_version", ent.PlanVersion,
		"feature", feature, "required_plan", required, "shadow", !s.enforce)
	if !s.enforce {
		return nil
	}
	return &FeatureError{Feature: feature, RequiredPlan: required}
}

// UploadsClosed reports whether a new upload must be refused because the
// event's upload window has ended. Guest uploads fail open: when the plan
// cannot be read the upload is allowed and the problem logged.
func (s *Service) UploadsClosed(ctx context.Context, eventID uuid.UUID) bool {
	ent, err := s.Resolve(ctx, eventID)
	if err != nil {
		s.log.ErrorContext(ctx, "plan.resolve_failed", "event_id", eventID, "error", err)
		return false
	}
	if !ent.UploadClosed(s.now()) {
		return false
	}
	s.log.InfoContext(ctx, "plan.upload_window_closed",
		"event_id", eventID, "plan", ent.PlanCode, "upload_expires_at", ent.UploadExpiresAt, "shadow", !s.enforce)
	return s.enforce
}

func (s *Service) Usage(ctx context.Context, eventID uuid.UUID) (Usage, error) {
	usage, err := s.repo.Usage(ctx, []uuid.UUID{eventID})
	if err != nil {
		return Usage{}, err
	}
	return usage[eventID], nil
}

type HostEventPlanSummary struct {
	HostEventPlan
	Usage Usage
}

type HostEventPlansPage struct {
	Items      []HostEventPlanSummary
	Page       int
	PerPage    int
	Total      int64
	TotalPages int
}

// HostEventPlans lists a host's events with their plan and usage in two
// queries, however many events are on the page.
func (s *Service) HostEventPlans(ctx context.Context, hostID uuid.UUID, page, perPage int) (HostEventPlansPage, error) {
	if page < 1 {
		page = 1
	}
	if perPage < 1 {
		perPage = 20
	} else if perPage > 100 {
		perPage = 100
	}
	rows, total, err := s.repo.HostEventPlans(ctx, hostID, page, perPage)
	if err != nil {
		return HostEventPlansPage{}, err
	}
	ids := make([]uuid.UUID, len(rows))
	for i, row := range rows {
		ids[i] = row.EventID
	}
	usage, err := s.repo.Usage(ctx, ids)
	if err != nil {
		return HostEventPlansPage{}, err
	}
	items := make([]HostEventPlanSummary, len(rows))
	for i, row := range rows {
		items[i] = HostEventPlanSummary{HostEventPlan: row, Usage: usage[row.EventID]}
	}
	totalPages := 1
	if total > 0 {
		totalPages = int(math.Ceil(float64(total) / float64(perPage)))
	}
	return HostEventPlansPage{Items: items, Page: page, PerPage: perPage, Total: total, TotalPages: totalPages}, nil
}

// AuditTrail lists an event's plan changes, newest first.
func (s *Service) AuditTrail(ctx context.Context, eventID uuid.UUID, limit int) ([]AuditEntry, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	return s.repo.AuditTrail(ctx, eventID, limit)
}

// Reconcile checks the plan invariants across every event.
func (s *Service) Reconcile(ctx context.Context, grace, expiringSoon time.Duration, sample int) (ReconcileReport, error) {
	if sample < 1 {
		sample = 10
	}
	return s.repo.Reconcile(ctx, ReconcileOptions{Now: s.now(), Grace: grace, ExpiringSoon: expiringSoon, Sample: sample})
}
