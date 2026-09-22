package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MediaRepository is the PostgreSQL adapter for the media persistence port.
// It owns SQL, ORM and row-locking concerns; media.Service does not.
type MediaRepository struct {
	db *gorm.DB
}

func NewMediaRepository(db *gorm.DB) *MediaRepository {
	return &MediaRepository{db: db}
}

func (r *MediaRepository) ReserveUpload(ctx context.Context, record media.Media, maxEventBytes int64) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var evt event.Event
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&evt, "id = ?", record.EventID).Error; err != nil {
			return mapMediaNotFound(err)
		}
		if evt.Status != event.StatusActive {
			return fmt.Errorf("event is not accepting uploads")
		}
		var reserved int64
		if err := tx.Model(&media.Media{}).
			Where("event_id = ? AND status IN ?", record.EventID, []media.Status{media.StatusPending, media.StatusUploading, media.StatusUploaded}).
			Select("COALESCE(SUM(expected_size), 0)").Scan(&reserved).Error; err != nil {
			return err
		}
		limit := maxEventBytes
		if limit <= 0 {
			limit = evt.MaxMediaBytes
		}
		if evt.UsedMediaBytes+reserved+record.ExpectedSize > limit {
			return fmt.Errorf("event storage quota exceeded")
		}
		return tx.Create(&record).Error
	})
}

func (r *MediaRepository) Delete(ctx context.Context, mediaID uuid.UUID) error {
	return r.db.WithContext(ctx).Delete(&media.Media{}, "id = ?", mediaID).Error
}

func (r *MediaRepository) FindUploadForSession(ctx context.Context, mediaID, eventID, sessionID uuid.UUID) (media.Media, error) {
	var record media.Media
	err := r.db.WithContext(ctx).
		Where("id = ? AND event_id = ? AND guest_session_id = ? AND status IN ?", mediaID, eventID, sessionID, []media.Status{media.StatusPending, media.StatusUploading, media.StatusUploaded}).
		First(&record).Error
	return record, mapMediaNotFound(err)
}

func (r *MediaRepository) MarkReady(ctx context.Context, eventID, mediaID uuid.UUID, actualSize int64, uploadedAt time.Time) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&media.Media{}).
			Where("id = ? AND event_id = ? AND status IN ?", mediaID, eventID, []media.Status{media.StatusPending, media.StatusUploading, media.StatusUploaded}).
			Updates(map[string]any{"status": media.StatusReady, "actual_size": actualSize, "uploaded_at": uploadedAt})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return media.ErrNotFound
		}
		return tx.Model(&event.Event{}).Where("id = ?", eventID).UpdateColumn("used_media_bytes", gorm.Expr("used_media_bytes + ?", actualSize)).Error
	})
}

func (r *MediaRepository) ListReady(ctx context.Context, eventID uuid.UUID, limit int, before *media.Cursor) ([]media.Media, error) {
	q := r.db.WithContext(ctx).Where("event_id = ? AND status = ?", eventID, media.StatusReady)
	if before != nil {
		q = q.Where("(created_at, id) < (?, ?)", before.CreatedAt, before.ID)
	}
	var records []media.Media
	err := q.Order("created_at DESC").Order("id DESC").Limit(limit).Find(&records).Error
	return records, err
}

func (r *MediaRepository) FindReady(ctx context.Context, eventID, mediaID uuid.UUID) (media.Media, error) {
	var record media.Media
	err := r.db.WithContext(ctx).Where("id = ? AND event_id = ? AND status = ?", mediaID, eventID, media.StatusReady).First(&record).Error
	return record, mapMediaNotFound(err)
}

func (r *MediaRepository) FindArchivedLocation(ctx context.Context, eventID, mediaID uuid.UUID) (string, error) {
	var location string
	err := r.db.WithContext(ctx).Raw(`SELECT r.location_reference FROM storage_replicas r JOIN media m ON m.id=r.media_id WHERE m.id=? AND m.event_id=? AND r.provider='google_drive' AND r.state='verified' ORDER BY r.verified_at DESC LIMIT 1`, mediaID, eventID).Scan(&location).Error
	if err != nil {
		return "", err
	}
	if location == "" {
		return "", media.ErrNotFound
	}
	return location, nil
}

func mapMediaNotFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return media.ErrNotFound
	}
	return err
}
