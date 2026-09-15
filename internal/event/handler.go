package event

import (
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
	if e := c.ShouldBindJSON(&req); e != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", e.Error()))
		return
	}
	i, e := auth.Get(c)
	if e != nil {
		apierror.Respond(c, e)
		return
	}
	if e = h.profiles.RequireEventCreation(c.Request.Context(), i); e != nil {
		switch e {
		case profile.ErrEmailUnverified:
			apierror.Respond(c, apierror.New(http.StatusForbidden, "email_unverified", "verify your email before creating an event"))
		case profile.ErrConsentRequired:
			apierror.Respond(c, apierror.New(http.StatusForbidden, "consent_required", "accept the current terms and privacy policy"))
		default:
			apierror.Respond(c, e)
		}
		return
	}
	out, e := h.service.Create(c.Request.Context(), i.BetterAuthUserID, i.Email, CreateInput(req))
	if e != nil {
		apierror.Respond(c, e)
		return
	}
	c.JSON(http.StatusCreated, out)
}
func (h *Handler) List(c *gin.Context) {
	i, e := auth.Get(c)
	if e != nil {
		apierror.Respond(c, e)
		return
	}
	out, e := h.service.List(c.Request.Context(), i.BetterAuthUserID)
	if e != nil {
		apierror.Respond(c, e)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}
func (h *Handler) Get(c *gin.Context) {
	id, e := uuid.Parse(c.Param("id"))
	if e != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "event id must be a UUID"))
		return
	}
	i, _ := auth.Get(c)
	out, e := h.service.GetOwned(c.Request.Context(), id, i.BetterAuthUserID)
	if e != nil {
		apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "event not found"))
		return
	}
	c.JSON(http.StatusOK, out)
}
