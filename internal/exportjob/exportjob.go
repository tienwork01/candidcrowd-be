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

func (Job) TableName() string {
	return "event_exports"
}

type Item struct {
	ID               uuid.UUID  `json:"id"`
	ObjectKey        string     `json:"object_key"`
	OriginalFilename string     `json:"original_filename"`
	SourceDeletedAt  *time.Time `json:"source_deleted_at,omitempty"`
	DriveLocation    *string    `json:"drive_location,omitempty"`
}

type Storage interface {
	OpenRead(context.Context, string) (io.ReadCloser, media.ObjectInfo, error)
	Put(context.Context, string, string, io.Reader) error
	PresignGet(context.Context, string, time.Duration) (string, error)
}

type ArchiveReader interface {
	OpenRead(context.Context, string) (io.ReadCloser, error)
}

type Repository interface {
	Create(context.Context, Job) (Job, error)
	FindActive(context.Context, uuid.UUID) (Job, error)
	FindLatest(context.Context, uuid.UUID) (Job, error)
	FindOwned(context.Context, uuid.UUID, uuid.UUID) (Job, error)
	ClaimNext(context.Context) (Job, error)
	ListItems(context.Context, uuid.UUID) ([]Item, error)
	MarkReady(context.Context, uuid.UUID, string, time.Time) error
	MarkFailed(context.Context, uuid.UUID, string, time.Time) error
}

type Service struct {
	repo    Repository
	storage Storage
	archive ArchiveReader
	logger  *slog.Logger
}

type Option func(*Service)

func WithArchiveReader(r ArchiveReader) Option {
	return func(s *Service) {
		s.archive = r
	}
}

func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		s.logger = l
	}
}

func NewService(repo Repository, storage Storage, opts ...any) *Service {
	s := &Service{repo: repo, storage: storage}
	for _, opt := range opts {
		switch v := opt.(type) {
		case *slog.Logger:
			s.logger = v
		case ArchiveReader:
			s.archive = v
		case Option:
			v(s)
		}
	}
	return s
}

func (s *Service) Create(ctx context.Context, eventID uuid.UUID) (Job, error) {
	active, err := s.repo.FindActive(ctx, eventID)
	if err == nil {
		return active, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Job{}, err
	}
	return s.repo.Create(ctx, Job{ID: uuid.New(), EventID: eventID, Status: StatusQueued})
}

type downloadSigner interface {
	PresignDownload(context.Context, string, string, time.Duration) (string, error)
}

func (s *Service) presignDownload(ctx context.Context, key, filename string, expiry time.Duration) (string, error) {
	if ds, ok := s.storage.(downloadSigner); ok && filename != "" {
		return ds.PresignDownload(ctx, key, filename, expiry)
	}
	return s.storage.PresignGet(ctx, key, expiry)
}

func (s *Service) Latest(ctx context.Context, eventID uuid.UUID) (Job, string, error) {
	return s.LatestWithFilename(ctx, eventID, "")
}

func (s *Service) LatestWithFilename(ctx context.Context, eventID uuid.UUID, filename string) (Job, string, error) {
	job, err := s.repo.FindLatest(ctx, eventID)
	if err != nil {
		return Job{}, "", err
	}
	if job.Status != StatusReady || job.ObjectKey == nil {
		return job, "", nil
	}
	url, err := s.presignDownload(ctx, *job.ObjectKey, filename, 1*time.Hour)
	return job, url, err
}

func (s *Service) Get(ctx context.Context, eventID, jobID uuid.UUID) (Job, string, error) {
	return s.GetWithFilename(ctx, eventID, jobID, "")
}

func (s *Service) GetWithFilename(ctx context.Context, eventID, jobID uuid.UUID, filename string) (Job, string, error) {
	job, err := s.repo.FindOwned(ctx, eventID, jobID)
	if err != nil || job.Status != StatusReady || job.ObjectKey == nil {
		return job, "", err
	}
	url, err := s.presignDownload(ctx, *job.ObjectKey, filename, 10*time.Minute)
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

func (s *Service) openArchiveWithRetry(ctx context.Context, location string) (io.ReadCloser, error) {
	var lastErr error
	backoff := 200 * time.Millisecond
	for attempt := 0; attempt < 3; attempt++ {
		body, err := s.archive.OpenRead(ctx, location)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		time.Sleep(backoff)
		backoff *= 2
	}
	return nil, fmt.Errorf("archive read failed after 3 attempts: %w", lastErr)
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
		var body io.ReadCloser
		var err error

		if item.SourceDeletedAt == nil || item.DriveLocation == nil || *item.DriveLocation == "" {
			body, _, err = s.storage.OpenRead(ctx, item.ObjectKey)
		} else {
			if s.archive == nil {
				writeErr = fmt.Errorf("media %s is archived to Google Drive but archive storage is not configured", item.ID)
				break
			}
			body, err = s.openArchiveWithRetry(ctx, *item.DriveLocation)
		}

		if err != nil {
			writeErr = err
			break
		}
		name := exportFilename(item, usedNames)
		entry, err := zipWriter.CreateHeader(&zip.FileHeader{
			Name:   name,
			Method: zip.Store,
		})
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
	name := strings.TrimSpace(filepath.Base(item.OriginalFilename))
	defaultExt := filepath.Ext(item.ObjectKey)
	if defaultExt == "" {
		defaultExt = ".jpg"
	}

	// If missing, empty, dot, or generic camera blob, generate a clean friendly base name
	if name == "." || name == "" || name == "blob" || strings.HasPrefix(name, "blob.") {
		if strings.HasSuffix(defaultExt, ".mov") || strings.HasSuffix(defaultExt, ".mp4") {
			name = "candid-video" + defaultExt
		} else {
			name = "candid-photo" + defaultExt
		}
	} else if filepath.Ext(name) == "" {
		// Ensure extension exists if original didn't have one
		name += defaultExt
	}

	if count := used[name]; count > 0 {
		ext := filepath.Ext(name)
		name = strings.TrimSuffix(name, ext) + "-" + item.ID.String()[:8] + ext
	}
	used[name]++
	return name
}
