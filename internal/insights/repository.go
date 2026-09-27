package insights

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type gormRepository struct{ db *gorm.DB }

func NewGormRepository(db *gorm.DB) Repository { return &gormRepository{db: db} }

func (r *gormRepository) CreateSource(ctx context.Context, source QRSource) (QRSource, error) {
	if err := r.db.WithContext(ctx).Create(&source).Error; err != nil {
		return QRSource{}, err
	}
	return source, nil
}

func (r *gormRepository) ListSources(ctx context.Context, eventID uuid.UUID) ([]QRSource, error) {
	var sources []QRSource
	err := r.db.WithContext(ctx).Where("event_id = ?", eventID).Order("created_at ASC").Find(&sources).Error
	return sources, err
}

func (r *gormRepository) UpdateSource(ctx context.Context, eventID, sourceID uuid.UUID, name string) (QRSource, error) {
	// RETURNING makes this one statement instead of an update followed by a
	// read of what it just wrote.
	var source QRSource
	result := r.db.WithContext(ctx).Model(&source).
		Clauses(clause.Returning{}).
		Where("id = ? AND event_id = ?", sourceID, eventID).
		Update("name", name)
	if result.Error != nil {
		return QRSource{}, result.Error
	}
	if result.RowsAffected != 1 {
		return QRSource{}, ErrNotFound
	}
	return source, nil
}

func (r *gormRepository) DeleteSource(ctx context.Context, eventID, sourceID uuid.UUID) error {
	result := r.db.WithContext(ctx).Where("id = ? AND event_id = ?", sourceID, eventID).Delete(&QRSource{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrNotFound
	}
	return nil
}

func (r *gormRepository) SourceExists(ctx context.Context, eventID uuid.UUID, code string) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&QRSource{}).Where("event_id = ? AND code = ?", eventID, code).Count(&count).Error
	return count > 0, err
}

func (r *gormRepository) Analytics(ctx context.Context, eventID uuid.UUID, expectedGuestCount int) (Analytics, error) {
	result := Analytics{ExpectedGuestCount: expectedGuestCount, Sources: []SourceMetric{}}
	if err := r.db.WithContext(ctx).Raw(`
		SELECT COUNT(*) AS scans
		FROM guest_sessions WHERE event_id = ?`, eventID).Scan(&result).Error; err != nil {
		return Analytics{}, err
	}
	if err := r.db.WithContext(ctx).Raw(`
		SELECT COUNT(DISTINCT guest_session_id) AS contributors,
		       COUNT(*) AS media,
		       COUNT(*) FILTER (WHERE mime_type LIKE 'image/%') AS photos,
		       COUNT(*) FILTER (WHERE mime_type LIKE 'video/%') AS videos
		FROM media
		WHERE event_id = ? AND status IN ('ready', 'featured', 'hidden')`, eventID).Scan(&result).Error; err != nil {
		return Analytics{}, err
	}
	// Uploads are aggregated per session before the join. Joining media
	// directly would fan the session rows out once per uploaded file, which
	// counts one guest who shared ten photos as ten scans.
	if err := r.db.WithContext(ctx).Raw(`
		WITH sessions AS (
			SELECT gs.id, gs.qr_source_code
			FROM guest_sessions gs
			WHERE gs.event_id = ?
		), uploads AS (
			SELECT m.guest_session_id, COUNT(*) AS media_count
			FROM media m
			WHERE m.event_id = ? AND m.status IN ('ready', 'featured', 'hidden')
			GROUP BY m.guest_session_id
		)
		SELECT COALESCE(s.code, 'direct') AS code,
		       COALESCE(s.name, 'Direct link') AS name,
		       COUNT(sess.id) AS scans,
		       COUNT(u.guest_session_id) AS contributors,
		       COALESCE(SUM(u.media_count), 0) AS uploads
		FROM sessions sess
		LEFT JOIN qr_sources s ON s.event_id = ? AND s.code = sess.qr_source_code
		LEFT JOIN uploads u ON u.guest_session_id = sess.id
		GROUP BY s.code, s.name
		ORDER BY scans DESC, name ASC`, eventID, eventID, eventID).Scan(&result.Sources).Error; err != nil {
		return Analytics{}, err
	}
	return result, nil
}

func mapNotFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}
