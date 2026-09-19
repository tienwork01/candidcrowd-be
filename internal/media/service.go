package media

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/guest"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
	db                 *gorm.DB
	storage            Storage
	limiter            Limiter
	expiry, window     time.Duration
	rate               int
	maxImage, maxVideo int64
}

func NewService(db *gorm.DB, storage Storage, limiter Limiter, expiry, window time.Duration, rate int, maxImage, maxVideo int64) *Service {
	return &Service{db, storage, limiter, expiry, window, rate, maxImage, maxVideo}
}

type CreateInput struct {
	Filename, MIMEType string
	Size               int64
	SessionToken       string
}
type UploadTarget struct {
	MediaID   uuid.UUID `json:"media_id"`
	UploadURL string    `json:"upload_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Service) CreateUpload(ctx context.Context, e event.Event, session guest.Session, in CreateInput) (UploadTarget, error) {
	if e.Status != event.StatusActive {
		return UploadTarget{}, fmt.Errorf("event is not accepting uploads")
	}
	if in.Size <= 0 || !allowed(in.MIMEType) || in.Size > limitByMIME(in.MIMEType, s.maxImage, s.maxVideo) {
		return UploadTarget{}, fmt.Errorf("invalid media type or file size")
	}
	ok, err := s.limiter.Allow(ctx, "upload:"+session.ID.String(), s.rate, s.window)
	if err != nil {
		return UploadTarget{}, err
	}
	if !ok {
		return UploadTarget{}, fmt.Errorf("upload rate limit exceeded")
	}
	m := Media{ID: uuid.New(), EventID: e.ID, GuestSessionID: session.ID, OriginalFilename: filepath.Base(in.Filename), MIMEType: in.MIMEType, ExpectedSize: in.Size, Status: StatusPending}
	m.ObjectKey = fmt.Sprintf("events/%s/media/%s/original%s", e.ID, m.ID, extension(in.MIMEType))
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var locked event.Event
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, "id = ?", e.ID).Error; err != nil {
			return err
		}
		var reserved int64
		if err := tx.Model(&Media{}).Where("event_id = ? AND status = ?", e.ID, StatusPending).Select("COALESCE(SUM(expected_size), 0)").Scan(&reserved).Error; err != nil {
			return err
		}
		if locked.UsedMediaBytes+reserved+in.Size > locked.MaxMediaBytes {
			return fmt.Errorf("event storage quota exceeded")
		}
		return tx.Create(&m).Error
	})
	if err != nil {
		return UploadTarget{}, err
	}
	url, err := s.storage.PresignPut(ctx, m.ObjectKey, m.MIMEType, s.expiry)
	if err != nil {
		_ = s.db.WithContext(ctx).Delete(&m).Error
		return UploadTarget{}, err
	}
	return UploadTarget{m.ID, url, time.Now().Add(s.expiry)}, nil
}

func (s *Service) Complete(ctx context.Context, e event.Event, session guest.Session, mediaID uuid.UUID) error {
	if e.Status != event.StatusActive {
		return fmt.Errorf("event is not accepting uploads")
	}
	var m Media
	if err := s.db.WithContext(ctx).Where("id = ? AND event_id = ? AND guest_session_id = ? AND status = ?", mediaID, e.ID, session.ID, StatusPending).First(&m).Error; err != nil {
		return err
	}
	obj, err := s.storage.Head(ctx, m.ObjectKey)
	if err != nil {
		return fmt.Errorf("uploaded object is unavailable: %w", err)
	}
	if obj.Size != m.ExpectedSize || obj.ContentType != m.MIMEType {
		return fmt.Errorf("uploaded object metadata does not match requested upload")
	}
	now := time.Now()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&Media{}).Where("id = ? AND status = ?", m.ID, StatusPending).Updates(map[string]any{"status": StatusReady, "actual_size": obj.Size, "uploaded_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("upload was already completed")
		}
		return tx.Model(&event.Event{}).Where("id = ?", e.ID).UpdateColumn("used_media_bytes", gorm.Expr("used_media_bytes + ?", obj.Size)).Error
	})
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
