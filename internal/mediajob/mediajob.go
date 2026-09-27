// Package mediajob runs deferred work on media records.
//
// It exists because the work it carries must survive the process that asked
// for it. Thumbnail rendering used to run as `go s.createThumbnail(...)`
// straight from the upload confirmation: a deploy or a crash lost the job with
// no record that it was ever owed, there was no retry, and a burst of guests
// finishing their uploads at the same moment started one full image decode per
// goroutine with nothing bounding how many ran at once.
//
// The claim protocol is the one internal/exportjob already uses: rows are
// taken with FOR UPDATE SKIP LOCKED, so any number of replicas may run a
// worker without coordinating.
package mediajob

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNoQueuedJob reports an empty queue. It is an expected result, not a
// failure: the worker idles on it.
var ErrNoQueuedJob = errors.New("mediajob: no queued job")

type Kind string

// KindThumbnail renders the gallery-sized variant of an uploaded image.
const KindThumbnail Kind = "thumbnail"

type Status string

const (
	StatusQueued     Status = "queued"
	StatusProcessing Status = "processing"
	StatusDone       Status = "done"
	StatusFailed     Status = "failed"
)

type Job struct {
	ID        uuid.UUID
	MediaID   uuid.UUID
	Kind      Kind
	Status    Status
	Attempts  int
	LastError *string
	RunAfter  time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (Job) TableName() string { return "media_jobs" }

// Repository owns the claim and completion transitions. Implementations decide
// how a row is locked; the worker only sees whole jobs.
type Repository interface {
	// ClaimBatch moves up to limit due jobs to processing and returns them.
	// It returns ErrNoQueuedJob when nothing is due.
	ClaimBatch(ctx context.Context, limit int, now time.Time) ([]Job, error)
	MarkDone(ctx context.Context, id uuid.UUID) error
	// Reschedule returns a job to the queue for a later attempt.
	Reschedule(ctx context.Context, id uuid.UUID, runAfter time.Time, cause string) error
	// MarkFailed retires a job that has exhausted its attempts.
	MarkFailed(ctx context.Context, id uuid.UUID, cause string) error
}

// Runner performs the work a job names. media.Service satisfies it, which
// keeps this package free of any knowledge about images or storage.
type Runner interface {
	GenerateThumbnail(ctx context.Context, mediaID uuid.UUID) error
}
