package profile

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = "postgres://candidcrowd:candidcrowd@127.0.0.1:5433/candidcrowd_test?sslmode=disable"
	}
	db, err := gorm.Open(postgres.Open(url), &gorm.Config{})
	if err != nil {
		t.Skipf("skipping test: database connection failed: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("skipping test: database ping failed")
	}
	return db
}

func TestProfileServiceMeAndLazyUserCreation(t *testing.T) {
	db := testDB(t)
	svc := NewService(db, "2026-01", "2026-01")
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
	db := testDB(t)
	svc := NewService(db, "2026-01", "2026-01")
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
	db := testDB(t)
	svc := NewService(db, "2026-01", "2026-01")
	ctx := context.Background()

	externalID := "ba_user_" + uuid.NewString()
	identity := auth.Identity{
		BetterAuthUserID: externalID,
		Email:            "gate_" + externalID[:8] + "@example.com",
		Name:             "Gate Host",
		EmailVerified:    false,
	}

	// 1. Unverified email blocks
	err := svc.RequireEventCreation(ctx, identity)
	require.ErrorIs(t, err, ErrEmailUnverified)

	// 2. Verified email without consent blocks
	identity.EmailVerified = true
	err = svc.RequireEventCreation(ctx, identity)
	require.ErrorIs(t, err, ErrConsentRequired)

	// 3. Verified email with consent succeeds
	_, err = svc.Accept(ctx, identity, "2026-01", "2026-01")
	require.NoError(t, err)

	err = svc.RequireEventCreation(ctx, identity)
	require.NoError(t, err)
}

func TestProfileHandler(t *testing.T) {
	db := testDB(t)
	svc := NewService(db, "2026-01", "2026-01")
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
