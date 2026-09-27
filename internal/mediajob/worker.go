package mediajob

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Config tunes the worker. Zero values fall back to the defaults below.
type Config struct {
	// Concurrency is the hard ceiling on jobs running at once in this process.
	// Thumbnail rendering decodes a whole image into memory, so this number is
	// what stands between an upload burst and an out-of-memory kill.
	Concurrency int
	// Idle is how long to wait after finding an empty queue.
	Idle time.Duration
	// MaxAttempts retires a job that keeps failing rather than letting it
	// occupy a worker forever.
	MaxAttempts int
	// Backoff is the delay before the second attempt. It doubles from there.
	Backoff time.Duration
}

func (c Config) withDefaults() Config {
	if c.Concurrency <= 0 {
		c.Concurrency = 2
	}
	if c.Idle <= 0 {
		c.Idle = 5 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}
	if c.Backoff <= 0 {
		c.Backoff = 30 * time.Second
	}
	return c
}

type Worker struct {
	repo   Repository
	runner Runner
	log    *slog.Logger
	cfg    Config
}

func NewWorker(repo Repository, runner Runner, log *slog.Logger, cfg Config) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{repo: repo, runner: runner, log: log, cfg: cfg.withDefaults()}
}

// Run drains the queue until ctx is cancelled. It claims a full batch at a
// time so a backlog is worked through immediately instead of one job per tick.
func (w *Worker) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	for {
		worked, err := w.drainOnce(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			w.log.Error("media job claim failed", "error", err)
		}
		if worked && ctx.Err() == nil {
			// The queue had work; look again straight away rather than
			// sleeping through a backlog.
			continue
		}
		timer.Reset(w.cfg.Idle)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}

// drainOnce claims one batch and reports whether anything was processed.
func (w *Worker) drainOnce(ctx context.Context) (bool, error) {
	jobs, err := w.repo.ClaimBatch(ctx, w.cfg.Concurrency, time.Now().UTC())
	if errors.Is(err, ErrNoQueuedJob) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func(job Job) {
			defer wg.Done()
			w.process(ctx, job)
		}(job)
	}
	wg.Wait()
	return true, nil
}

func (w *Worker) process(ctx context.Context, job Job) {
	// A panic in an image decoder must retire one job, not take down the
	// process that is also serving uploads.
	defer func() {
		if recovered := recover(); recovered != nil {
			w.log.Error("media job panicked", "job_id", job.ID, "media_id", job.MediaID, "kind", job.Kind, "error", recovered)
			w.fail(ctx, job, fmt.Sprintf("panic: %v", recovered))
		}
	}()

	var err error
	switch job.Kind {
	case KindThumbnail:
		err = w.runner.GenerateThumbnail(ctx, job.MediaID)
	default:
		// An unknown kind is a deployment that rolled back below the migration
		// that introduced it. Retiring the job keeps it out of the way.
		w.log.Warn("media job has unknown kind", "job_id", job.ID, "kind", job.Kind)
		w.fail(ctx, job, "unknown job kind")
		return
	}

	if err == nil {
		if markErr := w.repo.MarkDone(context.WithoutCancel(ctx), job.ID); markErr != nil {
			w.log.Error("media job completion not recorded", "job_id", job.ID, "error", markErr)
		}
		return
	}
	w.fail(ctx, job, err.Error())
}

func (w *Worker) fail(ctx context.Context, job Job, cause string) {
	// The claim is already durable, so completion must be recorded even when
	// the surrounding context is being torn down.
	ctx = context.WithoutCancel(ctx)
	if job.Attempts >= w.cfg.MaxAttempts {
		w.log.Error("media job retired after repeated failures",
			"job_id", job.ID, "media_id", job.MediaID, "kind", job.Kind, "attempts", job.Attempts, "error", cause)
		if err := w.repo.MarkFailed(ctx, job.ID, cause); err != nil {
			w.log.Error("media job failure not recorded", "job_id", job.ID, "error", err)
		}
		return
	}
	// Attempts is incremented by the claim, so it is at least 1 here. The
	// guard keeps a hand-written or restored row from shifting by a negative
	// amount, which would panic.
	shift := job.Attempts - 1
	if shift < 0 {
		shift = 0
	}
	delay := w.cfg.Backoff << shift
	w.log.Warn("media job rescheduled",
		"job_id", job.ID, "media_id", job.MediaID, "kind", job.Kind, "attempts", job.Attempts, "retry_in", delay.String(), "error", cause)
	if err := w.repo.Reschedule(ctx, job.ID, time.Now().UTC().Add(delay), cause); err != nil {
		w.log.Error("media job reschedule not recorded", "job_id", job.ID, "error", err)
	}
}
