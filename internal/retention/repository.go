package retention

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type gormRepository struct{ db *gorm.DB }

func NewGormRepository(db *gorm.DB) Repository { return &gormRepository{db: db} }

func (r *gormRepository) Candidates(ctx context.Context, cutoff time.Time, limit int) ([]Candidate, error) {
	candidates := make([]Candidate, 0)
	err := r.db.WithContext(ctx).Raw(`
		SELECT g.event_id, pv.code AS plan_code, g.retention_expires_at
		FROM event_plan_grants g
		JOIN plan_versions pv ON pv.id = g.plan_version_id
		JOIN events e ON e.id = g.event_id
		WHERE g.status = 'active'
		  AND g.retention_expires_at <= ?
		  AND e.media_purged_at IS NULL
		ORDER BY g.retention_expires_at ASC
		LIMIT ?`, cutoff, limit).Scan(&candidates).Error
	return candidates, err
}

func (r *gormRepository) ArchivedReplicas(ctx context.Context, eventID uuid.UUID) ([]Replica, error) {
	replicas := make([]Replica, 0)
	err := r.db.WithContext(ctx).Raw(`
		SELECT r.media_id, r.location_reference AS location
		FROM storage_replicas r
		JOIN media m ON m.id = r.media_id
		WHERE m.event_id = ? AND r.provider = 'google_drive' AND r.state = 'verified'`, eventID).Scan(&replicas).Error
	return replicas, err
}

func (r *gormRepository) MarkReplicaDeleted(ctx context.Context, replica Replica, at time.Time) error {
	return r.db.WithContext(ctx).Exec(`
		UPDATE storage_replicas SET state = 'deleted', deleted_at = ?
		WHERE media_id = ? AND provider = 'google_drive' AND location_reference = ?`,
		at, replica.MediaID, replica.Location).Error
}

// MarkPurged records the outcome of a purge. Media rows are kept, as
// deleted, so the event's history and lifetime upload counters stay intact;
// only what pointed at stored bytes is cleared.
func (r *gormRepository) MarkPurged(ctx context.Context, eventID uuid.UUID, at time.Time) (Purged, error) {
	var purged Purged
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		media := tx.Exec(`
			UPDATE media
			SET status = 'deleted', source_deleted_at = COALESCE(source_deleted_at, ?), last_activity_at = ?
			WHERE event_id = ? AND status <> 'deleted'`, at, at, eventID)
		if media.Error != nil {
			return media.Error
		}
		purged.Media = media.RowsAffected
		exports := tx.Exec(`
			UPDATE event_exports
			SET status = 'failed', object_key = NULL,
			    error_message = 'removed at the end of the event storage period',
			    completed_at = COALESCE(completed_at, ?)
			WHERE event_id = ? AND (object_key IS NOT NULL OR status IN ('queued', 'processing'))`, at, eventID)
		if exports.Error != nil {
			return exports.Error
		}
		purged.Exports = exports.RowsAffected
		return tx.Exec(`
			UPDATE events
			SET status = CASE WHEN status = 'active' THEN 'closed'::event_status ELSE status END,
			    used_media_bytes = 0,
			    reserved_media_bytes = 0,
			    reserved_media_items = 0,
			    reserved_photo_items = 0,
			    reserved_video_items = 0,
			    media_purged_at = ?
			WHERE id = ?`, at, eventID).Error
	})
	return purged, err
}
