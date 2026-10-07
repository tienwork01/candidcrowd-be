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

func (r *gormRepository) FindActive(ctx context.Context, eventID uuid.UUID) (Job, error) {
	var job Job
	err := r.db.WithContext(ctx).
		Where("event_id = ? AND status IN (?, ?)", eventID, StatusQueued, StatusProcessing).
		Order("created_at DESC").First(&job).Error
	return job, mapNotFound(err)
}

func (r *gormRepository) FindLatest(ctx context.Context, eventID uuid.UUID) (Job, error) {
	var job Job
	err := r.db.WithContext(ctx).
		Where("event_id = ?", eventID).
		Order("created_at DESC").First(&job).Error
	return job, mapNotFound(err)
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
		SELECT
			m.id,
			m.object_key,
			m.original_filename,
			m.source_deleted_at,
			r.location_reference AS drive_location
		FROM media m
		LEFT JOIN storage_replicas r ON r.media_id = m.id
			AND r.provider = 'google_drive'
			AND r.state = 'verified'
		WHERE m.event_id = ? AND m.status IN ('ready', 'featured', 'hidden')
		ORDER BY m.created_at ASC, m.id ASC`, eventID).Scan(&items).Error
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
