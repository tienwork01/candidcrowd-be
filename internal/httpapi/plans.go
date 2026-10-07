package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// PlanHandler serves the read-only plan views a host needs. Nothing here can
// change a plan: grants are only issued through entitlement.GrantActivator.
type PlanHandler struct {
	events       *event.Service
	profiles     *profile.Service
	entitlements *entitlement.Service
}

func NewPlanHandler(events *event.Service, profiles *profile.Service, entitlements *entitlement.Service) *PlanHandler {
	return &PlanHandler{events: events, profiles: profiles, entitlements: entitlements}
}

type planRef struct {
	Code    catalog.PlanCode `json:"code"`
	Name    string           `json:"name"`
	Version int              `json:"version"`
}

type grantView struct {
	ID                 uuid.UUID          `json:"id"`
	Status             entitlement.Status `json:"status"`
	Source             entitlement.Source `json:"source"`
	ActivatedAt        time.Time          `json:"activated_at"`
	UploadExpiresAt    time.Time          `json:"upload_expires_at"`
	RetentionExpiresAt time.Time          `json:"retention_expires_at"`
}

type limitsView struct {
	MediaItems    int64 `json:"media_items"`
	PhotoItems    int64 `json:"photo_items"`
	VideoItems    int64 `json:"video_items"`
	MediaBytes    int64 `json:"media_bytes"`
	LiveWallItems int64 `json:"live_wall_items"`
	RetentionDays int   `json:"retention_days"`
}

func toLimitsView(l catalog.Limits) limitsView {
	return limitsView{
		MediaItems:    l.MaxMediaItems,
		PhotoItems:    l.MaxPhotoItems,
		VideoItems:    l.MaxVideoItems,
		MediaBytes:    l.MaxMediaBytes,
		LiveWallItems: l.MaxLiveWallItems,
		RetentionDays: l.RetentionDays,
	}
}

func (h *PlanHandler) resolve(c *gin.Context) (event.Event, entitlement.EventEntitlement, bool) {
	evt, ok := resolveOwnedEvent(c, h.events, h.profiles)
	if !ok {
		return evt, entitlement.EventEntitlement{}, false
	}
	ent, err := h.entitlements.Resolve(c.Request.Context(), evt.ID)
	if errors.Is(err, entitlement.ErrNoActiveGrant) {
		apierror.Respond(c, apierror.New(http.StatusNotFound, "plan_not_found", "this event has no plan yet"))
		return evt, ent, false
	}
	if err != nil {
		apierror.Respond(c, err)
		return evt, ent, false
	}
	return evt, ent, true
}

// EventPlan answers GET /events/:id/plan.
func (h *PlanHandler) EventPlan(c *gin.Context) {
	evt, ent, ok := h.resolve(c)
	if !ok {
		return
	}
	now := h.entitlements.Now()
	features := ent.Entitlements.Features
	if features == nil {
		features = []catalog.Feature{}
	}
	c.JSON(http.StatusOK, gin.H{
		"event_id": evt.ID,
		"plan":     planRef{Code: ent.PlanCode, Name: ent.PlanName, Version: ent.PlanVersion},
		"grant": grantView{
			ID:                 ent.GrantID,
			Status:             entitlement.StatusActive,
			Source:             ent.Source,
			ActivatedAt:        ent.ActivatedAt,
			UploadExpiresAt:    ent.UploadExpiresAt,
			RetentionExpiresAt: ent.RetentionExpiresAt,
		},
		"upload_closed":     ent.UploadClosed(now),
		"retention_expired": ent.RetentionExpired(now),
		"features":          features,
		"fair_use":          ent.Entitlements.FairUse,
		"limits":            toLimitsView(ent.Limits()),
		"upgrade_options":   ent.UpgradeOptions(),
	})
}

// EventUsage answers GET /events/:id/usage.
func (h *PlanHandler) EventUsage(c *gin.Context) {
	evt, ent, ok := h.resolve(c)
	if !ok {
		return
	}
	usage, err := h.entitlements.Usage(c.Request.Context(), evt.ID)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	limits := ent.Limits()
	c.JSON(http.StatusOK, gin.H{
		"usage": usage,
		"limits": gin.H{
			"media_items": limits.MaxMediaItems,
			"photo_items": limits.MaxPhotoItems,
			"video_items": limits.MaxVideoItems,
			"media_bytes": limits.MaxMediaBytes,
		},
	})
}

type hostPlansQuery struct {
	Page    int `form:"page" binding:"omitempty,min=1"`
	PerPage int `form:"per_page" binding:"omitempty,min=1,max=100"`
}

type hostEventPlanItem struct {
	Event struct {
		ID        uuid.UUID  `json:"id"`
		Name      string     `json:"name"`
		EventDate *time.Time `json:"event_date"`
	} `json:"event"`
	Plan *struct {
		Code   catalog.PlanCode   `json:"code"`
		Name   string             `json:"name"`
		Status entitlement.Status `json:"status"`
	} `json:"plan"`
	Usage struct {
		MediaItems      int64  `json:"media_items"`
		MediaLimit      *int64 `json:"media_limit"`
		MediaBytes      int64  `json:"media_bytes"`
		MediaBytesLimit *int64 `json:"media_bytes_limit"`
	} `json:"usage"`
	UploadExpiresAt    *time.Time `json:"upload_expires_at"`
	RetentionExpiresAt *time.Time `json:"retention_expires_at"`
}

// HostEventPlans answers GET /me/event-plans: every event of the host with its
// plan and usage summary, for the billing overview.
func (h *PlanHandler) HostEventPlans(c *gin.Context) {
	var query hostPlansQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_query", err.Error()))
		return
	}
	identity, err := auth.Get(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	hostID, err := h.profiles.UserID(c.Request.Context(), identity)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	page, err := h.entitlements.HostEventPlans(c.Request.Context(), hostID, query.Page, query.PerPage)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	items := make([]hostEventPlanItem, len(page.Items))
	for i, row := range page.Items {
		item := &items[i]
		item.Event.ID, item.Event.Name, item.Event.EventDate = row.EventID, row.EventName, row.EventDate
		if row.PlanCode != nil {
			item.Plan = &struct {
				Code   catalog.PlanCode   `json:"code"`
				Name   string             `json:"name"`
				Status entitlement.Status `json:"status"`
			}{Code: *row.PlanCode, Name: deref(row.PlanName), Status: derefStatus(row.GrantStatus)}
		}
		item.Usage.MediaItems = row.Usage.MediaItems
		item.Usage.MediaLimit = row.MediaItemsLimit
		item.Usage.MediaBytes = row.Usage.MediaBytes
		item.Usage.MediaBytesLimit = row.MediaBytesLimit
		item.UploadExpiresAt = row.UploadExpiresAt
		item.RetentionExpiresAt = row.RetentionExpiresAt
	}
	c.JSON(http.StatusOK, gin.H{
		"data": items,
		"pagination": event.Pagination{
			Page:       page.Page,
			PerPage:    page.PerPage,
			Total:      page.Total,
			TotalPages: page.TotalPages,
			HasNext:    page.Page < page.TotalPages,
			HasPrev:    page.Page > 1,
		},
	})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefStatus(s *entitlement.Status) entitlement.Status {
	if s == nil {
		return ""
	}
	return *s
}

// planGate is the plan check host handlers share. The zero value (no plans
// wired) allows everything, so handlers stay usable without the plan system.
type planGate struct {
	plans *entitlement.Service
}

// require answers the request with feature_not_in_plan and returns false when
// the event's plan lacks any of features. In shadow mode it always passes.
func (g planGate) require(c *gin.Context, eventID uuid.UUID, features ...catalog.Feature) bool {
	if g.plans == nil {
		return true
	}
	for _, feature := range features {
		if err := g.plans.Require(c.Request.Context(), eventID, feature); err != nil {
			apierror.Respond(c, err)
			return false
		}
	}
	return true
}

// planView lets a handler shape a response by plan without refusing it, such
// as capping a Live Wall or hiding a breakdown. It fails open: if the plan
// cannot be read, everything is allowed and the failure is logged.
type planView struct {
	gate planGate
	ent  entitlement.EventEntitlement
	ok   bool
}

func (g planGate) view(c *gin.Context, eventID uuid.UUID) planView {
	if g.plans == nil {
		return planView{gate: g}
	}
	ent, err := g.plans.Resolve(c.Request.Context(), eventID)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "plan.resolve_failed", "event_id", eventID, "error", err)
		return planView{gate: g}
	}
	return planView{gate: g, ent: ent, ok: true}
}

func (v planView) allows(c *gin.Context, feature catalog.Feature) bool {
	if !v.ok {
		return true
	}
	err := v.gate.plans.Check(c.Request.Context(), v.ent, feature)
	var denied *entitlement.FeatureError
	return !errors.As(err, &denied)
}

// includes reports whether the plan has feature without logging a denial.
// It is for high-traffic projections such as the guest page, where shadow
// logging every load would drown the rollout signal. Like allows, it treats an
// unreadable plan and shadow mode as "included".
func (v planView) includes(feature catalog.Feature) bool {
	if !v.ok || !v.gate.plans.Enforcing() {
		return true
	}
	return v.ent.Has(feature)
}

func (v planView) limits() (catalog.Limits, bool) {
	return v.ent.Limits(), v.ok
}
