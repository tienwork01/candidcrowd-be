package httpapi

import (
	"errors"
	"net/http"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/exportjob"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type ExportHandler struct {
	events   *event.Service
	profiles *profile.Service
	exports  *exportjob.Service
}

func NewExportHandler(events *event.Service, profiles *profile.Service, exports *exportjob.Service) *ExportHandler {
	return &ExportHandler{events: events, profiles: profiles, exports: exports}
}

func (h *ExportHandler) ownedEvent(c *gin.Context) (event.Event, bool) {
	eventID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "event id must be a UUID"))
		return event.Event{}, false
	}
	identity, err := auth.Get(c)
	if err != nil {
		apierror.Respond(c, err)
		return event.Event{}, false
	}
	hostID, err := h.profiles.UserID(c.Request.Context(), identity)
	if err != nil {
		apierror.Respond(c, err)
		return event.Event{}, false
	}
	evt, err := h.events.GetOwned(c.Request.Context(), eventID, hostID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "event not found"))
		} else {
			apierror.Respond(c, err)
		}
		return event.Event{}, false
	}
	return evt, true
}

func (h *ExportHandler) Create(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	job, err := h.exports.Create(c.Request.Context(), evt.ID)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusAccepted, job)
}

func (h *ExportHandler) Get(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	jobID, err := uuid.Parse(c.Param("exportId"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "export id must be a UUID"))
		return
	}
	job, downloadURL, err := h.exports.Get(c.Request.Context(), evt.ID, jobID)
	if err != nil {
		if errors.Is(err, exportjob.ErrNotFound) {
			apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "export not found"))
		} else {
			apierror.Respond(c, err)
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": job.ID, "status": job.Status, "error_message": job.ErrorMessage, "created_at": job.CreatedAt, "completed_at": job.CompletedAt, "download_url": downloadURL})
}
