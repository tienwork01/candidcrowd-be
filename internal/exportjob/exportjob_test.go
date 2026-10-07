package exportjob_test

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/exportjob"
	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockStorage struct {
	mu      sync.Mutex
	files   map[string][]byte
	putData map[string][]byte
}

func newMockStorage() *mockStorage {
	return &mockStorage{
		files:   make(map[string][]byte),
		putData: make(map[string][]byte),
	}
}

func (m *mockStorage) OpenRead(ctx context.Context, key string) (io.ReadCloser, media.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.files[key]
	if !ok {
		return nil, media.ObjectInfo{}, fmt.Errorf("file not found: %s", key)
	}
	return io.NopCloser(bytes.NewReader(data)), media.ObjectInfo{Size: int64(len(data))}, nil
}

func (m *mockStorage) Put(ctx context.Context, key, mime string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putData[key] = data
	return nil
}

func (m *mockStorage) PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error) {
	return "https://storage.example.com/" + key, nil
}

type mockArchiveReader struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newMockArchiveReader() *mockArchiveReader {
	return &mockArchiveReader{files: make(map[string][]byte)}
}

func (m *mockArchiveReader) OpenRead(ctx context.Context, location string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.files[location]
	if !ok {
		return nil, fmt.Errorf("archive file not found: %s", location)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

type mockRepository struct {
	mu         sync.Mutex
	jobs       map[uuid.UUID]exportjob.Job
	queued     []uuid.UUID
	items      map[uuid.UUID][]exportjob.Item
	readyCalls map[uuid.UUID]string
	failCalls  map[uuid.UUID]string
}

func newMockRepository() *mockRepository {
	return &mockRepository{
		jobs:       make(map[uuid.UUID]exportjob.Job),
		items:      make(map[uuid.UUID][]exportjob.Item),
		readyCalls: make(map[uuid.UUID]string),
		failCalls:  make(map[uuid.UUID]string),
	}
}

func (r *mockRepository) Create(ctx context.Context, job exportjob.Job) (exportjob.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs[job.ID] = job
	r.queued = append(r.queued, job.ID)
	return job, nil
}

func (r *mockRepository) FindActive(ctx context.Context, eventID uuid.UUID) (exportjob.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, job := range r.jobs {
		if job.EventID == eventID && (job.Status == exportjob.StatusQueued || job.Status == exportjob.StatusProcessing) {
			return job, nil
		}
	}
	return exportjob.Job{}, exportjob.ErrNotFound
}

func (r *mockRepository) FindLatest(ctx context.Context, eventID uuid.UUID) (exportjob.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var latest exportjob.Job
	var found bool
	for _, job := range r.jobs {
		if job.EventID == eventID {
			if !found || job.CreatedAt.After(latest.CreatedAt) {
				latest = job
				found = true
			}
		}
	}
	if !found {
		return exportjob.Job{}, exportjob.ErrNotFound
	}
	return latest, nil
}

func (r *mockRepository) FindOwned(ctx context.Context, eventID, jobID uuid.UUID) (exportjob.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.jobs[jobID]
	if !ok || job.EventID != eventID {
		return exportjob.Job{}, exportjob.ErrNotFound
	}
	return job, nil
}

func (r *mockRepository) ClaimNext(ctx context.Context) (exportjob.Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.queued) == 0 {
		return exportjob.Job{}, exportjob.ErrNoQueuedJob
	}
	id := r.queued[0]
	r.queued = r.queued[1:]
	job := r.jobs[id]
	job.Status = exportjob.StatusProcessing
	r.jobs[id] = job
	return job, nil
}

func (r *mockRepository) ListItems(ctx context.Context, eventID uuid.UUID) ([]exportjob.Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.items[eventID], nil
}

func (r *mockRepository) MarkReady(ctx context.Context, jobID uuid.UUID, objectKey string, completedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.jobs[jobID]
	if !ok {
		return exportjob.ErrNotFound
	}
	job.Status = exportjob.StatusReady
	job.ObjectKey = &objectKey
	job.CompletedAt = &completedAt
	r.jobs[jobID] = job
	r.readyCalls[jobID] = objectKey
	return nil
}

func (r *mockRepository) MarkFailed(ctx context.Context, jobID uuid.UUID, message string, completedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.jobs[jobID]
	if !ok {
		return exportjob.ErrNotFound
	}
	job.Status = exportjob.StatusFailed
	job.ErrorMessage = &message
	job.CompletedAt = &completedAt
	r.jobs[jobID] = job
	r.failCalls[jobID] = message
	return nil
}

func TestJobTableName(t *testing.T) {
	j := exportjob.Job{}
	assert.Equal(t, "event_exports", j.TableName(), "Job struct must map to table 'event_exports'")
}

func TestService_RunNext_Success(t *testing.T) {
	ctx := context.Background()
	storage := newMockStorage()
	repo := newMockRepository()
	service := exportjob.NewService(repo, storage)

	eventID := uuid.New()
	item1ID := uuid.New()
	item2ID := uuid.New()

	storage.files["events/1/media/1.jpg"] = []byte("photo content 1")
	storage.files["events/1/media/2.mp4"] = []byte("video content 2")

	repo.items[eventID] = []exportjob.Item{
		{ID: item1ID, ObjectKey: "events/1/media/1.jpg", OriginalFilename: "photo.jpg"},
		{ID: item2ID, ObjectKey: "events/1/media/2.mp4", OriginalFilename: "photo.jpg"}, // Duplicate filename test
	}

	createdJob, err := service.Create(ctx, eventID)
	require.NoError(t, err)
	assert.Equal(t, exportjob.StatusQueued, createdJob.Status)

	ran, err := service.RunNext(ctx)
	require.NoError(t, err)
	assert.True(t, ran)

	// Verify job was marked ready
	expectedKey := fmt.Sprintf("events/%s/exports/%s.zip", eventID, createdJob.ID)
	assert.Equal(t, expectedKey, repo.readyCalls[createdJob.ID])

	// Verify ZIP archive contents
	zipBytes, exists := storage.putData[expectedKey]
	require.True(t, exists, "ZIP file must be uploaded to storage")
	require.NotEmpty(t, zipBytes)

	zipReader, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	require.NoError(t, err)
	require.Len(t, zipReader.File, 2)

	// Check file names and contents in ZIP
	names := []string{zipReader.File[0].Name, zipReader.File[1].Name}
	assert.Contains(t, names, "photo.jpg")
	assert.Contains(t, names, fmt.Sprintf("photo-%s.jpg", item2ID.String()[:8]))

	for _, f := range zipReader.File {
		rc, err := f.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		require.NoError(t, err)
		assert.Contains(t, []string{"photo content 1", "video content 2"}, string(data))
	}

	// Test Get service method
	readyJob, downloadURL, err := service.Get(ctx, eventID, createdJob.ID)
	require.NoError(t, err)
	assert.Equal(t, exportjob.StatusReady, readyJob.Status)
	assert.Contains(t, downloadURL, expectedKey)
}

func TestService_RunNext_EmptyEvent(t *testing.T) {
	ctx := context.Background()
	storage := newMockStorage()
	repo := newMockRepository()
	service := exportjob.NewService(repo, storage)

	eventID := uuid.New()
	repo.items[eventID] = []exportjob.Item{} // Zero items

	createdJob, err := service.Create(ctx, eventID)
	require.NoError(t, err)

	ran, err := service.RunNext(ctx)
	assert.Error(t, err)
	assert.True(t, ran)
	assert.Equal(t, "no media available for export", repo.failCalls[createdJob.ID])
}

func TestService_Create_Deduplication(t *testing.T) {
	ctx := context.Background()
	storage := newMockStorage()
	repo := newMockRepository()
	service := exportjob.NewService(repo, storage)

	eventID := uuid.New()
	job1, err := service.Create(ctx, eventID)
	require.NoError(t, err)
	assert.Equal(t, exportjob.StatusQueued, job1.Status)

	// Second create while job1 is still queued should return existing job1
	job2, err := service.Create(ctx, eventID)
	require.NoError(t, err)
	assert.Equal(t, job1.ID, job2.ID, "Subsequent Create call should return existing active job")
}

func TestService_RunNext_GoogleDriveArchive(t *testing.T) {
	ctx := context.Background()
	storage := newMockStorage()
	archive := newMockArchiveReader()
	repo := newMockRepository()
	service := exportjob.NewService(repo, storage, exportjob.WithArchiveReader(archive))

	eventID := uuid.New()
	deletedAt := time.Now().UTC()
	driveFileID := "gdrive-file-12345"
	archive.files[driveFileID] = []byte("photo stored in google drive")

	repo.items[eventID] = []exportjob.Item{
		{
			ID:               uuid.New(),
			ObjectKey:        "events/evt1/media/med1/original.jpg",
			OriginalFilename: "archived-wedding.jpg",
			SourceDeletedAt:  &deletedAt,
			DriveLocation:    &driveFileID,
		},
	}

	createdJob, err := service.Create(ctx, eventID)
	require.NoError(t, err)

	ran, err := service.RunNext(ctx)
	require.NoError(t, err)
	assert.True(t, ran)

	expectedKey := fmt.Sprintf("events/%s/exports/%s.zip", eventID, createdJob.ID)
	zipBytes, exists := storage.putData[expectedKey]
	require.True(t, exists)

	zipReader, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	require.NoError(t, err)
	require.Len(t, zipReader.File, 1)
	assert.Equal(t, "archived-wedding.jpg", zipReader.File[0].Name)

	rc, err := zipReader.File[0].Open()
	require.NoError(t, err)
	content, err := io.ReadAll(rc)
	_ = rc.Close()
	require.NoError(t, err)
	assert.Equal(t, "photo stored in google drive", string(content))
}

func TestService_RunNext_Hybrid(t *testing.T) {
	ctx := context.Background()
	storage := newMockStorage()
	archive := newMockArchiveReader()
	repo := newMockRepository()
	service := exportjob.NewService(repo, storage, exportjob.WithArchiveReader(archive))

	eventID := uuid.New()
	deletedAt := time.Now().UTC()

	// Item 1: in R2 (hot)
	storage.files["events/evt1/media/hot/original.jpg"] = []byte("hot r2 photo")

	// Item 2: in Google Drive (archived)
	driveFileID := "gdrive-cold-6789"
	archive.files[driveFileID] = []byte("cold drive photo")

	repo.items[eventID] = []exportjob.Item{
		{
			ID:               uuid.New(),
			ObjectKey:        "events/evt1/media/hot/original.jpg",
			OriginalFilename: "ceremony.jpg",
			SourceDeletedAt:  nil,
		},
		{
			ID:               uuid.New(),
			ObjectKey:        "events/evt1/media/cold/original.jpg",
			OriginalFilename: "reception.jpg",
			SourceDeletedAt:  &deletedAt,
			DriveLocation:    &driveFileID,
		},
	}

	createdJob, err := service.Create(ctx, eventID)
	require.NoError(t, err)

	ran, err := service.RunNext(ctx)
	require.NoError(t, err)
	assert.True(t, ran)

	expectedKey := fmt.Sprintf("events/%s/exports/%s.zip", eventID, createdJob.ID)
	zipBytes := storage.putData[expectedKey]
	zipReader, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	require.NoError(t, err)
	require.Len(t, zipReader.File, 2)

	contents := make(map[string]string)
	for _, f := range zipReader.File {
		rc, err := f.Open()
		require.NoError(t, err)
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		require.NoError(t, err)
		contents[f.Name] = string(data)
	}

	assert.Equal(t, "hot r2 photo", contents["ceremony.jpg"])
	assert.Equal(t, "cold drive photo", contents["reception.jpg"])
}

func TestService_RunNext_ArchiveMissingReader(t *testing.T) {
	ctx := context.Background()
	storage := newMockStorage()
	repo := newMockRepository()
	// Service configured WITHOUT ArchiveReader
	service := exportjob.NewService(repo, storage)

	eventID := uuid.New()
	deletedAt := time.Now().UTC()
	driveFileID := "gdrive-orphan"

	repo.items[eventID] = []exportjob.Item{
		{
			ID:               uuid.New(),
			ObjectKey:        "events/evt1/media/cold/original.jpg",
			OriginalFilename: "orphaned.jpg",
			SourceDeletedAt:  &deletedAt,
			DriveLocation:    &driveFileID,
		},
	}

	createdJob, err := service.Create(ctx, eventID)
	require.NoError(t, err)

	ran, err := service.RunNext(ctx)
	assert.Error(t, err)
	assert.True(t, ran)
	assert.Contains(t, repo.failCalls[createdJob.ID], "archive storage is not configured")
}

func TestService_Latest(t *testing.T) {
	ctx := context.Background()
	storage := newMockStorage()
	repo := newMockRepository()
	service := exportjob.NewService(repo, storage)

	eventID := uuid.New()

	// 1. When no job exists -> ErrNotFound
	_, _, err := service.Latest(ctx, eventID)
	assert.ErrorIs(t, err, exportjob.ErrNotFound)

	// 2. When job is queued
	job, err := service.Create(ctx, eventID)
	require.NoError(t, err)

	latest, url, err := service.Latest(ctx, eventID)
	require.NoError(t, err)
	assert.Equal(t, job.ID, latest.ID)
	assert.Equal(t, exportjob.StatusQueued, latest.Status)
	assert.Empty(t, url) // not ready yet
}
