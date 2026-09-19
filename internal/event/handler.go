package event

import (
	"errors"
	"net/http"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type Handler struct {
	service  *Service
	profiles *profile.Service
}

func NewHandler(service *Service, profiles *profile.Service) *Handler {
	return &Handler{service: service, profiles: profiles}
}

type createRequest struct {
	Name               string     `json:"name" binding:"required,max=200"`
	EventType          string     `json:"event_type" binding:"omitempty,max=80"`
	EventDate          *time.Time `json:"event_date"`
	ExpectedGuestCount int        `json:"expected_guest_count" binding:"gte=0"`
}

func (h *Handler) Create(c *gin.Context) {
	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", err.Error()))
		return
	}
	identity, err := auth.Get(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	view, err := h.profiles.RequireEventCreation(c.Request.Context(), identity)
	if err != nil {
		if errors.Is(err, profile.ErrEmailUnverified) {
			apierror.Respond(c, apierror.New(http.StatusForbidden, "email_unverified", "verify your email before creating an event"))
			return
		}
		if errors.Is(err, profile.ErrConsentRequired) {
			apierror.Respond(c, apierror.New(http.StatusForbidden, "consent_required", "accept the current terms and privacy policy"))
			return
		}
		apierror.Respond(c, err)
		return
	}
	created, err := h.service.Create(c.Request.Context(), view.ID, CreateInput{
		Name:               req.Name,
		EventType:          req.EventType,
		EventDate:          req.EventDate,
		ExpectedGuestCount: req.ExpectedGuestCount,
	})
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusCreated, created)
}

type listQuery struct {
	Page      int    `form:"page" binding:"omitempty,min=1"`
	PerPage   int    `form:"per_page" binding:"omitempty,min=1,max=100"`
	Query     string `form:"q" binding:"omitempty,max=100"`
	EventType string `form:"type" binding:"omitempty,max=80"`
	Sort      string `form:"sort" binding:"omitempty,oneof=newest oldest name upcoming"`
}

func (h *Handler) List(c *gin.Context) {
	var query listQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_query", err.Error()))
		return
	}
	identity, err := auth.Get(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	view, err := h.profiles.Me(c.Request.Context(), identity)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	output, err := h.service.List(c.Request.Context(), view.ID, ListInput{
		Page:      query.Page,
		PerPage:   query.PerPage,
		Query:     query.Query,
		EventType: query.EventType,
		Sort:      query.Sort,
	})
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data":       output.Events,
		"pagination": output.Pagination,
	})
}

func (h *Handler) Get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "event id must be a UUID"))
		return
	}
	identity, err := auth.Get(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	view, err := h.profiles.Me(c.Request.Context(), identity)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	evt, err := h.service.GetOwned(c.Request.Context(), id, view.ID)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "event not found"))
		return
	}
	c.JSON(http.StatusOK, evt)
}
