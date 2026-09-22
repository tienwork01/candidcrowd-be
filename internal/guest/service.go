package guest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type Service struct {
	repo Repository
	ttl  time.Duration
}

func NewService(repo Repository, ttl time.Duration) *Service {
	return &Service{repo: repo, ttl: ttl}
}

func (s *Service) Create(ctx context.Context, eventID uuid.UUID) (Session, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Session{}, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	// Generate the primary key in the application rather than relying on the
	// database default. This keeps inserts portable and avoids a driver-specific
	// UUID scan from a RETURNING clause.
	session := Session{ID: uuid.New(), EventID: eventID, TokenHash: hash[:], ExpiresAt: time.Now().Add(s.ttl)}
	return session, token, s.repo.Create(ctx, &session)
}

func (s *Service) Validate(ctx context.Context, eventID uuid.UUID, token string) (Session, error) {
	hash := sha256.Sum256([]byte(token))
	session, err := s.repo.FindByToken(ctx, eventID, hash[:], time.Now())
	if err != nil {
		return Session{}, fmt.Errorf("invalid guest session: %w", err)
	}
	return session, nil
}
