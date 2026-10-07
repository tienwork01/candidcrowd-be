package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/exportjob"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type ExportHandler struct {
	events   *event.Service
	profiles *profile.Service
	exports  *exportjob.Service
	gate     planGate
}

// UsePlans makes the handler respect each event's plan.
func (h *ExportHandler) UsePlans(plans *entitlement.Service) *ExportHandler {
	h.gate = planGate{plans: plans}
	return h
}

func NewExportHandler(events *event.Service, profiles *profile.Service, exports *exportjob.Service) *ExportHandler {
	return &ExportHandler{events: events, profiles: profiles, exports: exports}
}

func (h *ExportHandler) ownedEvent(c *gin.Context) (event.Event, bool) {
	return resolveOwnedEvent(c, h.events, h.profiles)
}

func (h *ExportHandler) Create(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok || !h.gate.require(c, evt.ID, catalog.FeatureZIPExport) {
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
	filename := safeExportZipFilename(evt)
	job, downloadURL, err := h.exports.GetWithFilename(c.Request.Context(), evt.ID, jobID, filename)
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

func (h *ExportHandler) Latest(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	filename := safeExportZipFilename(evt)
	job, downloadURL, err := h.exports.LatestWithFilename(c.Request.Context(), evt.ID, filename)
	if err != nil {
		if errors.Is(err, exportjob.ErrNotFound) {
			c.JSON(http.StatusOK, gin.H{"export": nil})
			return
		}
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"export": gin.H{
		"id":            job.ID,
		"status":        job.Status,
		"error_message": job.ErrorMessage,
		"created_at":    job.CreatedAt,
		"completed_at":  job.CompletedAt,
		"download_url":  downloadURL,
	}})
}

func safeExportZipFilename(evt event.Event) string {
	base := strings.TrimSpace(evt.Slug)
	if base == "" {
		base = strings.TrimSpace(evt.Name)
	}
	if base == "" {
		return "candidcrowd-memories.zip"
	}
	var b strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	clean := strings.Trim(strings.ToLower(b.String()), "-")
	for strings.Contains(clean, "--") {
		clean = strings.ReplaceAll(clean, "--", "-")
	}
	if clean == "" {
		return "candidcrowd-memories.zip"
	}
	return clean + "-memories.zip"
}
