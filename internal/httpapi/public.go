package httpapi

import (
	"net/http"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/guest"
	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type PublicHandler struct {
	events *event.Service
	guests *guest.Service
	media  *media.Service
}

func NewPublicHandler(e *event.Service, g *guest.Service, m *media.Service) *PublicHandler {
	return &PublicHandler{e, g, m}
}

func (h *PublicHandler) Event(c *gin.Context) {
	e, err := h.events.GetPublic(c.Request.Context(), c.Param("slug"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "event not found"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": e.ID, "name": e.Name, "slug": e.Slug, "event_date": e.EventDate, "gallery_enabled": e.GalleryEnabled})
}

func (h *PublicHandler) CreateSession(c *gin.Context) {
	e, err := h.events.GetPublic(c.Request.Context(), c.Param("slug"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "event not found"))
		return
	}
	_, token, err := h.guests.Create(c.Request.Context(), e.ID)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"guest_session_token": token})
}

type uploadRequest struct {
	Filename          string `json:"filename" binding:"required,max=255"`
	MIMEType          string `json:"mime_type" binding:"required,max=100"`
	Size              int64  `json:"size" binding:"gt=0"`
	GuestSessionToken string `json:"guest_session_token" binding:"required"`
}

func (h *PublicHandler) CreateUpload(c *gin.Context) {
	var req uploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", err.Error()))
		return
	}
	e, err := h.events.GetPublic(c.Request.Context(), c.Param("slug"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "event not found"))
		return
	}
	session, err := h.guests.Validate(c.Request.Context(), e.ID, req.GuestSessionToken)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusUnauthorized, "invalid_guest_session", "guest session is invalid or expired"))
		return
	}
	target, err := h.media.CreateUpload(c.Request.Context(), e, session, media.CreateInput{Filename: req.Filename, MIMEType: req.MIMEType, Size: req.Size, SessionToken: req.GuestSessionToken})
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusUnprocessableEntity, "upload_not_allowed", err.Error()))
		return
	}
	c.JSON(http.StatusCreated, target)
}

type completeRequest struct {
	GuestSessionToken string `json:"guest_session_token" binding:"required"`
}

func (h *PublicHandler) Complete(c *gin.Context) {
	var req completeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", err.Error()))
		return
	}
	id, err := uuid.Parse(c.Param("uploadId"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "upload id must be a UUID"))
		return
	}
	e, err := h.events.GetPublic(c.Request.Context(), c.Param("slug"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "event not found"))
		return
	}
	session, err := h.guests.Validate(c.Request.Context(), e.ID, req.GuestSessionToken)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusUnauthorized, "invalid_guest_session", "guest session is invalid or expired"))
		return
	}
	if err = h.media.Complete(c.Request.Context(), e, session, id); err != nil {
		if err == gorm.ErrRecordNotFound {
			apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "upload not found"))
		} else {
			apierror.Respond(c, apierror.New(http.StatusUnprocessableEntity, "upload_verification_failed", err.Error()))
		}
		return
	}
	c.Status(http.StatusNoContent)
}
