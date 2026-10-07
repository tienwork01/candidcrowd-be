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
	"gorm.io/datatypes"
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
	ClientRequestID    string     `json:"client_request_id" binding:"omitempty,uuid4"`
}

type updateRequest struct {
	Name                *string         `json:"name" binding:"omitempty,max=200"`
	EventType           *string         `json:"event_type" binding:"omitempty,max=80"`
	EventDate           *time.Time      `json:"event_date"`
	ClearEventDate      bool            `json:"clear_event_date"`
	ExpectedGuestCount  *int            `json:"expected_guest_count" binding:"omitempty,gte=0"`
	GalleryEnabled      *bool           `json:"gallery_enabled"`
	LifecyclePhase      *string         `json:"lifecycle_phase" binding:"omitempty,max=32"`
	SetupChecklist      *datatypes.JSON `json:"setup_checklist"`
	CandidCameraEnabled *bool           `json:"candid_camera_enabled"`
	GuestTheme          *datatypes.JSON `json:"guest_theme"`
	QRConfig            *datatypes.JSON `json:"qr_config"`
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
	var clientRequestID *uuid.UUID
	if req.ClientRequestID != "" {
		parsed, parseErr := uuid.Parse(req.ClientRequestID)
		if parseErr != nil {
			apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", "client_request_id must be a UUID"))
			return
		}
		clientRequestID = &parsed
	}
	created, err := h.service.Create(c.Request.Context(), view.ID, CreateInput{
		Name:               req.Name,
		EventType:          req.EventType,
		EventDate:          req.EventDate,
		ExpectedGuestCount: req.ExpectedGuestCount,
		ClientRequestID:    clientRequestID,
	})
	if err != nil {
		if errors.Is(err, ErrTrialWindowLimitReached) {
			apierror.Respond(c, apierror.New(http.StatusTooManyRequests, "trial_window_limit_reached", "you have reached the Free Trial limit for the last 30 days"))
			return
		}
		if errors.Is(err, ErrActiveEventLimitReached) {
			apierror.Respond(c, apierror.New(http.StatusConflict, "active_event_limit_reached", "you already have an active event"))
			return
		}
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
	Sort      string `form:"sort" binding:"omitempty,oneof=newest oldest name upcoming created_at event_date"`
	Direction string `form:"direction" binding:"omitempty,oneof=asc desc ASC DESC"`
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
	hostID, err := h.profiles.UserID(c.Request.Context(), identity)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	output, err := h.service.List(c.Request.Context(), hostID, ListInput{
		Page:      query.Page,
		PerPage:   query.PerPage,
		Query:     query.Query,
		EventType: query.EventType,
		Sort:      query.Sort,
		Direction: query.Direction,
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
	hostID, err := h.profiles.UserID(c.Request.Context(), identity)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	evt, err := h.service.GetOwned(c.Request.Context(), id, hostID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "event not found"))
		} else {
			apierror.Respond(c, err)
		}
		return
	}
	c.JSON(http.StatusOK, evt)
}

func (h *Handler) Update(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "event id must be a UUID"))
		return
	}
	var req updateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", err.Error()))
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
	updated, err := h.service.Update(c.Request.Context(), id, hostID, UpdateInput{
		Name:                req.Name,
		EventType:           req.EventType,
		EventDate:           req.EventDate,
		ClearEventDate:      req.ClearEventDate,
		ExpectedGuestCount:  req.ExpectedGuestCount,
		GalleryEnabled:      req.GalleryEnabled,
		LifecyclePhase:      req.LifecyclePhase,
		SetupChecklist:      req.SetupChecklist,
		CandidCameraEnabled: req.CandidCameraEnabled,
		GuestTheme:          req.GuestTheme,
		QRConfig:            req.QRConfig,
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "event not found"))
		} else {
			var coder apierror.Coder
			if errors.As(err, &coder) {
				apierror.Respond(c, err)
				return
			}
			apierror.Respond(c, apierror.New(http.StatusBadRequest, "update_failed", err.Error()))
		}
		return
	}
	c.JSON(http.StatusOK, updated)
}

func (h *Handler) Delete(c *gin.Context) {
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
	hostID, err := h.profiles.UserID(c.Request.Context(), identity)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	if err = h.service.Delete(c.Request.Context(), id, hostID); err != nil {
		if errors.Is(err, ErrNotFound) {
			apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "event not found"))
		} else {
			apierror.Respond(c, err)
		}
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) Close(c *gin.Context) {
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
	hostID, err := h.profiles.UserID(c.Request.Context(), identity)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	if err = h.service.Close(c.Request.Context(), id, hostID); err != nil {
		if errors.Is(err, ErrNotFound) {
			apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "event not found"))
		} else {
			apierror.Respond(c, err)
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "closed"})
}
