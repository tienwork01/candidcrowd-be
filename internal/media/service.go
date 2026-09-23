package media

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

type ObjectInfo struct {
	Size        int64
	ContentType string
}

type Storage interface {
	PresignPut(ctx context.Context, key, mime string, expiry time.Duration) (string, error)
	PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error)
	Head(ctx context.Context, key string) (ObjectInfo, error)
	Delete(ctx context.Context, key string) error
}

type Limiter interface {
	Allow(context.Context, string, int, time.Duration) (bool, error)
}

type Service struct {
	repo               Repository
	storage            Storage
	limiter            Limiter
	expiry, window     time.Duration
	rate               int
	maxImage, maxVideo int64
}

func NewService(repo Repository, storage Storage, limiter Limiter, expiry, window time.Duration, rate int, maxImage, maxVideo int64) *Service {
	return &Service{repo, storage, limiter, expiry, window, rate, maxImage, maxVideo}
}

type CreateInput struct {
	Filename, MIMEType string
	Size               int64
	ChecksumSHA256     string
	SessionToken       string
	ClientUploadID     uuid.UUID
}
type UploadTarget struct {
	MediaID         uuid.UUID         `json:"media_id"`
	UploadURL       string            `json:"upload_url"`
	ExpiresAt       time.Time         `json:"expires_at"`
	RequiredHeaders map[string]string `json:"required_headers"`
}

type UploadScope struct {
	EventID        uuid.UUID
	GuestSessionID uuid.UUID
	Accepting      bool
	MaxEventBytes  int64
}

func (s *Service) CreateUpload(ctx context.Context, scope UploadScope, in CreateInput) (UploadTarget, error) {
	if !scope.Accepting {
		return UploadTarget{}, fmt.Errorf("event is not accepting uploads")
	}
	if in.Size <= 0 || !allowed(in.MIMEType) || in.Size > limitByMIME(in.MIMEType, s.maxImage, s.maxVideo) || !validSHA256(in.ChecksumSHA256) {
		return UploadTarget{}, fmt.Errorf("invalid media type or file size")
	}
	if in.ClientUploadID == uuid.Nil {
		return UploadTarget{}, fmt.Errorf("client upload id is required")
	}
	// A browser can lose the response after the reservation committed. Reuse the
	// same record/object key on the next request rather than reserving quota again.
	m, existingErr := s.repo.FindByClientUpload(ctx, scope.EventID, scope.GuestSessionID, in.ClientUploadID)
	created := false
	if existingErr != nil && !errors.Is(existingErr, ErrNotFound) {
		return UploadTarget{}, existingErr
	}
	if errors.Is(existingErr, ErrNotFound) {
		duplicate, err := s.repo.HasReadyChecksum(ctx, scope.EventID, in.ChecksumSHA256)
		if err != nil {
			return UploadTarget{}, err
		}
		if duplicate {
			return UploadTarget{}, ErrDuplicate
		}
		ok, err := s.limiter.Allow(ctx, "upload:"+scope.GuestSessionID.String(), s.rate, s.window)
		if err != nil {
			return UploadTarget{}, err
		}
		if !ok {
			return UploadTarget{}, fmt.Errorf("upload rate limit exceeded")
		}
		clientID := in.ClientUploadID
		m = Media{ID: uuid.New(), EventID: scope.EventID, GuestSessionID: scope.GuestSessionID, OriginalFilename: filepath.Base(in.Filename), MIMEType: in.MIMEType, ExpectedSize: in.Size, ChecksumSHA256: strings.ToLower(in.ChecksumSHA256), ClientUploadID: &clientID, Status: StatusPending, LastActivityAt: time.Now()}
		m.ObjectKey = fmt.Sprintf("events/%s/media/%s/original%s", scope.EventID, m.ID, extension(in.MIMEType))
		err = s.repo.ReserveUpload(ctx, m, scope.MaxEventBytes)
		if err != nil {
			// A concurrent duplicate request may have won the unique client ID race.
			if existing, findErr := s.repo.FindByClientUpload(ctx, scope.EventID, scope.GuestSessionID, in.ClientUploadID); findErr == nil {
				m = existing
			} else {
				return UploadTarget{}, err
			}
		} else {
			created = true
		}
	}
	url, err := s.storage.PresignPut(ctx, m.ObjectKey, m.MIMEType, s.expiry)
	if err != nil {
		if created {
			_ = s.repo.Delete(ctx, m.ID)
		}
		return UploadTarget{}, err
	}
	return UploadTarget{
		MediaID:         m.ID,
		UploadURL:       url,
		ExpiresAt:       time.Now().Add(s.expiry),
		RequiredHeaders: map[string]string{"Content-Type": m.MIMEType},
	}, nil
}

func (s *Service) Complete(ctx context.Context, scope UploadScope, mediaID uuid.UUID) error {
	if !scope.Accepting {
		return fmt.Errorf("event is not accepting uploads")
	}
	m, err := s.repo.FindUploadForSession(ctx, mediaID, scope.EventID, scope.GuestSessionID)
	if err != nil {
		return err
	}
	if m.Status == StatusReady {
		return nil
	}
	obj, err := s.storage.Head(ctx, m.ObjectKey)
	if err != nil {
		return fmt.Errorf("uploaded object is unavailable: %w", err)
	}
	if obj.Size != m.ExpectedSize || obj.ContentType != m.MIMEType {
		return fmt.Errorf("uploaded object metadata does not match requested upload")
	}
	err = s.repo.MarkReady(ctx, scope.EventID, m.ID, obj.Size, time.Now())
	if errors.Is(err, ErrNotFound) {
		// Another complete request may have won the race. Treat it as success if
		// it made the same event/session-owned record ready.
		if current, findErr := s.repo.FindUploadForSession(ctx, mediaID, scope.EventID, scope.GuestSessionID); findErr == nil && current.Status == StatusReady {
			return nil
		}
	}
	if errors.Is(err, ErrDuplicate) {
		_ = s.storage.Delete(ctx, m.ObjectKey)
		_ = s.repo.Delete(ctx, m.ID)
	}
	return err
}

// ExpireStale removes abandoned reservations. A later retry always creates or
// reuses a client-idempotent upload record, so stale rows must not consume an
// event's upload quota forever.
func (s *Service) ExpireStale(ctx context.Context, before time.Time, limit int) error {
	items, err := s.repo.FindStale(ctx, before, limit)
	if err != nil {
		return err
	}
	for _, item := range items {
		if err := s.storage.Delete(ctx, item.ObjectKey); err != nil {
			return err
		}
		if err := s.repo.Delete(ctx, item.ID); err != nil {
			return err
		}
	}
	return nil
}

type CursorPage struct {
	Data       []PublicView
	NextCursor string
	HasMore    bool
}

func (s *Service) ListReady(ctx context.Context, eventID uuid.UUID, limit int, cursor string, urlFor func(uuid.UUID) string) (CursorPage, error) {
	if limit < 1 || limit > 100 {
		limit = 60
	}
	var before *Cursor
	if cursor != "" {
		var c struct {
			CreatedAt time.Time `json:"created_at"`
			ID        uuid.UUID `json:"id"`
		}
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil || json.Unmarshal(raw, &c) != nil {
			return CursorPage{}, fmt.Errorf("invalid cursor")
		}
		before = &Cursor{CreatedAt: c.CreatedAt, ID: c.ID}
	}
	records, err := s.repo.ListReady(ctx, eventID, limit+1, before)
	if err != nil {
		return CursorPage{}, err
	}
	hasMore := len(records) > limit
	if hasMore {
		records = records[:limit]
	}
	views := make([]PublicView, 0, len(records))
	for _, record := range records {
		views = append(views, PublicView{
			ID:        record.ID,
			URL:       urlFor(record.ID),
			MIMEType:  record.MIMEType,
			CreatedAt: record.CreatedAt,
			IsVideo:   strings.HasPrefix(record.MIMEType, "video/"),
		})
	}
	page := CursorPage{Data: views, HasMore: hasMore}
	if hasMore {
		last := records[len(records)-1]
		raw, _ := json.Marshal(struct {
			CreatedAt time.Time `json:"created_at"`
			ID        uuid.UUID `json:"id"`
		}{last.CreatedAt, last.ID})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}

func (s *Service) ReadURL(ctx context.Context, eventID, mediaID uuid.UUID, expiry time.Duration) (string, error) {
	record, err := s.repo.FindReady(ctx, eventID, mediaID)
	if err != nil {
		return "", err
	}
	return s.storage.PresignGet(ctx, record.ObjectKey, expiry)
}

// ArchivedLocation returns the opaque Google Drive reference for an archived
// original. It is intentionally never serialized to API clients.
func (s *Service) ArchivedLocation(ctx context.Context, eventID, mediaID uuid.UUID) (string, error) {
	return s.repo.FindArchivedLocation(ctx, eventID, mediaID)
}

var supportedMIMETypes = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"video/mp4":       ".mp4",
	"video/quicktime": ".mov",
}

func allowed(mimeType string) bool {
	_, ok := supportedMIMETypes[mimeType]
	return ok
}

func limitByMIME(mimeType string, maxImageBytes, maxVideoBytes int64) int64 {
	if strings.HasPrefix(mimeType, "image/") {
		return maxImageBytes
	}
	return maxVideoBytes
}

func extension(mimeType string) string {
	return supportedMIMETypes[mimeType]
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}
