package guest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type inMemoryGuestRepo struct {
	mu       sync.RWMutex
	sessions map[uuid.UUID]Session
}

func newInMemoryGuestRepo() *inMemoryGuestRepo {
	return &inMemoryGuestRepo{sessions: make(map[uuid.UUID]Session)}
}

func (r *inMemoryGuestRepo) Create(ctx context.Context, session *Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if session.ID == uuid.Nil {
		session.ID = uuid.New()
	}
	r.sessions[session.ID] = *session
	return nil
}

func (r *inMemoryGuestRepo) FindByToken(ctx context.Context, eventID uuid.UUID, tokenHash []byte, now time.Time) (Session, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.sessions {
		if s.EventID == eventID && bytes.Equal(s.TokenHash, tokenHash) && s.ExpiresAt.After(now) {
			return s, nil
		}
	}
	return Session{}, fmt.Errorf("session not found")
}

func TestGuestSessionLifecycle(t *testing.T) {
	repo := newInMemoryGuestRepo()
	ttl := 2 * time.Hour
	svc := NewService(repo, ttl)
	ctx := context.Background()

	eventID := uuid.New()

	// 1. Create a guest session
	session, token, err := svc.Create(ctx, eventID)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, session.ID)
	require.Equal(t, eventID, session.EventID)
	require.NotEmpty(t, token)

	// Verify SHA-256 hash
	expectedHash := sha256.Sum256([]byte(token))
	require.Equal(t, expectedHash[:], session.TokenHash)

	// 2. Validate with correct token and eventID
	validated, err := svc.Validate(ctx, eventID, token)
	require.NoError(t, err)
	require.Equal(t, session.ID, validated.ID)
	require.Equal(t, eventID, validated.EventID)

	// 3. Validate with incorrect token fails
	_, err = svc.Validate(ctx, eventID, "invalid-token-xyz")
	require.Error(t, err)

	// 4. Validate with incorrect eventID fails
	wrongEventID := uuid.New()
	_, err = svc.Validate(ctx, wrongEventID, token)
	require.Error(t, err)

	// 5. Expired session fails
	expiredRepo := newInMemoryGuestRepo()
	expiredSvc := NewService(expiredRepo, -1*time.Minute)
	_, expiredToken, err := expiredSvc.Create(ctx, eventID)
	require.NoError(t, err)

	_, err = expiredSvc.Validate(ctx, eventID, expiredToken)
	require.Error(t, err)
}
