package guest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

var ErrGuestLimitReached = errors.New("guest: event guest limit reached")

type Repository interface {
	Create(ctx context.Context, session *Session) error
	FindByToken(ctx context.Context, eventID uuid.UUID, tokenHash []byte, now time.Time) (Session, error)
}

type gormRepository struct {
	db *gorm.DB
}

func NewGormRepository(db *gorm.DB) Repository {
	return &gormRepository{db: db}
}

func (r *gormRepository) Create(ctx context.Context, session *Session) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Lock the event so simultaneous scans cannot both pass the same final
		// seat. A zero/missing max_guests means unlimited.
		var rows []struct{ MaxGuests int64 }
		if err := tx.Raw(`
			SELECT COALESCE((g.entitlement_snapshot->'limits'->>'max_guests')::bigint, 0) AS max_guests
			FROM events e
			JOIN event_plan_grants g ON g.event_id = e.id AND g.status = 'active'
			WHERE e.id = ? AND e.status = 'active'
			FOR UPDATE`, session.EventID).Scan(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("guest: event not available")
		}
		if rows[0].MaxGuests > 0 {
			var count int64
			if err := tx.Model(&Session{}).Where("event_id = ?", session.EventID).Count(&count).Error; err != nil {
				return err
			}
			if count >= rows[0].MaxGuests {
				return ErrGuestLimitReached
			}
		}
		return tx.Create(session).Error
	})
}

func (r *gormRepository) FindByToken(ctx context.Context, eventID uuid.UUID, tokenHash []byte, now time.Time) (Session, error) {
	var session Session
	err := r.db.WithContext(ctx).Where("event_id = ? AND token_hash = ? AND expires_at > ?", eventID, tokenHash, now).First(&session).Error
	if err != nil {
		return Session{}, fmt.Errorf("find guest session: %w", err)
	}
	return session, nil
}
