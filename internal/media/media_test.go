package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"sync"
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
	// CompleteMany confirms a batch in parallel, so the fake has to honour the
	// same concurrency contract the Postgres repository does.
	mu      sync.Mutex
	records map[uuid.UUID]Media
	ready   map[uuid.UUID]bool
	// gallery is the ordered page source for ListGallery. It stays nil unless
	// a test seeds it, so tests that do not exercise paging are unaffected.
	gallery []Media
	// countCalls records how often the totals were recomputed.
	countCalls int
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{records: map[uuid.UUID]Media{}, ready: map[uuid.UUID]bool{}}
}

func (r *memoryRepository) ReserveUpload(_ context.Context, record Media, _ int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records[record.ID] = record
	return nil
}
func (r *memoryRepository) FindByClientUpload(_ context.Context, eventID, sessionID, clientUploadID uuid.UUID) (Media, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, record := range r.records {
		if record.EventID == eventID && record.GuestSessionID == sessionID && record.ClientUploadID != nil && *record.ClientUploadID == clientUploadID {
			return record, nil
		}
	}
	return Media{}, ErrNotFound
}
func (r *memoryRepository) HasReadyChecksum(_ context.Context, eventID uuid.UUID, checksumSHA256 string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, record := range r.records {
		if r.ready[id] && record.EventID == eventID && record.ChecksumSHA256 == checksumSHA256 {
			return true, nil
		}
	}
	return false, nil
}
func (r *memoryRepository) Delete(_ context.Context, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.records, id)
	return nil
}
func (r *memoryRepository) UpdateStatus(_ context.Context, eventID, id uuid.UUID, status Status) (Media, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[id]
	if !ok || record.EventID != eventID || !validModerationStatus(status) {
		return Media{}, ErrNotFound
	}
	record.Status = status
	r.records[id] = record
	return record, nil
}
func (r *memoryRepository) UpdateStatuses(ctx context.Context, eventID uuid.UUID, ids []uuid.UUID, status Status) error {
	for _, id := range ids {
		if _, err := r.UpdateStatus(ctx, eventID, id, status); err != nil {
			return err
		}
	}
	return nil
}
func (r *memoryRepository) DeleteForEvent(_ context.Context, eventID, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[id]
	if !ok || record.EventID != eventID {
		return ErrNotFound
	}
	record.Status = StatusDeleted
	r.records[id] = record
	return nil
}
func (r *memoryRepository) DeleteManyForEvent(ctx context.Context, eventID uuid.UUID, ids []uuid.UUID) error {
	for _, id := range ids {
		if err := r.DeleteForEvent(ctx, eventID, id); err != nil {
			return err
		}
	}
	return nil
}
func (r *memoryRepository) FindUploadForSession(_ context.Context, id, eventID, sessionID uuid.UUID) (Media, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[id]
	if !ok || record.EventID != eventID || record.GuestSessionID != sessionID {
		return Media{}, ErrNotFound
	}
	return record, nil
}
func (r *memoryRepository) MarkReady(_ context.Context, eventID, id uuid.UUID, actualSize int64, uploadedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[id]
	if !ok || record.EventID != eventID {
		return ErrNotFound
	}
	record.Status, record.ActualSize, record.UploadedAt = StatusReady, &actualSize, &uploadedAt
	r.records[id], r.ready[id] = record, true
	return nil
}
func (r *memoryRepository) MarkThumbnailReady(_ context.Context, eventID, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[id]
	if !ok || record.EventID != eventID {
		return ErrNotFound
	}
	record.ThumbnailReady = true
	r.records[id] = record
	return nil
}
func (r *memoryRepository) ListReady(_ context.Context, _ uuid.UUID, _ int, _ *Cursor) ([]Media, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return nil, nil
}
func (r *memoryRepository) ListGallery(_ context.Context, _ uuid.UUID, _ GalleryFilter, _ GallerySort, limit int, before *Cursor) ([]Media, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]Media, 0, len(r.gallery))
	for _, record := range r.gallery {
		if before != nil && !record.CreatedAt.Before(before.CreatedAt) {
			continue
		}
		items = append(items, record)
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}
func (r *memoryRepository) GalleryCounts(_ context.Context, _ uuid.UUID) (GalleryCounts, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.countCalls++
	return GalleryCounts{All: int64(len(r.gallery))}, nil
}
func (r *memoryRepository) FindReady(_ context.Context, eventID, id uuid.UUID) (Media, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[id]
	if !ok || record.EventID != eventID || !r.ready[id] {
		return Media{}, ErrNotFound
	}
	return record, nil
}
func (r *memoryRepository) DeleteManyUploads(_ context.Context, ids []uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		delete(r.records, id)
	}
	return nil
}
func (r *memoryRepository) FindByID(_ context.Context, id uuid.UUID) (Media, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.records[id]
	if !ok {
		return Media{}, ErrNotFound
	}
	return record, nil
}
func (r *memoryRepository) FindArchivedLocation(context.Context, uuid.UUID, uuid.UUID) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return "", ErrNotFound
}
func (r *memoryRepository) FindStale(_ context.Context, before time.Time, _ int) ([]Media, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
func (s memoryStorage) DeleteMany(context.Context, []string) ([]string, error) {
	return nil, nil
}
func (s memoryStorage) OpenRead(context.Context, string) (io.ReadCloser, ObjectInfo, error) {
	return io.NopCloser(bytes.NewReader(nil)), s.head, nil
}
func (s memoryStorage) Put(context.Context, string, string, io.Reader) error { return nil }

type allowAllLimiter struct{}

func (allowAllLimiter) Allow(context.Context, string, int, time.Duration) (bool, error) {
	return true, nil
}

func TestServiceCreatesAndCompletesWithoutInfrastructure(t *testing.T) {
	repo := newMemoryRepository()
	service := NewService(repo, memoryStorage{head: ObjectInfo{Size: 42, ContentType: "image/jpeg"}}, allowAllLimiter{}, time.Minute, time.Minute, 1, 100, 100)
	scope := UploadScope{EventID: uuid.New(), GuestSessionID: uuid.New(), Accepting: true, MaxEventBytes: 1_000}

	input := CreateInput{Filename: "guest.jpg", MIMEType: "image/jpeg", Size: 42, ChecksumSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", ClientUploadID: uuid.New(), GuestName: " Linh ", Caption: " A joyful moment "}
	target, err := service.CreateUpload(context.Background(), scope, input)
	require.NoError(t, err)
	require.NotEmpty(t, target.UploadURL)

	require.NoError(t, service.Complete(context.Background(), scope, target.MediaID))
	require.NoError(t, service.Complete(context.Background(), scope, target.MediaID), "complete must survive a lost success response")
	record, err := repo.FindReady(context.Background(), scope.EventID, target.MediaID)
	require.NoError(t, err)
	require.Equal(t, StatusReady, record.Status)
	require.EqualValues(t, 42, *record.ActualSize)
	require.Equal(t, "Linh", *record.GuestName)
	require.Equal(t, "A joyful moment", *record.Caption)
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

// recordingNotifier captures broadcasts so tests can assert both that a
// durable change is announced and that a failed one is not.
type recordingNotifier struct {
	mu      sync.Mutex
	changes []Change
}

func (n *recordingNotifier) MediaChanged(_ context.Context, change Change) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.changes = append(n.changes, change)
}

func (n *recordingNotifier) all() []Change {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Change(nil), n.changes...)
}

func notifyingService(repo Repository, notifier Notifier) *Service {
	return NewService(repo, memoryStorage{head: ObjectInfo{Size: 42, ContentType: "image/jpeg"}}, allowAllLimiter{}, time.Minute, time.Minute, 10, 100, 100, notifier)
}

func readyUpload(t *testing.T, service *Service, scope UploadScope, checksum string) uuid.UUID {
	t.Helper()
	target, err := service.CreateUpload(context.Background(), scope, CreateInput{
		Filename: "guest.jpg", MIMEType: "image/jpeg", Size: 42, ChecksumSHA256: checksum, ClientUploadID: uuid.New(),
	})
	require.NoError(t, err)
	require.NoError(t, service.Complete(context.Background(), scope, target.MediaID))
	return target.MediaID
}

func TestCompleteAnnouncesReadyMediaExactlyOnce(t *testing.T) {
	notifier := &recordingNotifier{}
	repo := newMemoryRepository()
	service := notifyingService(repo, notifier)
	scope := UploadScope{EventID: uuid.New(), GuestSessionID: uuid.New(), Accepting: true, MaxEventBytes: 1_000}

	mediaID := readyUpload(t, service, scope, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	// A retried complete after a lost response must not broadcast twice, or
	// every gallery would insert the same photo again.
	require.NoError(t, service.Complete(context.Background(), scope, mediaID))

	changes := notifier.all()
	require.Len(t, changes, 1)
	require.Equal(t, ChangeCreated, changes[0].Kind)
	require.Equal(t, scope.EventID, changes[0].EventID)
	require.Equal(t, []uuid.UUID{mediaID}, changes[0].IDs)
	require.NotNil(t, changes[0].Item)
	require.Equal(t, StatusReady, changes[0].Item.Status, "a gallery must not be told to insert a pending record")
	require.False(t, changes[0].Item.IsVideo)
}

func TestModerationAndDeletionAreAnnounced(t *testing.T) {
	notifier := &recordingNotifier{}
	repo := newMemoryRepository()
	service := notifyingService(repo, notifier)
	scope := UploadScope{EventID: uuid.New(), GuestSessionID: uuid.New(), Accepting: true, MaxEventBytes: 10_000}

	first := readyUpload(t, service, scope, "1111111111111111111111111111111111111111111111111111111111111111")
	second := readyUpload(t, service, scope, "2222222222222222222222222222222222222222222222222222222222222222")

	_, err := service.UpdateStatus(context.Background(), scope.EventID, first, StatusHidden)
	require.NoError(t, err)
	require.NoError(t, service.UpdateStatuses(context.Background(), scope.EventID, []uuid.UUID{second}, StatusFeatured))
	require.NoError(t, service.DeleteForEvent(context.Background(), scope.EventID, first))
	require.NoError(t, service.DeleteManyForEvent(context.Background(), scope.EventID, []uuid.UUID{second}))

	changes := notifier.all()
	require.Len(t, changes, 6) // two uploads plus four moderation actions

	hide := changes[2]
	require.Equal(t, ChangeModerated, hide.Kind)
	require.Equal(t, StatusHidden, hide.Status)
	require.Equal(t, []uuid.UUID{first}, hide.IDs)

	feature := changes[3]
	require.Equal(t, ChangeModerated, feature.Kind)
	require.Equal(t, StatusFeatured, feature.Status)

	require.Equal(t, ChangeDeleted, changes[4].Kind)
	require.Equal(t, []uuid.UUID{first}, changes[4].IDs)
	require.Equal(t, ChangeDeleted, changes[5].Kind)
	require.Equal(t, []uuid.UUID{second}, changes[5].IDs)
}

func TestFailedChangesAreNotAnnounced(t *testing.T) {
	notifier := &recordingNotifier{}
	service := notifyingService(newMemoryRepository(), notifier)
	eventID := uuid.New()

	// Telling browsers about a change the database refused would leave every
	// gallery showing state that a refetch immediately contradicts.
	_, err := service.UpdateStatus(context.Background(), eventID, uuid.New(), StatusHidden)
	require.Error(t, err)
	_, err = service.UpdateStatus(context.Background(), eventID, uuid.New(), StatusPending)
	require.Error(t, err)
	require.Error(t, service.DeleteForEvent(context.Background(), eventID, uuid.New()))
	require.Error(t, service.DeleteManyForEvent(context.Background(), eventID, []uuid.UUID{uuid.New()}))
	require.Error(t, service.UpdateStatuses(context.Background(), eventID, nil, StatusHidden))

	require.Empty(t, notifier.all())
}

func TestServiceWorksWithoutANotifier(t *testing.T) {
	repo := newMemoryRepository()
	service := NewService(repo, memoryStorage{head: ObjectInfo{Size: 42, ContentType: "image/jpeg"}}, allowAllLimiter{}, time.Minute, time.Minute, 10, 100, 100)
	scope := UploadScope{EventID: uuid.New(), GuestSessionID: uuid.New(), Accepting: true, MaxEventBytes: 1_000}

	require.NotPanics(t, func() {
		mediaID := readyUpload(t, service, scope, "3333333333333333333333333333333333333333333333333333333333333333")
		require.NoError(t, service.DeleteForEvent(context.Background(), scope.EventID, mediaID))
	})
}

// Gallery totals aggregate every media row of an event and cannot change
// between the pages of one cursor walk. Recomputing them per page is a full
// scan bought for nothing, so only the first page pays for it.
func TestGalleryCountsAreComputedOnlyForTheFirstPage(t *testing.T) {
	repo := newMemoryRepository()
	eventID := uuid.New()
	now := time.Now().UTC()
	repo.gallery = []Media{
		{ID: uuid.New(), EventID: eventID, MIMEType: "image/jpeg", Status: StatusReady, CreatedAt: now},
		{ID: uuid.New(), EventID: eventID, MIMEType: "image/jpeg", Status: StatusReady, CreatedAt: now.Add(-time.Minute)},
	}
	service := NewService(repo, memoryStorage{}, allowAllLimiter{}, time.Minute, time.Minute, 10, 100, 100)
	route := func(id uuid.UUID) string { return "/media/" + id.String() }

	first, err := service.ListGallery(context.Background(), eventID, FilterAll, SortNewest, 1, "", route)
	require.NoError(t, err)
	require.True(t, first.HasMore)
	require.NotEmpty(t, first.NextCursor)
	require.NotNil(t, first.Counts, "the first page carries the totals")
	require.EqualValues(t, 2, first.Counts.All)
	require.Equal(t, 1, repo.countCalls)

	next, err := service.ListGallery(context.Background(), eventID, FilterAll, SortNewest, 1, first.NextCursor, route)
	require.NoError(t, err)
	require.Len(t, next.Data, 1)
	require.Nil(t, next.Counts, "a cursor page must not recompute the totals")
	require.Equal(t, 1, repo.countCalls)
}

// thumbnailStorage serves a real image body so the decode path is exercised,
// and records what was written back.
type thumbnailStorage struct {
	mu      sync.Mutex
	object  []byte
	readErr error
	putErr  error
	puts    map[string][]byte
}

func newThumbnailStorage(object []byte) *thumbnailStorage {
	return &thumbnailStorage{object: object, puts: map[string][]byte{}}
}

func (s *thumbnailStorage) PresignPut(context.Context, string, string, time.Duration) (string, error) {
	return "https://upload.example", nil
}
func (s *thumbnailStorage) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "https://download.example", nil
}
func (s *thumbnailStorage) Head(context.Context, string) (ObjectInfo, error) {
	return ObjectInfo{Size: int64(len(s.object)), ContentType: "image/png"}, nil
}
func (s *thumbnailStorage) Delete(context.Context, string) error { return nil }
func (s *thumbnailStorage) DeleteMany(context.Context, []string) ([]string, error) {
	return nil, nil
}
func (s *thumbnailStorage) OpenRead(context.Context, string) (io.ReadCloser, ObjectInfo, error) {
	if s.readErr != nil {
		return nil, ObjectInfo{}, s.readErr
	}
	return io.NopCloser(bytes.NewReader(s.object)), ObjectInfo{Size: int64(len(s.object))}, nil
}
func (s *thumbnailStorage) Put(_ context.Context, key, _ string, body io.Reader) error {
	if s.putErr != nil {
		return s.putErr
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts[key] = raw
	return nil
}
func (s *thumbnailStorage) putCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.puts)
}

func encodePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// readyImage seeds a repository with one ready image and returns its id.
func readyImage(repo *memoryRepository, eventID uuid.UUID) uuid.UUID {
	id := uuid.New()
	repo.records[id] = Media{
		ID: id, EventID: eventID, MIMEType: "image/png", Status: StatusReady,
		ObjectKey: "events/x/media/y/original.png", CreatedAt: time.Now().UTC(),
	}
	repo.ready[id] = true
	return id
}

func TestGenerateThumbnailStoresVariantAndAnnouncesIt(t *testing.T) {
	repo := newMemoryRepository()
	eventID := uuid.New()
	id := readyImage(repo, eventID)
	storage := newThumbnailStorage(encodePNG(t, 1600, 900))
	notifier := &recordingNotifier{}
	service := NewService(repo, storage, allowAllLimiter{}, time.Minute, time.Minute, 10, 100, 100, notifier)

	require.NoError(t, service.GenerateThumbnail(context.Background(), id))

	require.Equal(t, 1, storage.putCount())
	require.True(t, repo.records[id].ThumbnailReady)
	require.Len(t, notifier.changes, 1)
	require.Equal(t, ChangeThumbnail, notifier.changes[0].Kind)

	// Running the same job twice must not redo the work: the queue guarantees
	// at-least-once delivery, not exactly-once.
	require.NoError(t, service.GenerateThumbnail(context.Background(), id))
	require.Equal(t, 1, storage.putCount())
}

func TestGenerateThumbnailDoesNotRetryPermanentConditions(t *testing.T) {
	eventID := uuid.New()
	t.Run("media deleted before the job ran", func(t *testing.T) {
		repo := newMemoryRepository()
		service := NewService(repo, newThumbnailStorage(nil), allowAllLimiter{}, time.Minute, time.Minute, 10, 100, 100)
		require.NoError(t, service.GenerateThumbnail(context.Background(), uuid.New()))
	})

	t.Run("format Go cannot decode", func(t *testing.T) {
		repo := newMemoryRepository()
		id := readyImage(repo, eventID)
		// Stands in for HEIC: accepted on upload, no decoder registered.
		storage := newThumbnailStorage([]byte("not an image"))
		service := NewService(repo, storage, allowAllLimiter{}, time.Minute, time.Minute, 10, 100, 100)
		require.NoError(t, service.GenerateThumbnail(context.Background(), id),
			"an undecodable body is permanent, so the job must not be retried")
		require.Equal(t, 0, storage.putCount())
		require.False(t, repo.records[id].ThumbnailReady)
	})
}

func TestGenerateThumbnailReportsTransientFailuresForRetry(t *testing.T) {
	eventID := uuid.New()
	t.Run("original unreadable", func(t *testing.T) {
		repo := newMemoryRepository()
		id := readyImage(repo, eventID)
		storage := newThumbnailStorage(nil)
		storage.readErr = errors.New("connection reset")
		service := NewService(repo, storage, allowAllLimiter{}, time.Minute, time.Minute, 10, 100, 100)
		require.Error(t, service.GenerateThumbnail(context.Background(), id))
	})

	t.Run("thumbnail could not be stored", func(t *testing.T) {
		repo := newMemoryRepository()
		id := readyImage(repo, eventID)
		storage := newThumbnailStorage(encodePNG(t, 64, 64))
		storage.putErr = errors.New("storage unavailable")
		service := NewService(repo, storage, allowAllLimiter{}, time.Minute, time.Minute, 10, 100, 100)
		require.Error(t, service.GenerateThumbnail(context.Background(), id))
		require.False(t, repo.records[id].ThumbnailReady)
	})
}

// countingStorage records how many presign calls a page costs.
type countingStorage struct {
	*thumbnailStorage
	presigns int
}

func (s *countingStorage) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	s.presigns++
	return "https://r2.example/" + key, nil
}

func galleryService(repo *memoryRepository, storage Storage) *Service {
	return NewService(repo, storage, allowAllLimiter{}, time.Minute, time.Minute, 10, 100, 100)
}

// Signing a whole page is what removes the per-image round trip: without it
// every photo costs the client a request that the API answers with two queries
// and a redirect.
func TestSignPageIssuesDirectLinksForEveryItem(t *testing.T) {
	repo := newMemoryRepository()
	eventID := uuid.New()
	now := time.Now().UTC()
	repo.gallery = []Media{
		{ID: uuid.New(), EventID: eventID, MIMEType: "image/jpeg", Status: StatusReady, ObjectKey: "o1", ThumbnailReady: true, CreatedAt: now},
		{ID: uuid.New(), EventID: eventID, MIMEType: "video/mp4", Status: StatusReady, ObjectKey: "o2", CreatedAt: now.Add(-time.Minute)},
	}
	storage := &countingStorage{thumbnailStorage: newThumbnailStorage(nil)}
	service := galleryService(repo, storage)

	page, err := service.ListGallery(context.Background(), eventID, FilterAll, SortNewest, 10, "",
		func(id uuid.UUID) string { return "/route/" + id.String() })
	require.NoError(t, err)
	require.NoError(t, service.SignPage(context.Background(), page.Data, time.Hour))

	require.Len(t, page.Data, 2)
	for _, item := range page.Data {
		require.Contains(t, item.URL, "https://r2.example/", "the item must not send the client back to the API")
	}
	// The image has a rendered thumbnail; the video does not.
	require.NotEmpty(t, page.Data[0].ThumbnailURL)
	require.Empty(t, page.Data[1].ThumbnailURL)
	require.Equal(t, 3, storage.presigns, "two originals and one thumbnail")
}

// An archived original has no object left in hot storage, so signing its key
// would hand the browser a dead link. Its thumbnail is not archived and is
// still signed.
func TestSignPageKeepsTheApplicationRouteForArchivedOriginals(t *testing.T) {
	repo := newMemoryRepository()
	eventID := uuid.New()
	archivedAt := time.Now().UTC().Add(-48 * time.Hour)
	id := uuid.New()
	repo.gallery = []Media{{
		ID: id, EventID: eventID, MIMEType: "image/jpeg", Status: StatusReady,
		ObjectKey: "gone", ThumbnailReady: true, CreatedAt: archivedAt,
		SourceDeletedAt: &archivedAt,
	}}
	storage := &countingStorage{thumbnailStorage: newThumbnailStorage(nil)}
	service := galleryService(repo, storage)

	page, err := service.ListGallery(context.Background(), eventID, FilterAll, SortNewest, 10, "",
		func(id uuid.UUID) string { return "/route/" + id.String() })
	require.NoError(t, err)
	require.NoError(t, service.SignPage(context.Background(), page.Data, time.Hour))

	require.Equal(t, "/route/"+id.String(), page.Data[0].URL,
		"an archived original must be streamed by the application, not signed")
	require.Contains(t, page.Data[0].ThumbnailURL, "https://r2.example/",
		"thumbnails are not archived and stay directly fetchable")
	require.Equal(t, 1, storage.presigns)
}

// failingDeleteStorage removes every key of a batch except one, the way a
// provider reports per-object errors inside an otherwise successful request.
type failingDeleteStorage struct {
	*thumbnailStorage
	poison  string
	deleted []string
	calls   int
}

func (s *failingDeleteStorage) DeleteMany(_ context.Context, keys []string) ([]string, error) {
	s.calls++
	var failed []string
	for _, key := range keys {
		if key == s.poison {
			failed = append(failed, key)
			continue
		}
		s.deleted = append(s.deleted, key)
	}
	if len(failed) > 0 {
		return failed, errors.New("object is locked")
	}
	return nil, nil
}

// A single stubborn record used to block the whole cleanup on every run,
// because FindStale returns the oldest first and the loop stopped on it. The
// quota those reservations held was never released.
//
// The record that could not be removed keeps its row on purpose: deleting it
// while its object survives would strand the object and lose the only handle
// on it.
func TestExpireStaleRemovesEveryItemItCan(t *testing.T) {
	repo := newMemoryRepository()
	eventID := uuid.New()
	old := time.Now().UTC().Add(-48 * time.Hour)
	poison, good := uuid.New(), uuid.New()
	repo.records[poison] = Media{ID: poison, EventID: eventID, ObjectKey: "stuck", Status: StatusPending, LastActivityAt: old}
	repo.records[good] = Media{ID: good, EventID: eventID, ObjectKey: "fine", Status: StatusPending, LastActivityAt: old}

	storage := &failingDeleteStorage{thumbnailStorage: newThumbnailStorage(nil), poison: "stuck"}
	service := galleryService(repo, storage)

	err := service.ExpireStale(context.Background(), time.Now().UTC(), 10)
	require.Error(t, err, "the run must report that something could not be cleaned up")
	require.Contains(t, err.Error(), "1 of 2")

	require.Equal(t, []string{"fine"}, storage.deleted)
	require.NotContains(t, repo.records, good, "the healthy record must still be expired")
	require.Contains(t, repo.records, poison, "a row whose object survives must be retried, not dropped")
}

// The whole point of batching: a hundred abandoned uploads must not cost a
// hundred provider round trips.
func TestExpireStaleDeletesObjectsInOneCall(t *testing.T) {
	repo := newMemoryRepository()
	eventID := uuid.New()
	old := time.Now().UTC().Add(-48 * time.Hour)
	for i := 0; i < 100; i++ {
		id := uuid.New()
		repo.records[id] = Media{ID: id, EventID: eventID, ObjectKey: fmt.Sprintf("k%d", i), Status: StatusPending, LastActivityAt: old}
	}
	storage := &failingDeleteStorage{thumbnailStorage: newThumbnailStorage(nil)}
	service := galleryService(repo, storage)

	require.NoError(t, service.ExpireStale(context.Background(), time.Now().UTC(), 200))
	require.Equal(t, 1, storage.calls, "one hundred objects, one provider request")
	require.Len(t, storage.deleted, 100)
	require.Empty(t, repo.records)
}

// slowHeadStorage measures how much of a batch overlapped.
type slowHeadStorage struct {
	memoryStorage
	mu      sync.Mutex
	inFlate int
	peak    int
	delay   time.Duration
	failFor map[string]bool
}

func (s *slowHeadStorage) Head(_ context.Context, key string) (ObjectInfo, error) {
	s.mu.Lock()
	s.inFlate++
	if s.inFlate > s.peak {
		s.peak = s.inFlate
	}
	s.mu.Unlock()
	time.Sleep(s.delay)
	s.mu.Lock()
	s.inFlate--
	shouldFail := s.failFor[key]
	s.mu.Unlock()

	if shouldFail {
		return ObjectInfo{}, errors.New("object not found")
	}
	return s.memoryStorage.head, nil
}

// reserveUploads creates n pending records and returns their ids.
func reserveUploads(t *testing.T, service *Service, scope UploadScope, n int) []uuid.UUID {
	t.Helper()
	ids := make([]uuid.UUID, 0, n)
	for i := 0; i < n; i++ {
		target, err := service.CreateUpload(context.Background(), scope, CreateInput{
			Filename: fmt.Sprintf("guest%d.jpg", i), MIMEType: "image/jpeg", Size: 42,
			ChecksumSHA256: fmt.Sprintf("%064x", i+1), ClientUploadID: uuid.New(),
		})
		require.NoError(t, err)
		ids = append(ids, target.MediaID)
	}
	return ids
}

// Confirming one photo at a time costs a guest one sequential round trip per
// photo on venue wifi, which is the product's hardest constraint.
func TestCompleteManyConfirmsUploadsConcurrently(t *testing.T) {
	repo := newMemoryRepository()
	storage := &slowHeadStorage{
		memoryStorage: memoryStorage{head: ObjectInfo{Size: 42, ContentType: "image/jpeg"}},
		delay:         20 * time.Millisecond,
		failFor:       map[string]bool{},
	}
	service := NewService(repo, storage, allowAllLimiter{}, time.Minute, time.Minute, 100, 100, 100)
	scope := UploadScope{EventID: uuid.New(), GuestSessionID: uuid.New(), Accepting: true, MaxEventBytes: 100_000}
	ids := reserveUploads(t, service, scope, 12)

	started := time.Now()
	results, err := service.CompleteMany(context.Background(), scope, ids)
	elapsed := time.Since(started)

	require.NoError(t, err)
	require.Len(t, results, 12)
	for _, result := range results {
		require.Equal(t, "ready", result.Status, result.Error)
	}
	require.Greater(t, storage.peak, 1, "the batch must overlap rather than run one at a time")
	require.LessOrEqual(t, storage.peak, completeConcurrency, "the pool ceiling must hold")
	// Sequential would be 12 x 20ms; overlapping cuts it to a few waves.
	require.Less(t, elapsed, 200*time.Millisecond)
}

// A guest with thirty good photos and one bad upload must keep the thirty.
func TestCompleteManyReportsEachItemSeparately(t *testing.T) {
	repo := newMemoryRepository()
	storage := &slowHeadStorage{
		memoryStorage: memoryStorage{head: ObjectInfo{Size: 42, ContentType: "image/jpeg"}},
		failFor:       map[string]bool{},
	}
	service := NewService(repo, storage, allowAllLimiter{}, time.Minute, time.Minute, 100, 100, 100)
	scope := UploadScope{EventID: uuid.New(), GuestSessionID: uuid.New(), Accepting: true, MaxEventBytes: 100_000}
	ids := reserveUploads(t, service, scope, 4)

	// The second photo never finished uploading to storage.
	broken, err := repo.FindByID(context.Background(), ids[1])
	require.NoError(t, err)
	storage.failFor[broken.ObjectKey] = true

	results, err := service.CompleteMany(context.Background(), scope, ids)
	require.NoError(t, err)
	require.Len(t, results, 4)

	// Results come back in the order asked for, so a client can match them up.
	for i, result := range results {
		require.Equal(t, ids[i], result.MediaID)
	}
	require.Equal(t, "failed", results[1].Status)
	require.NotEmpty(t, results[1].Error)
	for _, i := range []int{0, 2, 3} {
		require.Equal(t, "ready", results[i].Status, results[i].Error)
		record, findErr := repo.FindReady(context.Background(), scope.EventID, ids[i])
		require.NoError(t, findErr)
		require.Equal(t, StatusReady, record.Status)
	}
}

func TestCompleteManyRejectsUnusableBatches(t *testing.T) {
	repo := newMemoryRepository()
	service := NewService(repo, memoryStorage{head: ObjectInfo{Size: 42, ContentType: "image/jpeg"}},
		allowAllLimiter{}, time.Minute, time.Minute, 100, 100, 100)
	scope := UploadScope{EventID: uuid.New(), GuestSessionID: uuid.New(), Accepting: true, MaxEventBytes: 100_000}

	_, err := service.CompleteMany(context.Background(), scope, nil)
	require.Error(t, err, "an empty batch is a client mistake, not a no-op")

	oversized := make([]uuid.UUID, MaxCompleteBatch+1)
	for i := range oversized {
		oversized[i] = uuid.New()
	}
	_, err = service.CompleteMany(context.Background(), scope, oversized)
	require.Error(t, err)

	closed := scope
	closed.Accepting = false
	_, err = service.CompleteMany(context.Background(), closed, []uuid.UUID{uuid.New()})
	require.Error(t, err, "a closed event must not accept confirmations")
}

// Duplicate ids in one batch must be confirmed once, not raced against
// themselves.
func TestCompleteManyDeduplicatesIDs(t *testing.T) {
	repo := newMemoryRepository()
	service := NewService(repo, memoryStorage{head: ObjectInfo{Size: 42, ContentType: "image/jpeg"}},
		allowAllLimiter{}, time.Minute, time.Minute, 100, 100, 100)
	scope := UploadScope{EventID: uuid.New(), GuestSessionID: uuid.New(), Accepting: true, MaxEventBytes: 100_000}
	ids := reserveUploads(t, service, scope, 1)

	results, err := service.CompleteMany(context.Background(), scope, []uuid.UUID{ids[0], ids[0], ids[0]})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "ready", results[0].Status)
}
