package database

import (
	"context"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/archive"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ArchiveRepository is the PostgreSQL adapter for archive replica state.
type ArchiveRepository struct{ db *gorm.DB }

func NewArchiveRepository(db *gorm.DB) *ArchiveRepository { return &ArchiveRepository{db: db} }

func (r *ArchiveRepository) ListForArchive(ctx context.Context, olderThan time.Time, limit int) ([]archive.Candidate, error) {
	var rows []archive.Candidate
	err := r.db.WithContext(ctx).Raw(`
		SELECT m.id, m.object_key, m.original_filename, m.mime_type
		FROM media m
		WHERE m.status = 'ready' AND m.uploaded_at <= ?
		  AND NOT EXISTS (
			SELECT 1 FROM storage_replicas replica
			WHERE replica.media_id = m.id AND replica.provider = 'google_drive' AND replica.state = 'verified'
		  )
		ORDER BY m.uploaded_at ASC
		LIMIT ?`, olderThan, limit).Scan(&rows).Error
	return rows, err
}

func (r *ArchiveRepository) ListSourceDeletionPending(ctx context.Context, limit int) ([]archive.Candidate, error) {
	var rows []archive.Candidate
	err := r.db.WithContext(ctx).Raw(`
		SELECT m.id, m.object_key, m.original_filename, m.mime_type
		FROM media m
		JOIN storage_replicas drive ON drive.media_id = m.id AND drive.provider = 'google_drive' AND drive.state = 'verified'
		WHERE NOT EXISTS (
			SELECT 1 FROM storage_replicas source
			WHERE source.media_id = m.id AND source.provider = 'r2' AND source.location_reference = m.object_key AND source.state = 'deleted'
		)
		ORDER BY drive.verified_at ASC
		LIMIT ?`, limit).Scan(&rows).Error
	return rows, err
}

func (r *ArchiveRepository) RecordVerified(ctx context.Context, mediaID uuid.UUID, location string, size int64, checksum string, verifiedAt time.Time) error {
	return r.db.WithContext(ctx).Exec(`
		INSERT INTO storage_replicas (media_id, provider, location_reference, state, size, checksum_sha256, verified_at)
		VALUES (?, 'google_drive', ?, 'verified', ?, ?, ?)
		ON CONFLICT (media_id, provider, location_reference)
		DO UPDATE SET state = 'verified', size = EXCLUDED.size, checksum_sha256 = EXCLUDED.checksum_sha256, verified_at = EXCLUDED.verified_at`,
		mediaID, location, size, checksum, verifiedAt).Error
}

func (r *ArchiveRepository) MarkSourceDeleted(ctx context.Context, mediaID uuid.UUID, objectKey string, deletedAt time.Time) error {
	return r.db.WithContext(ctx).Exec(`
		INSERT INTO storage_replicas (media_id, provider, location_reference, state, deleted_at)
		VALUES (?, 'r2', ?, 'deleted', ?)
		ON CONFLICT (media_id, provider, location_reference)
		DO UPDATE SET state = 'deleted', deleted_at = EXCLUDED.deleted_at`, mediaID, objectKey, deletedAt).Error
}
