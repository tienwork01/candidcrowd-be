package exportjob

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrNotFound = errors.New("export: not found")
var ErrNoQueuedJob = errors.New("export: no queued job")

type gormRepository struct{ db *gorm.DB }

func NewGormRepository(db *gorm.DB) Repository { return &gormRepository{db: db} }

func (r *gormRepository) Create(ctx context.Context, job Job) (Job, error) {
	err := r.db.WithContext(ctx).Create(&job).Error
	return job, err
}

func (r *gormRepository) FindOwned(ctx context.Context, eventID, jobID uuid.UUID) (Job, error) {
	var job Job
	err := r.db.WithContext(ctx).Where("id = ? AND event_id = ?", jobID, eventID).First(&job).Error
	return job, mapNotFound(err)
}

func (r *gormRepository) ClaimNext(ctx context.Context) (Job, error) {
	var job Job
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ?", StatusQueued).Order("created_at ASC").First(&job).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNoQueuedJob
		}
		if err != nil {
			return err
		}
		return tx.Model(&job).Updates(map[string]any{"status": StatusProcessing, "error_message": nil}).Error
	})
	return job, err
}

func (r *gormRepository) ListItems(ctx context.Context, eventID uuid.UUID) ([]Item, error) {
	var items []Item
	err := r.db.WithContext(ctx).Raw(`
		SELECT id, object_key, original_filename
		FROM media
		WHERE event_id = ? AND status IN ('ready', 'featured', 'hidden')
		ORDER BY created_at ASC, id ASC`, eventID).Scan(&items).Error
	return items, err
}

func (r *gormRepository) MarkReady(ctx context.Context, jobID uuid.UUID, objectKey string, completedAt time.Time) error {
	result := r.db.WithContext(ctx).Model(&Job{}).Where("id = ? AND status = ?", jobID, StatusProcessing).
		Updates(map[string]any{"status": StatusReady, "object_key": objectKey, "completed_at": completedAt, "error_message": nil})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrNotFound
	}
	return nil
}

func (r *gormRepository) MarkFailed(ctx context.Context, jobID uuid.UUID, message string, completedAt time.Time) error {
	result := r.db.WithContext(ctx).Model(&Job{}).Where("id = ? AND status = ?", jobID, StatusProcessing).
		Updates(map[string]any{"status": StatusFailed, "error_message": message, "completed_at": completedAt})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrNotFound
	}
	return nil
}

func mapNotFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}
