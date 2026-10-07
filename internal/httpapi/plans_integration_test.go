//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/database"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestIntegrationPlanEndpoints(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, sqlDB, err := database.Open(url, 5, 2, time.Minute)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	ctx := context.Background()

	plans := catalog.NewService(catalog.NewGormRepository(db))
	events := event.NewService(event.NewGormRepository(db, entitlement.NewProvisioner(plans)), 5<<30)
	profiles := profile.NewService(profile.NewGormRepository(db), "2026-01", "2026-01")
	handler := NewPlanHandler(events, profiles, entitlement.NewService(entitlement.NewGormRepository(db), plans))

	identity := func() auth.Identity {
		id := "plans-http-" + uuid.NewString()
		return auth.Identity{BetterAuthUserID: id, Email: id + "@example.test", EmailVerified: true}
	}
	owner, stranger := identity(), identity()
	ownerView, err := profiles.Me(ctx, owner)
	require.NoError(t, err)
	_, err = profiles.Me(ctx, stranger)
	require.NoError(t, err)

	evt, err := events.Create(ctx, ownerView.ID, event.CreateInput{Name: "HTTP plan test"})
	require.NoError(t, err)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM event_plan_grants WHERE event_id = ?`, evt.ID)
		db.Exec(`DELETE FROM events WHERE id = ?`, evt.ID)
	})

	serve := func(as auth.Identity, path string) *httptest.ResponseRecorder {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.Use(func(c *gin.Context) { c.Set(auth.IdentityKey, as); c.Next() })
		r.GET("/events/:id/plan", handler.EventPlan)
		r.GET("/events/:id/usage", handler.EventUsage)
		r.GET("/me/event-plans", handler.HostEventPlans)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	rec := serve(owner, "/events/"+evt.ID.String()+"/plan")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var plan struct {
		Plan struct {
			Code catalog.PlanCode `json:"code"`
		} `json:"plan"`
		UploadClosed   bool               `json:"upload_closed"`
		Features       []catalog.Feature  `json:"features"`
		UpgradeOptions []catalog.PlanCode `json:"upgrade_options"`
		Limits         struct {
			PhotoItems int64 `json:"photo_items"`
		} `json:"limits"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &plan))
	require.Equal(t, catalog.PlanFree, plan.Plan.Code)
	require.False(t, plan.UploadClosed)
	require.NotNil(t, plan.Features, "an empty feature list is [] not null")
	require.Equal(t, []catalog.PlanCode{catalog.PlanExperience, catalog.PlanSignature}, plan.UpgradeOptions)
	require.Equal(t, int64(45), plan.Limits.PhotoItems)

	rec = serve(owner, "/events/"+evt.ID.String()+"/usage")
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"usage":{"media_items":0,"photo_items":0,"video_items":0,"media_bytes":0},
		"limits":{"media_items":50,"photo_items":45,"video_items":5,"media_bytes":524288000}}`, rec.Body.String())

	rec = serve(owner, "/me/event-plans?per_page=5")
	require.Equal(t, http.StatusOK, rec.Code)
	var overview struct {
		Data []struct {
			Event struct {
				ID uuid.UUID `json:"id"`
			} `json:"event"`
			Plan *struct {
				Code catalog.PlanCode `json:"code"`
			} `json:"plan"`
			Usage struct {
				MediaLimit *int64 `json:"media_limit"`
			} `json:"usage"`
		} `json:"data"`
		Pagination event.Pagination `json:"pagination"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &overview))
	require.Len(t, overview.Data, 1)
	require.Equal(t, evt.ID, overview.Data[0].Event.ID)
	require.Equal(t, catalog.PlanFree, overview.Data[0].Plan.Code)
	require.Equal(t, int64(50), *overview.Data[0].Usage.MediaLimit)
	require.Equal(t, int64(1), overview.Pagination.Total)

	require.Equal(t, http.StatusNotFound, serve(stranger, "/events/"+evt.ID.String()+"/plan").Code, "another host cannot read the plan")
	require.Equal(t, http.StatusNotFound, serve(stranger, "/events/"+evt.ID.String()+"/usage").Code)
	require.Equal(t, http.StatusBadRequest, serve(owner, "/events/not-a-uuid/plan").Code)
	require.Equal(t, http.StatusBadRequest, serve(owner, "/me/event-plans?per_page=500").Code)
}

func TestIntegrationGuestBrandingFollowsPlanAndEnforcement(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, sqlDB, err := database.Open(url, 5, 2, time.Minute)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	ctx := context.Background()

	plans := catalog.NewService(catalog.NewGormRepository(db))
	events := event.NewService(event.NewGormRepository(db, entitlement.NewProvisioner(plans)), 5<<30)
	profiles := profile.NewService(profile.NewGormRepository(db), "2026-01", "2026-01")
	id := "branding-" + uuid.NewString()
	host, err := profiles.Me(ctx, auth.Identity{BetterAuthUserID: id, Email: id + "@example.test", EmailVerified: true})
	require.NoError(t, err)

	free, err := events.Create(ctx, host.ID, event.CreateInput{Name: "free"})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`UPDATE events SET trial_ended_at = now() WHERE id = ?`, free.ID).Error)
	paid, err := events.Create(ctx, host.ID, event.CreateInput{Name: "paid"})
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, e := range []uuid.UUID{free.ID, paid.ID} {
			db.Exec(`DELETE FROM event_plan_grants WHERE event_id = ?`, e)
			db.Exec(`DELETE FROM events WHERE id = ?`, e)
		}
	})
	repo := entitlement.NewGormRepository(db)
	_, err = entitlement.NewGrantService(repo, plans).Activate(ctx, entitlement.ActivateGrantInput{
		EventID: paid.ID, TargetPlanCode: catalog.PlanExperience, Source: entitlement.SourceManual, SourceRef: "brand-" + uuid.NewString(),
	})
	require.NoError(t, err)

	branding := func(enforce bool, slug string) bool {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		handler := NewPublicHandler(events, nil, nil, nil).
			UsePlans(entitlement.NewService(repo, plans, entitlement.WithEnforcement(enforce)), 5<<30)
		r.GET("/public/events/:slug", handler.Event)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/public/events/"+slug, nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body struct {
			ShowBranding bool `json:"show_branding"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		return body.ShowBranding
	}

	require.True(t, branding(true, free.Slug), "Free shows the credit once plans are enforced")
	require.False(t, branding(true, paid.Slug), "Experience removes branding")
	require.False(t, branding(false, free.Slug), "nothing changes for guests before enforcement")
}
