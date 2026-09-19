package guest

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

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
	return r.db.WithContext(ctx).Create(session).Error
}

func (r *gormRepository) FindByToken(ctx context.Context, eventID uuid.UUID, tokenHash []byte, now time.Time) (Session, error) {
	var session Session
	err := r.db.WithContext(ctx).Where("event_id = ? AND token_hash = ? AND expires_at > ?", eventID, tokenHash, now).First(&session).Error
	if err != nil {
		return Session{}, fmt.Errorf("find guest session: %w", err)
	}
	return session, nil
}
