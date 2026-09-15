package guest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Service struct {
	db  *gorm.DB
	ttl time.Duration
}

func NewService(db *gorm.DB, ttl time.Duration) *Service { return &Service{db, ttl} }
func (s *Service) Create(ctx context.Context, eventID uuid.UUID) (Session, string, error) {
	raw := make([]byte, 32)
	if _, e := rand.Read(raw); e != nil {
		return Session{}, "", e
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	out := Session{EventID: eventID, TokenHash: hash[:], ExpiresAt: time.Now().Add(s.ttl)}
	return out, token, s.db.WithContext(ctx).Create(&out).Error
}
func (s *Service) Validate(ctx context.Context, eventID uuid.UUID, token string) (Session, error) {
	hash := sha256.Sum256([]byte(token))
	var out Session
	e := s.db.WithContext(ctx).Where("event_id = ? AND token_hash = ? AND expires_at > ?", eventID, hash[:], time.Now()).First(&out).Error
	if e != nil {
		return Session{}, fmt.Errorf("invalid guest session: %w", e)
	}
	return out, nil
}
