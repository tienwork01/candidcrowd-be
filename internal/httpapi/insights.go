package httpapi

import (
	"errors"
	"net/http"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/insights"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type InsightsHandler struct {
	events   *event.Service
	profiles *profile.Service
	insights *insights.Service
	gate     planGate
}

// UsePlans makes the handler respect each event's plan.
func (h *InsightsHandler) UsePlans(plans *entitlement.Service) *InsightsHandler {
	h.gate = planGate{plans: plans}
	return h
}

func NewInsightsHandler(events *event.Service, profiles *profile.Service, service *insights.Service) *InsightsHandler {
	return &InsightsHandler{events: events, profiles: profiles, insights: service}
}

func (h *InsightsHandler) ownedEvent(c *gin.Context) (event.Event, bool) {
	return resolveOwnedEvent(c, h.events, h.profiles)
}

func (h *InsightsHandler) Analytics(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	result, err := h.insights.Analytics(c.Request.Context(), evt.ID, evt.ExpectedGuestCount)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	// Participation is on every plan; the per-QR-source breakdown is not. It
	// is withheld rather than refused, so the rest of the dashboard works.
	response := analyticsResponse{Analytics: result}
	if !h.gate.view(c, evt.ID).allows(c, catalog.FeatureQRSourceAnalytics) {
		response.Sources = []insights.SourceMetric{}
		response.SourcesLocked = true
	}
	c.JSON(http.StatusOK, response)
}

type analyticsResponse struct {
	insights.Analytics
	SourcesLocked bool `json:"sources_locked"`
}

type createQRSourceRequest struct {
	Code string `json:"code" binding:"required"`
	Name string `json:"name" binding:"required"`
}

type updateQRSourceRequest struct {
	Name string `json:"name" binding:"required"`
}

func (h *InsightsHandler) ListSources(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	sources, err := h.insights.ListSources(c.Request.Context(), evt.ID)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": sources})
}

func (h *InsightsHandler) CreateSource(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	var req createQRSourceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", err.Error()))
		return
	}
	source, err := h.insights.CreateSource(c.Request.Context(), evt.ID, req.Code, req.Name)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", err.Error()))
		return
	}
	c.JSON(http.StatusCreated, source)
}

func (h *InsightsHandler) UpdateSource(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	sourceID, err := uuid.Parse(c.Param("sourceId"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "source id must be a UUID"))
		return
	}
	var req updateQRSourceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", err.Error()))
		return
	}
	source, err := h.insights.UpdateSource(c.Request.Context(), evt.ID, sourceID, req.Name)
	if err != nil {
		if errors.Is(err, insights.ErrNotFound) {
			apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "QR source not found"))
		} else {
			apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", err.Error()))
		}
		return
	}
	c.JSON(http.StatusOK, source)
}

func (h *InsightsHandler) DeleteSource(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	sourceID, err := uuid.Parse(c.Param("sourceId"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "source id must be a UUID"))
		return
	}
	if err = h.insights.DeleteSource(c.Request.Context(), evt.ID, sourceID); err != nil {
		if errors.Is(err, insights.ErrNotFound) {
			apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "QR source not found"))
		} else {
			apierror.Respond(c, err)
		}
		return
	}
	c.Status(http.StatusNoContent)
}
