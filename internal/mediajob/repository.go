package mediajob

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type gormRepository struct{ db *gorm.DB }

func NewGormRepository(db *gorm.DB) Repository { return &gormRepository{db: db} }

// ClaimBatch takes due jobs with SKIP LOCKED so a second replica running its
// own worker picks up different rows instead of blocking on these.
func (r *gormRepository) ClaimBatch(ctx context.Context, limit int, now time.Time) ([]Job, error) {
	if limit < 1 {
		limit = 1
	}
	var jobs []Job
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ? AND run_after <= ?", StatusQueued, now).
			Order("run_after ASC").Limit(limit).Find(&jobs).Error; err != nil {
			return err
		}
		if len(jobs) == 0 {
			return ErrNoQueuedJob
		}
		ids := make([]uuid.UUID, 0, len(jobs))
		for i := range jobs {
			ids = append(ids, jobs[i].ID)
			// Keep the returned values consistent with what was just written,
			// so a caller never sees a stale attempt count.
			jobs[i].Status = StatusProcessing
			jobs[i].Attempts++
		}
		return tx.Model(&Job{}).Where("id IN ?", ids).
			Updates(map[string]any{
				"status":     StatusProcessing,
				"attempts":   gorm.Expr("attempts + 1"),
				"updated_at": now,
			}).Error
	})
	if err != nil {
		return nil, err
	}
	return jobs, nil
}

func (r *gormRepository) MarkDone(ctx context.Context, id uuid.UUID) error {
	return r.db.WithContext(ctx).Model(&Job{}).Where("id = ?", id).
		Updates(map[string]any{
			"status":     StatusDone,
			"last_error": nil,
			"updated_at": time.Now().UTC(),
		}).Error
}

func (r *gormRepository) Reschedule(ctx context.Context, id uuid.UUID, runAfter time.Time, cause string) error {
	return r.db.WithContext(ctx).Model(&Job{}).Where("id = ?", id).
		Updates(map[string]any{
			"status":     StatusQueued,
			"run_after":  runAfter,
			"last_error": truncate(cause),
			"updated_at": time.Now().UTC(),
		}).Error
}

func (r *gormRepository) MarkFailed(ctx context.Context, id uuid.UUID, cause string) error {
	return r.db.WithContext(ctx).Model(&Job{}).Where("id = ?", id).
		Updates(map[string]any{
			"status":     StatusFailed,
			"last_error": truncate(cause),
			"updated_at": time.Now().UTC(),
		}).Error
}

// truncate bounds what a failing dependency can write into the row. An error
// string is diagnostic, not a payload.
func truncate(cause string) string {
	const limit = 500
	if len(cause) > limit {
		return cause[:limit]
	}
	return cause
}
