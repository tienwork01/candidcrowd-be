package media

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/guest"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/r2"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Limiter interface {
	Allow(context.Context, string, int, time.Duration) (bool, error)
}
type Service struct {
	db                 *gorm.DB
	storage            r2.Storage
	limiter            Limiter
	expiry, window     time.Duration
	rate               int
	maxImage, maxVideo int64
}

func NewService(db *gorm.DB, storage r2.Storage, limiter Limiter, expiry, window time.Duration, rate int, maxImage, maxVideo int64) *Service {
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
	if in.Size <= 0 || !allowed(in.MIMEType) || in.Size > limit(in.MIMEType, s.maxImage, s.maxVideo) {
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
		if e := tx.Set("gorm:query_option", "FOR UPDATE").First(&locked, "id = ?", e.ID).Error; e != nil {
			return e
		}
		var reserved int64
		if e := tx.Model(&Media{}).Where("event_id = ? AND status = ?", e.ID, StatusPending).Select("COALESCE(SUM(expected_size), 0)").Scan(&reserved).Error; e != nil {
			return e
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
func allowed(m string) bool { return strings.HasPrefix(m, "image/") || strings.HasPrefix(m, "video/") }
func limit(m string, i, v int64) int64 {
	if strings.HasPrefix(m, "image/") {
		return i
	}
	return v
}
func extension(m string) string {
	switch m {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "video/quicktime":
		return ".mov"
	default:
		return ""
	}
}
