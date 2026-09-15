package event

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
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

func TestEventCreateAndIsolation(t *testing.T) {
	db := testDB(t)
	svc := NewService(db, 100*1024*1024)
	ctx := context.Background()

	host1 := "host1_" + uuid.NewString()
	host2 := "host2_" + uuid.NewString()

	// Host 1 creates an event
	e1, err := svc.Create(ctx, host1, host1+"@example.com", CreateInput{
		Name:               "Host 1 Wedding",
		EventType:          "Wedding",
		ExpectedGuestCount: 50,
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, e1.ID)
	require.Equal(t, "Host 1 Wedding", e1.Name)
	require.Equal(t, StatusActive, e1.Status)
	require.NotEmpty(t, e1.Slug)

	// Host 2 creates an event
	e2, err := svc.Create(ctx, host2, host2+"@example.com", CreateInput{
		Name:               "Host 2 Birthday",
		EventType:          "Birthday",
		ExpectedGuestCount: 20,
	})
	require.NoError(t, err)

	// Host 1 lists events: only sees Host 1 event
	list1, err := svc.List(ctx, host1)
	require.NoError(t, err)
	require.NotEmpty(t, list1)
	for _, item := range list1 {
		require.Equal(t, e1.HostID, item.HostID)
		require.NotEqual(t, e2.ID, item.ID)
	}

	// Host 1 can get Host 1 event
	owned, err := svc.GetOwned(ctx, e1.ID, host1)
	require.NoError(t, err)
	require.Equal(t, e1.ID, owned.ID)

	// Host 2 CANNOT get Host 1 event (returns record not found)
	_, err = svc.GetOwned(ctx, e1.ID, host2)
	require.Error(t, err)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestEventCreateValidation(t *testing.T) {
	db := testDB(t)
	svc := NewService(db, 100*1024*1024)
	ctx := context.Background()

	host := "host_" + uuid.NewString()

	// Empty name fails
	_, err := svc.Create(ctx, host, host+"@example.com", CreateInput{
		Name:               "   ",
		ExpectedGuestCount: 10,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "event name is required")

	// Negative guest count fails
	_, err = svc.Create(ctx, host, host+"@example.com", CreateInput{
		Name:               "Valid Name",
		ExpectedGuestCount: -5,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "expected guest count cannot be negative")
}

func TestEventHandlerCreateGate(t *testing.T) {
	db := testDB(t)
	eventSvc := NewService(db, 100*1024*1024)
	profileSvc := profile.NewService(db, "2026-01", "2026-01")
	handler := NewHandler(eventSvc, profileSvc)

	gin.SetMode(gin.TestMode)

	makeRouter := func(id auth.Identity) *gin.Engine {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set(auth.IdentityKey, id)
			c.Next()
		})
		r.POST("/events", handler.Create)
		r.GET("/events/:id", handler.Get)
		return r
	}

	hostID := "gate_host_" + uuid.NewString()

	// 1. Unverified email returns 403 email_unverified
	unverifiedIdentity := auth.Identity{
		BetterAuthUserID: hostID,
		Email:            hostID + "@example.com",
		Name:             "Unverified Host",
		EmailVerified:    false,
	}
	r1 := makeRouter(unverifiedIdentity)

	body, _ := json.Marshal(map[string]any{
		"name":                 "Celebration",
		"expected_guest_count": 25,
	})
	req1 := httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	res1 := httptest.NewRecorder()
	r1.ServeHTTP(res1, req1)
	require.Equal(t, http.StatusForbidden, res1.Code)
	require.Contains(t, res1.Body.String(), "email_unverified")

	// 2. Verified email without consent returns 403 consent_required
	verifiedNoConsentIdentity := auth.Identity{
		BetterAuthUserID: hostID,
		Email:            hostID + "@example.com",
		Name:             "Verified Host",
		EmailVerified:    true,
	}
	r2 := makeRouter(verifiedNoConsentIdentity)

	req2 := httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	res2 := httptest.NewRecorder()
	r2.ServeHTTP(res2, req2)
	require.Equal(t, http.StatusForbidden, res2.Code)
	require.Contains(t, res2.Body.String(), "consent_required")

	// 3. Verified email + consent succeeds with 201 Created
	ctx := context.Background()
	_, err := profileSvc.Accept(ctx, verifiedNoConsentIdentity, "2026-01", "2026-01")
	require.NoError(t, err)

	req3 := httptest.NewRequest(http.MethodPost, "/events", bytes.NewReader(body))
	req3.Header.Set("Content-Type", "application/json")
	res3 := httptest.NewRecorder()
	r2.ServeHTTP(res3, req3)
	require.Equal(t, http.StatusCreated, res3.Code)

	var created Event
	require.NoError(t, json.Unmarshal(res3.Body.Bytes(), &created))
	require.Equal(t, "Celebration", created.Name)

	// 4. Host A requests Host B's event via Handler -> 404
	attackerIdentity := auth.Identity{
		BetterAuthUserID: "attacker_" + uuid.NewString(),
		Email:            "attacker@example.com",
	}
	rAttacker := makeRouter(attackerIdentity)
	reqGet := httptest.NewRequest(http.MethodGet, "/events/"+created.ID.String(), nil)
	resGet := httptest.NewRecorder()
	rAttacker.ServeHTTP(resGet, reqGet)
	require.Equal(t, http.StatusNotFound, resGet.Code)
}
