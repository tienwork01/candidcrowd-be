// Package retention permanently removes an event's media once its plan's
// storage period, plus the deletion grace period promised in the Terms, has
// passed.
//
// Removal is irreversible, so the service is conservative by construction:
// it is off unless explicitly enforced (and only reports what it would do),
// it acts one event at a time, and an event is only marked purged after every
// copy is confirmed gone, so a partial failure is simply retried next run.
package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// DefaultGrace is the deletion grace period the Terms of Service promise
// after a storage period ends.
const DefaultGrace = 30 * 24 * time.Hour

// Candidate is an event whose storage period and grace period have ended.
type Candidate struct {
	EventID            uuid.UUID
	PlanCode           string
	RetentionExpiresAt time.Time
}

// Replica is a long-term archive copy of one media item.
type Replica struct {
	MediaID  uuid.UUID
	Location string
}

// Purged is what MarkPurged changed for one event.
type Purged struct {
	Media   int64
	Exports int64
}

type Repository interface {
	// Candidates lists unpurged events whose active grant's storage period
	// ended at or before cutoff, oldest first.
	Candidates(ctx context.Context, cutoff time.Time, limit int) ([]Candidate, error)
	ArchivedReplicas(ctx context.Context, eventID uuid.UUID) ([]Replica, error)
	MarkReplicaDeleted(ctx context.Context, replica Replica, at time.Time) error
	// MarkPurged closes the event to uploads and records that its media no
	// longer exists, in one transaction.
	MarkPurged(ctx context.Context, eventID uuid.UUID, at time.Time) (Purged, error)
}

// ObjectStore removes every hot-storage object under a prefix.
type ObjectStore interface {
	DeletePrefix(ctx context.Context, prefix string) (int, error)
}

// Archive removes a long-term archive copy. Deleting a copy that is already
// gone must succeed.
type Archive interface {
	Delete(ctx context.Context, location string) error
}

// ErrArchiveUnavailable stops a purge that would leave archive copies behind
// because no archive client is configured.
var ErrArchiveUnavailable = errors.New("retention: event has archived copies but no archive client is configured")

type Service struct {
	repo    Repository
	objects ObjectStore
	archive Archive
	grace   time.Duration
	enforce bool
	now     func() time.Time
	log     *slog.Logger
}

type Option func(*Service)

// WithEnforcement makes Run delete. Without it Run only reports.
func WithEnforcement(enforce bool) Option { return func(s *Service) { s.enforce = enforce } }

// WithArchive lets the purge remove long-term archive copies too.
func WithArchive(archive Archive) Option { return func(s *Service) { s.archive = archive } }

func WithGrace(grace time.Duration) Option {
	return func(s *Service) {
		if grace > 0 {
			s.grace = grace
		}
	}
}

func WithLogger(log *slog.Logger) Option { return func(s *Service) { s.log = log } }

func NewService(repo Repository, objects ObjectStore, opts ...Option) *Service {
	s := &Service{repo: repo, objects: objects, grace: DefaultGrace, now: time.Now, log: slog.Default()}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Service) Enforcing() bool { return s.enforce }

type Failure struct {
	EventID uuid.UUID
	Err     error
}

type Report struct {
	// Due counts events past their storage and grace period.
	Due    int
	Purged int
	Failed []Failure
	// Candidates are the due events, for a dry run to print.
	Candidates []Candidate
}

// Run handles up to limit due events. dryRun forces a report even when
// enforcement is on, for operators previewing a run.
func (s *Service) Run(ctx context.Context, limit int, dryRun bool) (Report, error) {
	if limit < 1 {
		limit = 50
	}
	cutoff := s.now().Add(-s.grace)
	candidates, err := s.repo.Candidates(ctx, cutoff, limit)
	if err != nil {
		return Report{}, err
	}
	report := Report{Due: len(candidates), Candidates: candidates}
	for _, c := range candidates {
		if dryRun || !s.enforce {
			s.log.InfoContext(ctx, "plan.retention_due",
				"event_id", c.EventID, "plan", c.PlanCode, "retention_expires_at", c.RetentionExpiresAt, "shadow", true)
			continue
		}
		purged, err := s.purge(ctx, c)
		if err != nil {
			report.Failed = append(report.Failed, Failure{EventID: c.EventID, Err: err})
			s.log.ErrorContext(ctx, "plan.retention_purge_failed", "event_id", c.EventID, "error", err)
			continue
		}
		report.Purged++
		s.log.InfoContext(ctx, "plan.retention_purged",
			"event_id", c.EventID, "plan", c.PlanCode, "retention_expires_at", c.RetentionExpiresAt,
			"media", purged.Media, "exports", purged.Exports)
	}
	return report, nil
}

func (s *Service) purge(ctx context.Context, c Candidate) (Purged, error) {
	if c.EventID == uuid.Nil {
		return Purged{}, fmt.Errorf("retention: refusing to purge without an event id")
	}
	replicas, err := s.repo.ArchivedReplicas(ctx, c.EventID)
	if err != nil {
		return Purged{}, err
	}
	if len(replicas) > 0 && s.archive == nil {
		return Purged{}, ErrArchiveUnavailable
	}
	for _, replica := range replicas {
		if err := s.archive.Delete(ctx, replica.Location); err != nil {
			return Purged{}, fmt.Errorf("delete archive copy of %s: %w", replica.MediaID, err)
		}
		if err := s.repo.MarkReplicaDeleted(ctx, replica, s.now()); err != nil {
			return Purged{}, err
		}
	}
	// Originals, thumbnails, exports and QR assets all live under the event's
	// prefix, so one scoped sweep removes every hot copy.
	if _, err := s.objects.DeletePrefix(ctx, EventPrefix(c.EventID)); err != nil {
		return Purged{}, fmt.Errorf("delete stored objects: %w", err)
	}
	return s.repo.MarkPurged(ctx, c.EventID, s.now())
}

// EventPrefix is the storage prefix every object of one event lives under.
func EventPrefix(eventID uuid.UUID) string { return "events/" + eventID.String() + "/" }
