package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/candidcrowd/candidcrowd-backend/internal/mediajob"
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

// ReserveUpload takes the event's storage quota and creates the pending media
// record.
//
// The quota check is a single conditional UPDATE rather than a row lock held
// across an aggregate. Every guest at one event contends on the same event
// row, so how long that row is held is what decides whether 150 people can
// start uploading at once: the previous SELECT ... FOR UPDATE held it for a
// lock round trip plus a SUM() over the event's whole media history.
//
// The counter it maintains is released by MarkReady (upload succeeded),
// ExpireStale (upload abandoned) and Delete (target could not be handed out).
func (r *MediaRepository) ReserveUpload(ctx context.Context, record media.Media, maxEventBytes int64) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// A limit of 0 means "whatever the event allows"; NULLIF keeps that
		// decision in SQL so there is no read before the write.
		reserve := tx.Exec(`
			UPDATE events
			SET reserved_media_bytes = reserved_media_bytes + ?
			WHERE id = ?
			  AND status = ?
			  AND used_media_bytes + reserved_media_bytes + ? <= COALESCE(NULLIF(?, 0), max_media_bytes)`,
			record.ExpectedSize, record.EventID, event.StatusActive, record.ExpectedSize, maxEventBytes)
		if reserve.Error != nil {
			return reserve.Error
		}
		if reserve.RowsAffected != 1 {
			// Only the rejected path pays for a read, and only to tell the
			// guest which of the two reasons applies.
			return reservationRefusal(tx, record.EventID)
		}
		// A failure here rolls the reservation back with the transaction.
		return tx.Create(&record).Error
	})
}

// reservationRefusal explains why a reservation did not take. It runs only
// after the conditional UPDATE matched nothing, so it never costs an accepted
// upload anything.
func reservationRefusal(tx *gorm.DB, eventID uuid.UUID) error {
	var evt event.Event
	if err := tx.Select("status").First(&evt, "id = ?", eventID).Error; err != nil {
		return mapMediaNotFound(err)
	}
	if evt.Status != event.StatusActive {
		return fmt.Errorf("event is not accepting uploads")
	}
	return fmt.Errorf("event storage quota exceeded")
}

func (r *MediaRepository) FindByClientUpload(ctx context.Context, eventID, sessionID, clientUploadID uuid.UUID) (media.Media, error) {
	var record media.Media
	err := r.db.WithContext(ctx).Where("event_id = ? AND guest_session_id = ? AND client_upload_id = ?", eventID, sessionID, clientUploadID).First(&record).Error
	return record, mapMediaNotFound(err)
}

func (r *MediaRepository) HasReadyChecksum(ctx context.Context, eventID uuid.UUID, checksumSHA256 string) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).
		Model(&media.Media{}).
		Where("event_id = ? AND checksum_sha256 = ? AND status = ?", eventID, checksumSHA256, media.StatusReady).
		Count(&count).Error
	return count > 0, err
}

// Delete removes an upload record that never became ready: a target that
// could not be handed out, an abandoned reservation, or a duplicate rejected
// at completion. Removing the row also gives the event its reserved bytes
// back, in the same statement, so a lost upload cannot leak quota.
//
// A record that already counted towards used_media_bytes releases nothing
// here; moderation deletion owns that column.
func (r *MediaRepository) Delete(ctx context.Context, mediaID uuid.UUID) error {
	return r.DeleteManyUploads(ctx, []uuid.UUID{mediaID})
}

// DeleteManyUploads removes never-completed upload records and gives their
// reserved bytes back, in one statement.
//
// The releases are summed per event before the update, so several abandoned
// uploads belonging to the same event are subtracted once rather than the
// event row being visited repeatedly.
func (r *MediaRepository) DeleteManyUploads(ctx context.Context, mediaIDs []uuid.UUID) error {
	if len(mediaIDs) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Exec(`
		WITH removed AS (
			DELETE FROM media WHERE id IN ?
			RETURNING event_id, expected_size, status
		), released AS (
			SELECT event_id, SUM(expected_size) AS bytes
			FROM removed
			WHERE status IN ('pending', 'uploading', 'uploaded')
			GROUP BY event_id
		)
		UPDATE events e
		SET reserved_media_bytes = GREATEST(e.reserved_media_bytes - released.bytes, 0)
		FROM released
		WHERE e.id = released.event_id`, mediaIDs).Error
}

func (r *MediaRepository) UpdateStatus(ctx context.Context, eventID, mediaID uuid.UUID, status media.Status) (media.Media, error) {
	// RETURNING gives the moderated row back from the same statement, so the
	// realtime broadcast that follows can never describe a different version
	// than the one this call wrote.
	var record media.Media
	result := r.db.WithContext(ctx).Model(&record).
		Clauses(clause.Returning{}).
		Where("id = ? AND event_id = ? AND status IN ?", mediaID, eventID, []media.Status{media.StatusReady, media.StatusFeatured, media.StatusHidden}).
		Updates(map[string]any{"status": status, "last_activity_at": time.Now().UTC()})
	if result.Error != nil {
		return media.Media{}, result.Error
	}
	if result.RowsAffected != 1 {
		return media.Media{}, media.ErrNotFound
	}
	return record, nil
}

func (r *MediaRepository) UpdateStatuses(ctx context.Context, eventID uuid.UUID, mediaIDs []uuid.UUID, status media.Status) error {
	result := r.db.WithContext(ctx).Model(&media.Media{}).
		Where("event_id = ? AND id IN ? AND status IN ?", eventID, mediaIDs, []media.Status{media.StatusReady, media.StatusFeatured, media.StatusHidden}).
		Updates(map[string]any{"status": status, "last_activity_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != int64(len(mediaIDs)) {
		return media.ErrNotFound
	}
	return nil
}

func (r *MediaRepository) DeleteForEvent(ctx context.Context, eventID, mediaID uuid.UUID) error {
	return r.deleteForEvent(ctx, eventID, []uuid.UUID{mediaID})
}

func (r *MediaRepository) DeleteManyForEvent(ctx context.Context, eventID uuid.UUID, mediaIDs []uuid.UUID) error {
	return r.deleteForEvent(ctx, eventID, mediaIDs)
}

func (r *MediaRepository) deleteForEvent(ctx context.Context, eventID uuid.UUID, mediaIDs []uuid.UUID) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var bytesToRelease int64
		if err := tx.Model(&media.Media{}).
			Where("event_id = ? AND id IN ? AND status IN ?", eventID, mediaIDs, []media.Status{media.StatusReady, media.StatusFeatured, media.StatusHidden}).
			Select("COALESCE(SUM(actual_size), 0)").Scan(&bytesToRelease).Error; err != nil {
			return err
		}
		result := tx.Model(&media.Media{}).
			Where("event_id = ? AND id IN ? AND status IN ?", eventID, mediaIDs, []media.Status{media.StatusReady, media.StatusFeatured, media.StatusHidden}).
			Updates(map[string]any{"status": media.StatusDeleted, "last_activity_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != int64(len(mediaIDs)) {
			return media.ErrNotFound
		}
		return tx.Model(&event.Event{}).Where("id = ?", eventID).
			UpdateColumn("used_media_bytes", gorm.Expr("GREATEST(used_media_bytes - ?, 0)", bytesToRelease)).Error
	})
}

func (r *MediaRepository) FindUploadForSession(ctx context.Context, mediaID, eventID, sessionID uuid.UUID) (media.Media, error) {
	var record media.Media
	err := r.db.WithContext(ctx).
		Where("id = ? AND event_id = ? AND guest_session_id = ? AND status IN ?", mediaID, eventID, sessionID, []media.Status{media.StatusPending, media.StatusUploading, media.StatusUploaded, media.StatusReady}).
		First(&record).Error
	return record, mapMediaNotFound(err)
}

func (r *MediaRepository) MarkReady(ctx context.Context, eventID, mediaID uuid.UUID, actualSize int64, uploadedAt time.Time) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&media.Media{}).
			Where("id = ? AND event_id = ? AND status IN ?", mediaID, eventID, []media.Status{media.StatusPending, media.StatusUploading, media.StatusUploaded}).
			Updates(map[string]any{"status": media.StatusReady, "actual_size": actualSize, "uploaded_at": uploadedAt, "last_activity_at": uploadedAt})
		if result.Error != nil {
			if errors.Is(result.Error, gorm.ErrDuplicatedKey) {
				return media.ErrDuplicate
			}
			return result.Error
		}
		if result.RowsAffected != 1 {
			return media.ErrNotFound
		}
		// The reservation becomes usage. Both columns move in one statement so
		// the pair is never observed half-applied, and expected_size is read
		// from the row this transaction just updated rather than fetched in a
		// second round trip.
		if err := tx.Exec(`
			UPDATE events
			SET used_media_bytes = used_media_bytes + ?,
			    reserved_media_bytes = GREATEST(
			        reserved_media_bytes - COALESCE((SELECT expected_size FROM media WHERE id = ?), 0), 0)
			WHERE id = ?`, actualSize, mediaID, eventID).Error; err != nil {
			return err
		}
		// Deferred work is enqueued in the transaction that makes the record
		// durable, so a crash between the two is impossible: either the media
		// is ready and its thumbnail is owed, or neither happened. The unique
		// (media_id, kind) index makes a retried complete a no-op.
		return tx.Exec(`
			INSERT INTO media_jobs (media_id, kind)
			SELECT id, ? FROM media WHERE id = ? AND mime_type LIKE 'image/%'
			ON CONFLICT (media_id, kind) DO NOTHING`, mediajob.KindThumbnail, mediaID).Error
	})
}

func (r *MediaRepository) MarkThumbnailReady(ctx context.Context, eventID, mediaID uuid.UUID) error {
	// Featured and hidden are moderation states of an already-ready record. A
	// host curating the gallery while a thumbnail renders must not strand the
	// result.
	result := r.db.WithContext(ctx).Model(&media.Media{}).
		Where("id = ? AND event_id = ? AND status IN ?", mediaID, eventID,
			[]media.Status{media.StatusReady, media.StatusFeatured, media.StatusHidden}).
		Update("thumbnail_ready", true)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return media.ErrNotFound
	}
	return nil
}

func (r *MediaRepository) FindByID(ctx context.Context, mediaID uuid.UUID) (media.Media, error) {
	var record media.Media
	err := r.db.WithContext(ctx).Where("id = ?", mediaID).First(&record).Error
	return record, mapMediaNotFound(err)
}

func (r *MediaRepository) FindStale(ctx context.Context, before time.Time, limit int) ([]media.Media, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	var records []media.Media
	err := r.db.WithContext(ctx).
		Where("status IN ? AND last_activity_at < ?", []media.Status{media.StatusPending, media.StatusUploading, media.StatusUploaded}, before).
		Order("last_activity_at ASC").Limit(limit).Find(&records).Error
	return records, err
}

func (r *MediaRepository) ListReady(ctx context.Context, eventID uuid.UUID, limit int, before *media.Cursor) ([]media.Media, error) {
	q := r.db.WithContext(ctx).Where("event_id = ? AND status IN ?", eventID, []media.Status{media.StatusReady, media.StatusFeatured})
	if before != nil {
		q = q.Where("(created_at, id) < (?, ?)", before.CreatedAt, before.ID)
	}
	var records []media.Media
	err := q.Order("created_at DESC").Order("id DESC").Limit(limit).Find(&records).Error
	return records, err
}

func (r *MediaRepository) ListGallery(ctx context.Context, eventID uuid.UUID, filter media.GalleryFilter, sort media.GallerySort, limit int, before *media.Cursor) ([]media.Media, error) {
	q := r.db.WithContext(ctx).Where("event_id = ?", eventID)
	visible := []media.Status{media.StatusReady, media.StatusFeatured}
	switch filter {
	case media.FilterPhotos:
		q = q.Where("status IN ? AND mime_type LIKE ?", visible, "image/%")
	case media.FilterVideos:
		q = q.Where("status IN ? AND mime_type LIKE ?", visible, "video/%")
	case media.FilterFavorites:
		q = q.Where("status = ?", media.StatusFeatured)
	case media.FilterHidden:
		q = q.Where("status = ?", media.StatusHidden)
	default:
		q = q.Where("status IN ?", visible)
	}
	if before != nil {
		if sort == media.SortOldest {
			q = q.Where("(created_at, id) > (?, ?)", before.CreatedAt, before.ID)
		} else {
			q = q.Where("(created_at, id) < (?, ?)", before.CreatedAt, before.ID)
		}
	}
	if sort == media.SortOldest {
		q = q.Order("created_at ASC").Order("id ASC")
	} else {
		q = q.Order("created_at DESC").Order("id DESC")
	}
	var records []media.Media
	err := q.Limit(limit).Find(&records).Error
	return records, err
}

func (r *MediaRepository) GalleryCounts(ctx context.Context, eventID uuid.UUID) (media.GalleryCounts, error) {
	var counts media.GalleryCounts
	err := r.db.WithContext(ctx).Model(&media.Media{}).Where("event_id = ?", eventID).
		Select(`
			COUNT(*) FILTER (WHERE status IN ('ready', 'featured')) AS all,
			COUNT(*) FILTER (WHERE status IN ('ready', 'featured') AND mime_type LIKE 'image/%') AS photos,
			COUNT(*) FILTER (WHERE status IN ('ready', 'featured') AND mime_type LIKE 'video/%') AS videos,
			COUNT(*) FILTER (WHERE status = 'featured') AS favorites,
			COUNT(*) FILTER (WHERE status = 'hidden') AS hidden`).
		Scan(&counts).Error
	return counts, err
}

func (r *MediaRepository) FindReady(ctx context.Context, eventID, mediaID uuid.UUID) (media.Media, error) {
	var record media.Media
	err := r.db.WithContext(ctx).Where("id = ? AND event_id = ? AND status IN ?", mediaID, eventID, []media.Status{media.StatusReady, media.StatusFeatured}).First(&record).Error
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
