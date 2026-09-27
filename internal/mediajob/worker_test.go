package mediajob

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeRepository serves a fixed queue and records every transition, so a test
// can assert what the worker decided rather than how it got there.
type fakeRepository struct {
	mu          sync.Mutex
	queue       []Job
	done        []uuid.UUID
	failed      []uuid.UUID
	rescheduled map[uuid.UUID]time.Time
	causes      map[uuid.UUID]string
}

func newFakeRepository(jobs ...Job) *fakeRepository {
	return &fakeRepository{
		queue:       jobs,
		rescheduled: map[uuid.UUID]time.Time{},
		causes:      map[uuid.UUID]string{},
	}
}

func (r *fakeRepository) ClaimBatch(_ context.Context, limit int, _ time.Time) ([]Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.queue) == 0 {
		return nil, ErrNoQueuedJob
	}
	if limit > len(r.queue) {
		limit = len(r.queue)
	}
	claimed := make([]Job, 0, limit)
	for _, job := range r.queue[:limit] {
		// The real claim increments attempts as it takes the row.
		job.Status = StatusProcessing
		job.Attempts++
		claimed = append(claimed, job)
	}
	r.queue = r.queue[limit:]
	return claimed, nil
}

func (r *fakeRepository) MarkDone(_ context.Context, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done = append(r.done, id)
	return nil
}

func (r *fakeRepository) Reschedule(_ context.Context, id uuid.UUID, runAfter time.Time, cause string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rescheduled[id] = runAfter
	r.causes[id] = cause
	return nil
}

func (r *fakeRepository) MarkFailed(_ context.Context, id uuid.UUID, cause string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = append(r.failed, id)
	r.causes[id] = cause
	return nil
}

func (r *fakeRepository) snapshot(read func(*fakeRepository)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	read(r)
}

// fakeRunner counts calls and can be told to fail or to block.
type fakeRunner struct {
	mu      sync.Mutex
	calls   int
	inFlate int
	peak    int
	err     error
	hold    chan struct{}
	panics  bool
}

func (r *fakeRunner) GenerateThumbnail(_ context.Context, _ uuid.UUID) error {
	r.mu.Lock()
	r.calls++
	r.inFlate++
	if r.inFlate > r.peak {
		r.peak = r.inFlate
	}
	hold, shouldPanic, err := r.hold, r.panics, r.err
	r.mu.Unlock()

	if hold != nil {
		<-hold
	}
	defer func() {
		r.mu.Lock()
		r.inFlate--
		r.mu.Unlock()
	}()
	if shouldPanic {
		panic("decoder exploded")
	}
	return err
}

func queuedJob() Job {
	return Job{ID: uuid.New(), MediaID: uuid.New(), Kind: KindThumbnail, Status: StatusQueued}
}

func TestWorkerCompletesClaimedJobs(t *testing.T) {
	first, second := queuedJob(), queuedJob()
	repo := newFakeRepository(first, second)
	runner := &fakeRunner{}
	worker := NewWorker(repo, runner, discardLogger(), Config{Concurrency: 2, Idle: time.Millisecond})

	worked, err := worker.drainOnce(context.Background())
	require.NoError(t, err)
	require.True(t, worked)

	repo.snapshot(func(r *fakeRepository) {
		require.ElementsMatch(t, []uuid.UUID{first.ID, second.ID}, r.done)
		require.Empty(t, r.rescheduled)
		require.Empty(t, r.failed)
	})
	require.Equal(t, 2, runner.calls)
}

func TestWorkerRetriesWithGrowingBackoff(t *testing.T) {
	job := queuedJob()
	job.Attempts = 1 // the claim makes this the second attempt
	repo := newFakeRepository(job)
	runner := &fakeRunner{err: errors.New("storage unavailable")}
	worker := NewWorker(repo, runner, discardLogger(), Config{Concurrency: 1, Backoff: time.Minute, MaxAttempts: 5})

	before := time.Now().UTC()
	_, err := worker.drainOnce(context.Background())
	require.NoError(t, err)

	repo.snapshot(func(r *fakeRepository) {
		require.Empty(t, r.failed, "a transient failure must not retire the job")
		runAfter, ok := r.rescheduled[job.ID]
		require.True(t, ok)
		// Second attempt doubles the base delay once.
		require.WithinDuration(t, before.Add(2*time.Minute), runAfter, 5*time.Second)
		require.Contains(t, r.causes[job.ID], "storage unavailable")
	})
}

func TestWorkerRetiresJobThatExhaustsItsAttempts(t *testing.T) {
	job := queuedJob()
	job.Attempts = 4 // the claim makes this the fifth and final attempt
	repo := newFakeRepository(job)
	runner := &fakeRunner{err: errors.New("still broken")}
	worker := NewWorker(repo, runner, discardLogger(), Config{Concurrency: 1, MaxAttempts: 5})

	_, err := worker.drainOnce(context.Background())
	require.NoError(t, err)

	repo.snapshot(func(r *fakeRepository) {
		require.Equal(t, []uuid.UUID{job.ID}, r.failed)
		require.Empty(t, r.rescheduled)
	})
}

// A panic inside an image decoder must cost one job, not the process that is
// also serving guest uploads.
func TestWorkerSurvivesAPanickingJob(t *testing.T) {
	job := queuedJob()
	repo := newFakeRepository(job)
	worker := NewWorker(repo, &fakeRunner{panics: true}, discardLogger(), Config{Concurrency: 1, MaxAttempts: 5, Backoff: time.Second})

	require.NotPanics(t, func() {
		_, err := worker.drainOnce(context.Background())
		require.NoError(t, err)
	})
	repo.snapshot(func(r *fakeRepository) {
		require.Contains(t, r.causes[job.ID], "panic")
	})
}

// Concurrency is the only thing standing between an upload burst and an
// out-of-memory kill, so the ceiling has to hold.
func TestWorkerNeverExceedsItsConcurrencyCeiling(t *testing.T) {
	jobs := make([]Job, 0, 8)
	for i := 0; i < 8; i++ {
		jobs = append(jobs, queuedJob())
	}
	repo := newFakeRepository(jobs...)
	runner := &fakeRunner{hold: make(chan struct{})}
	worker := NewWorker(repo, runner, discardLogger(), Config{Concurrency: 2, Idle: time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		worker.Run(ctx)
		close(finished)
	}()

	// Let the first batch reach the runner, then release everything.
	require.Eventually(t, func() bool {
		runner.mu.Lock()
		defer runner.mu.Unlock()
		return runner.calls > 0
	}, time.Second, time.Millisecond)
	close(runner.hold)

	require.Eventually(t, func() bool {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		return len(repo.done) == len(jobs)
	}, 2*time.Second, time.Millisecond)

	cancel()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop when its context was cancelled")
	}

	runner.mu.Lock()
	defer runner.mu.Unlock()
	require.LessOrEqual(t, runner.peak, 2, "more jobs ran at once than the pool allows")
}
