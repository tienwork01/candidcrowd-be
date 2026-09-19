package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	"github.com/candidcrowd/candidcrowd-backend/internal/user"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type inMemoryProfileRepo struct {
	mu       sync.RWMutex
	users    map[string]user.User
	consents []Consent
}

func newInMemoryProfileRepo() *inMemoryProfileRepo {
	return &inMemoryProfileRepo{
		users:    make(map[string]user.User),
		consents: make([]Consent, 0),
	}
}

func (r *inMemoryProfileRepo) EnsureUser(ctx context.Context, identity auth.Identity) (user.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.users[identity.BetterAuthUserID]
	now := time.Now().UTC()
	if !ok {
		u = user.User{
			ID:                  uuid.New(),
			BetterAuthUserID:    identity.BetterAuthUserID,
			Email:               identity.Email,
			DisplayName:         identity.Name,
			LastAuthenticatedAt: &now,
		}
		if identity.EmailVerified {
			u.EmailVerifiedAt = &now
		}
		r.users[identity.BetterAuthUserID] = u
		return u, nil
	}
	u.Email = identity.Email
	u.DisplayName = identity.Name
	u.LastAuthenticatedAt = &now
	if identity.EmailVerified {
		u.EmailVerifiedAt = &now
	}
	r.users[identity.BetterAuthUserID] = u
	return u, nil
}

func (r *inMemoryProfileRepo) SaveConsents(ctx context.Context, consents []Consent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range consents {
		exists := false
		for _, existing := range r.consents {
			if existing.UserID == c.UserID && existing.DocumentType == c.DocumentType && existing.DocumentVersion == c.DocumentVersion {
				exists = true
				break
			}
		}
		if !exists {
			r.consents = append(r.consents, c)
		}
	}
	return nil
}

func (r *inMemoryProfileRepo) HasRequiredConsents(ctx context.Context, userID uuid.UUID, termsVersion, privacyVersion string) (bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	hasTerms := false
	hasPrivacy := false
	for _, c := range r.consents {
		if c.UserID == userID {
			if c.DocumentType == DocumentTerms && c.DocumentVersion == termsVersion {
				hasTerms = true
			}
			if c.DocumentType == DocumentPrivacy && c.DocumentVersion == privacyVersion {
				hasPrivacy = true
			}
		}
	}
	return hasTerms && hasPrivacy, nil
}

func TestProfileServiceMeAndLazyUserCreation(t *testing.T) {
	repo := newInMemoryProfileRepo()
	svc := NewService(repo, "2026-01", "2026-01")
	ctx := context.Background()

	externalID := "ba_user_" + uuid.NewString()
	identity := auth.Identity{
		BetterAuthUserID: externalID,
		Email:            "host_" + externalID[:8] + "@example.com",
		Name:             "Initial Host",
		EmailVerified:    false,
	}

	// First call: creates user lazily
	view, err := svc.Me(ctx, identity)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, view.ID)
	require.Equal(t, identity.Email, view.Email)
	require.Equal(t, "Initial Host", view.Name)
	require.False(t, view.EmailVerified)
	require.False(t, view.HasRequiredConsents)

	// Second call: updates display name and email verification
	identity.Name = "Updated Host"
	identity.EmailVerified = true
	updatedView, err := svc.Me(ctx, identity)
	require.NoError(t, err)
	require.Equal(t, view.ID, updatedView.ID)
	require.Equal(t, "Updated Host", updatedView.Name)
	require.True(t, updatedView.EmailVerified)
}

func TestProfileServiceConsent(t *testing.T) {
	repo := newInMemoryProfileRepo()
	svc := NewService(repo, "2026-01", "2026-01")
	ctx := context.Background()

	externalID := "ba_user_" + uuid.NewString()
	identity := auth.Identity{
		BetterAuthUserID: externalID,
		Email:            "consent_" + externalID[:8] + "@example.com",
		Name:             "Consent Host",
		EmailVerified:    true,
	}

	// Invalid consent version returns ErrInvalidConsent
	_, err := svc.Accept(ctx, identity, "old-terms", "2026-01")
	require.ErrorIs(t, err, ErrInvalidConsent)

	// Valid consent version succeeds and marks has_required_consents = true
	view, err := svc.Accept(ctx, identity, "2026-01", "2026-01")
	require.NoError(t, err)
	require.True(t, view.HasRequiredConsents)

	// Subsequent Me call retains consent
	viewAfter, err := svc.Me(ctx, identity)
	require.NoError(t, err)
	require.True(t, viewAfter.HasRequiredConsents)
}

func TestProfileServiceRequireEventCreation(t *testing.T) {
	repo := newInMemoryProfileRepo()
	svc := NewService(repo, "2026-01", "2026-01")
	ctx := context.Background()

	externalID := "ba_user_" + uuid.NewString()
	identity := auth.Identity{
		BetterAuthUserID: externalID,
		Email:            "gate_" + externalID[:8] + "@example.com",
		Name:             "Gate Host",
		EmailVerified:    false,
	}

	// 1. Unverified email blocks
	_, err := svc.RequireEventCreation(ctx, identity)
	require.ErrorIs(t, err, ErrEmailUnverified)

	// 2. Verified email without consent blocks
	identity.EmailVerified = true
	_, err = svc.RequireEventCreation(ctx, identity)
	require.ErrorIs(t, err, ErrConsentRequired)

	// 3. Verified email with consent succeeds
	_, err = svc.Accept(ctx, identity, "2026-01", "2026-01")
	require.NoError(t, err)

	view, err := svc.RequireEventCreation(ctx, identity)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, view.ID)
}

func TestProfileHandler(t *testing.T) {
	repo := newInMemoryProfileRepo()
	svc := NewService(repo, "2026-01", "2026-01")
	handler := NewHandler(svc)

	gin.SetMode(gin.TestMode)
	externalID := "ba_user_" + uuid.NewString()
	testIdentity := auth.Identity{
		BetterAuthUserID: externalID,
		Email:            "handler_" + externalID[:8] + "@example.com",
		Name:             "Handler Host",
		EmailVerified:    true,
	}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(auth.IdentityKey, testIdentity)
		c.Next()
	})
	router.GET("/me", handler.Me)
	router.POST("/me/consents", handler.Accept)

	// GET /me returns 200
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	require.Equal(t, http.StatusOK, res.Code)

	var view View
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &view))
	require.Equal(t, testIdentity.Email, view.Email)
	require.False(t, view.HasRequiredConsents)

	// POST /me/consents with invalid version returns 422
	badBody, _ := json.Marshal(map[string]string{
		"terms_version":   "invalid",
		"privacy_version": "2026-01",
	})
	reqBad := httptest.NewRequest(http.MethodPost, "/me/consents", bytes.NewReader(badBody))
	reqBad.Header.Set("Content-Type", "application/json")
	resBad := httptest.NewRecorder()
	router.ServeHTTP(resBad, reqBad)
	require.Equal(t, http.StatusUnprocessableEntity, resBad.Code)

	// POST /me/consents with valid versions returns 200
	goodBody, _ := json.Marshal(map[string]string{
		"terms_version":   "2026-01",
		"privacy_version": "2026-01",
	})
	reqGood := httptest.NewRequest(http.MethodPost, "/me/consents", bytes.NewReader(goodBody))
	reqGood.Header.Set("Content-Type", "application/json")
	resGood := httptest.NewRecorder()
	router.ServeHTTP(resGood, reqGood)
	require.Equal(t, http.StatusOK, resGood.Code)

	var consentedView View
	require.NoError(t, json.Unmarshal(resGood.Body.Bytes(), &consentedView))
	require.True(t, consentedView.HasRequiredConsents)
}
