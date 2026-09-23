package media

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUploadValidation(t *testing.T) {
	tests := []struct {
		mime      string
		allowed   bool
		extension string
	}{
		{"image/jpeg", true, ".jpg"},
		{"image/png", true, ".png"},
		{"image/webp", true, ".webp"},
		{"video/mp4", true, ".mp4"},
		{"video/quicktime", true, ".mov"},
		{"image/svg+xml", false, ""},
		{"image/gif", false, ""},
		{"application/pdf", false, ""},
		{"text/html", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.mime, func(t *testing.T) {
			if got := allowed(tt.mime); got != tt.allowed {
				t.Fatalf("allowed(%q)=%v, want %v", tt.mime, got, tt.allowed)
			}
			if got := extension(tt.mime); got != tt.extension {
				t.Fatalf("extension(%q)=%q, want %q", tt.mime, got, tt.extension)
			}
		})
	}
}

type memoryRepository struct {
	records map[uuid.UUID]Media
	ready   map[uuid.UUID]bool
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{records: map[uuid.UUID]Media{}, ready: map[uuid.UUID]bool{}}
}

func (r *memoryRepository) ReserveUpload(_ context.Context, record Media, _ int64) error {
	r.records[record.ID] = record
	return nil
}
func (r *memoryRepository) FindByClientUpload(_ context.Context, eventID, sessionID, clientUploadID uuid.UUID) (Media, error) {
	for _, record := range r.records {
		if record.EventID == eventID && record.GuestSessionID == sessionID && record.ClientUploadID != nil && *record.ClientUploadID == clientUploadID {
			return record, nil
		}
	}
	return Media{}, ErrNotFound
}
func (r *memoryRepository) HasReadyChecksum(_ context.Context, eventID uuid.UUID, checksumSHA256 string) (bool, error) {
	for id, record := range r.records {
		if r.ready[id] && record.EventID == eventID && record.ChecksumSHA256 == checksumSHA256 {
			return true, nil
		}
	}
	return false, nil
}
func (r *memoryRepository) Delete(_ context.Context, id uuid.UUID) error {
	delete(r.records, id)
	return nil
}
func (r *memoryRepository) FindUploadForSession(_ context.Context, id, eventID, sessionID uuid.UUID) (Media, error) {
	record, ok := r.records[id]
	if !ok || record.EventID != eventID || record.GuestSessionID != sessionID {
		return Media{}, ErrNotFound
	}
	return record, nil
}
func (r *memoryRepository) MarkReady(_ context.Context, eventID, id uuid.UUID, actualSize int64, uploadedAt time.Time) error {
	record, ok := r.records[id]
	if !ok || record.EventID != eventID {
		return ErrNotFound
	}
	record.Status, record.ActualSize, record.UploadedAt = StatusReady, &actualSize, &uploadedAt
	r.records[id], r.ready[id] = record, true
	return nil
}
func (r *memoryRepository) ListReady(_ context.Context, _ uuid.UUID, _ int, _ *Cursor) ([]Media, error) {
	return nil, nil
}
func (r *memoryRepository) FindReady(_ context.Context, eventID, id uuid.UUID) (Media, error) {
	record, ok := r.records[id]
	if !ok || record.EventID != eventID || !r.ready[id] {
		return Media{}, ErrNotFound
	}
	return record, nil
}
func (r *memoryRepository) FindArchivedLocation(context.Context, uuid.UUID, uuid.UUID) (string, error) {
	return "", ErrNotFound
}
func (r *memoryRepository) FindStale(_ context.Context, before time.Time, _ int) ([]Media, error) {
	items := make([]Media, 0)
	for _, record := range r.records {
		if record.Status != StatusReady && record.LastActivityAt.Before(before) {
			items = append(items, record)
		}
	}
	return items, nil
}

type memoryStorage struct{ head ObjectInfo }

func (s memoryStorage) PresignPut(context.Context, string, string, time.Duration) (string, error) {
	return "https://upload.example", nil
}
func (s memoryStorage) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "https://download.example", nil
}
func (s memoryStorage) Head(context.Context, string) (ObjectInfo, error) { return s.head, nil }
func (s memoryStorage) Delete(context.Context, string) error             { return nil }

type allowAllLimiter struct{}

func (allowAllLimiter) Allow(context.Context, string, int, time.Duration) (bool, error) {
	return true, nil
}

func TestServiceCreatesAndCompletesWithoutInfrastructure(t *testing.T) {
	repo := newMemoryRepository()
	service := NewService(repo, memoryStorage{head: ObjectInfo{Size: 42, ContentType: "image/jpeg"}}, allowAllLimiter{}, time.Minute, time.Minute, 1, 100, 100)
	scope := UploadScope{EventID: uuid.New(), GuestSessionID: uuid.New(), Accepting: true, MaxEventBytes: 1_000}

	input := CreateInput{Filename: "guest.jpg", MIMEType: "image/jpeg", Size: 42, ChecksumSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", ClientUploadID: uuid.New()}
	target, err := service.CreateUpload(context.Background(), scope, input)
	require.NoError(t, err)
	require.NotEmpty(t, target.UploadURL)

	require.NoError(t, service.Complete(context.Background(), scope, target.MediaID))
	require.NoError(t, service.Complete(context.Background(), scope, target.MediaID), "complete must survive a lost success response")
	record, err := repo.FindReady(context.Background(), scope.EventID, target.MediaID)
	require.NoError(t, err)
	require.Equal(t, StatusReady, record.Status)
	require.EqualValues(t, 42, *record.ActualSize)
}

func TestServiceRejectsReadyChecksumInSameEvent(t *testing.T) {
	repo := newMemoryRepository()
	service := NewService(repo, memoryStorage{head: ObjectInfo{Size: 42, ContentType: "image/jpeg"}}, allowAllLimiter{}, time.Minute, time.Minute, 1, 100, 100)
	scope := UploadScope{EventID: uuid.New(), GuestSessionID: uuid.New(), Accepting: true, MaxEventBytes: 1_000}
	checksum := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	target, err := service.CreateUpload(context.Background(), scope, CreateInput{Filename: "guest.jpg", MIMEType: "image/jpeg", Size: 42, ChecksumSHA256: checksum, ClientUploadID: uuid.New()})
	require.NoError(t, err)
	require.NoError(t, service.Complete(context.Background(), scope, target.MediaID))

	_, err = service.CreateUpload(context.Background(), scope, CreateInput{Filename: "same.jpg", MIMEType: "image/jpeg", Size: 42, ChecksumSHA256: checksum, ClientUploadID: uuid.New()})
	require.ErrorIs(t, err, ErrDuplicate)
}

func TestServiceReusesClientUploadAfterLostCreateResponse(t *testing.T) {
	repo := newMemoryRepository()
	service := NewService(repo, memoryStorage{head: ObjectInfo{Size: 42, ContentType: "image/jpeg"}}, allowAllLimiter{}, time.Minute, time.Minute, 10, 100, 100)
	scope := UploadScope{EventID: uuid.New(), GuestSessionID: uuid.New(), Accepting: true, MaxEventBytes: 1_000}
	input := CreateInput{Filename: "guest.jpg", MIMEType: "image/jpeg", Size: 42, ChecksumSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", ClientUploadID: uuid.New()}
	first, err := service.CreateUpload(context.Background(), scope, input)
	require.NoError(t, err)
	second, err := service.CreateUpload(context.Background(), scope, input)
	require.NoError(t, err)
	require.Equal(t, first.MediaID, second.MediaID)
	require.Len(t, repo.records, 1)
}
