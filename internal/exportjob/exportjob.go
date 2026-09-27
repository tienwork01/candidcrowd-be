package exportjob

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/google/uuid"
)

type Status string

const (
	StatusQueued     Status = "queued"
	StatusProcessing Status = "processing"
	StatusReady      Status = "ready"
	StatusFailed     Status = "failed"
)

type Job struct {
	ID           uuid.UUID  `json:"id"`
	EventID      uuid.UUID  `json:"event_id"`
	Status       Status     `json:"status"`
	ObjectKey    *string    `json:"-"`
	ErrorMessage *string    `json:"error_message,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

type Item struct {
	ID               uuid.UUID
	ObjectKey        string
	OriginalFilename string
}

type Storage interface {
	OpenRead(context.Context, string) (io.ReadCloser, media.ObjectInfo, error)
	Put(context.Context, string, string, io.Reader) error
	PresignGet(context.Context, string, time.Duration) (string, error)
}

type Repository interface {
	Create(context.Context, Job) (Job, error)
	FindOwned(context.Context, uuid.UUID, uuid.UUID) (Job, error)
	ClaimNext(context.Context) (Job, error)
	ListItems(context.Context, uuid.UUID) ([]Item, error)
	MarkReady(context.Context, uuid.UUID, string, time.Time) error
	MarkFailed(context.Context, uuid.UUID, string, time.Time) error
}

type Service struct {
	repo    Repository
	storage Storage
	logger  *slog.Logger
}

func NewService(repo Repository, storage Storage, loggers ...*slog.Logger) *Service {
	s := &Service{repo: repo, storage: storage}
	if len(loggers) > 0 {
		s.logger = loggers[0]
	}
	return s
}

func (s *Service) Create(ctx context.Context, eventID uuid.UUID) (Job, error) {
	return s.repo.Create(ctx, Job{ID: uuid.New(), EventID: eventID, Status: StatusQueued})
}

func (s *Service) Get(ctx context.Context, eventID, jobID uuid.UUID) (Job, string, error) {
	job, err := s.repo.FindOwned(ctx, eventID, jobID)
	if err != nil || job.Status != StatusReady || job.ObjectKey == nil {
		return job, "", err
	}
	url, err := s.storage.PresignGet(ctx, *job.ObjectKey, 10*time.Minute)
	return job, url, err
}

// Run drains the export queue until ctx is cancelled.
//
// It replaces a cron that fired once a minute and claimed a single job, which
// made the tenth waiting host wait ten minutes while the server sat idle, and
// which had no way to stop a thirty-minute job from overlapping with the next
// twenty-nine ticks. Here a job is followed immediately by the next one and
// nothing else runs beside it, so a ZIP being streamed through this process
// never competes with another.
func (s *Service) Run(ctx context.Context, idle time.Duration) {
	if idle <= 0 {
		idle = 5 * time.Second
	}
	timer := time.NewTimer(idle)
	defer timer.Stop()
	if !timer.Stop() {
		<-timer.C
	}
	for {
		ran, err := s.RunNext(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.log().Error("event export failed", "error", err)
		}
		if ran && ctx.Err() == nil {
			continue
		}
		timer.Reset(idle)
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

func (s *Service) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// RunNext claims at most one job and reports whether it found one. It is safe
// to run from multiple replicas: the claim uses FOR UPDATE SKIP LOCKED.
func (s *Service) RunNext(ctx context.Context) (bool, error) {
	job, err := s.repo.ClaimNext(ctx)
	if errors.Is(err, ErrNoQueuedJob) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	items, err := s.repo.ListItems(ctx, job.EventID)
	if err == nil && len(items) == 0 {
		err = fmt.Errorf("no media available for export")
	}
	key := fmt.Sprintf("events/%s/exports/%s.zip", job.EventID, job.ID)
	if err == nil {
		err = s.writeZip(ctx, key, items)
	}
	if err != nil {
		// The claim is durable, so the outcome is recorded even when the
		// surrounding context has been cancelled.
		_ = s.repo.MarkFailed(context.WithoutCancel(ctx), job.ID, err.Error(), time.Now().UTC())
		return true, err
	}
	return true, s.repo.MarkReady(ctx, job.ID, key, time.Now().UTC())
}

func (s *Service) writeZip(ctx context.Context, key string, items []Item) error {
	reader, writer := io.Pipe()
	putResult := make(chan error, 1)
	go func() {
		err := s.storage.Put(ctx, key, "application/zip", reader)
		_ = reader.CloseWithError(err)
		putResult <- err
	}()

	zipWriter := zip.NewWriter(writer)
	usedNames := make(map[string]int, len(items))
	var writeErr error
	for _, item := range items {
		body, _, err := s.storage.OpenRead(ctx, item.ObjectKey)
		if err != nil {
			writeErr = err
			break
		}
		name := exportFilename(item, usedNames)
		entry, err := zipWriter.Create(name)
		if err == nil {
			_, err = io.Copy(entry, body)
		}
		closeErr := body.Close()
		if err != nil {
			writeErr = err
			break
		}
		if closeErr != nil {
			writeErr = closeErr
			break
		}
	}
	if closeErr := zipWriter.Close(); writeErr == nil && closeErr != nil {
		writeErr = closeErr
	}
	_ = writer.CloseWithError(writeErr)
	putErr := <-putResult
	if writeErr != nil {
		return writeErr
	}
	return putErr
}

func exportFilename(item Item, used map[string]int) string {
	name := filepath.Base(item.OriginalFilename)
	if name == "." || name == "" {
		name = item.ID.String()
	}
	if count := used[name]; count > 0 {
		ext := filepath.Ext(name)
		name = strings.TrimSuffix(name, ext) + "-" + item.ID.String()[:8] + ext
	}
	used[name]++
	return name
}
