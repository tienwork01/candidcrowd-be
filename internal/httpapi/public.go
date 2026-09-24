package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/guest"
	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type PublicHandler struct {
	events        *event.Service
	guests        *guest.Service
	media         *media.Service
	archiveReader interface {
		OpenRead(context.Context, string) (io.ReadCloser, error)
	}
}

func NewPublicHandler(events *event.Service, guests *guest.Service, media *media.Service, readers ...interface {
	OpenRead(context.Context, string) (io.ReadCloser, error)
}) *PublicHandler {
	h := &PublicHandler{events: events, guests: guests, media: media}
	if len(readers) > 0 {
		h.archiveReader = readers[0]
	}
	return h
}

func (h *PublicHandler) publicEvent(c *gin.Context) (event.Event, bool) {
	evt, err := h.events.GetPublic(c.Request.Context(), c.Param("slug"))
	if err == nil {
		return evt, true
	}
	if errors.Is(err, event.ErrNotFound) {
		apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "event not found"))
	} else {
		apierror.Respond(c, err)
	}
	return event.Event{}, false
}

func (h *PublicHandler) Event(c *gin.Context) {
	evt, ok := h.publicEvent(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":              evt.ID,
		"name":            evt.Name,
		"slug":            evt.Slug,
		"event_date":      evt.EventDate,
		"event_type":      evt.EventType,
		"gallery_enabled": evt.GalleryEnabled,
		"guest_theme":     evt.GuestTheme,
	})
}

func (h *PublicHandler) Media(c *gin.Context) {
	evt, ok := h.publicEvent(c)
	if !ok {
		return
	}
	if !evt.GalleryEnabled {
		apierror.Respond(c, apierror.New(http.StatusForbidden, "gallery_disabled", "event gallery is disabled"))
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "60"))
	items, err := h.media.ListReady(c.Request.Context(), evt.ID, limit, c.Query("cursor"), func(id uuid.UUID) string {
		return "/api/v1/public/events/" + evt.Slug + "/media/" + id.String() + "/content"
	})
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": items.Data, "page": gin.H{"next_cursor": items.NextCursor, "has_more": items.HasMore}})
}

func (h *PublicHandler) MediaContent(c *gin.Context) {
	evt, ok := h.publicEvent(c)
	if !ok {
		return
	}
	if !evt.GalleryEnabled {
		apierror.Respond(c, apierror.New(http.StatusForbidden, "gallery_disabled", "event gallery is disabled"))
		return
	}
	mediaID, err := uuid.Parse(c.Param("mediaId"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "media id must be a UUID"))
		return
	}
	url, err := h.media.ReadURL(c.Request.Context(), evt.ID, mediaID, 5*time.Minute)
	if err != nil {
		if h.archiveReader != nil {
			if location, locationErr := h.media.ArchivedLocation(c.Request.Context(), evt.ID, mediaID); locationErr == nil && location != "" {
				body, readErr := h.archiveReader.OpenRead(c.Request.Context(), location)
				if readErr == nil {
					defer body.Close()
					c.Header("Cache-Control", "private, no-store")
					c.Status(http.StatusOK)
					_, _ = io.Copy(c.Writer, body)
					return
				}
			}
		}
		if errors.Is(err, media.ErrNotFound) {
			apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "media not found"))
			return
		}
		apierror.Respond(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.Redirect(http.StatusTemporaryRedirect, url)
}

func (h *PublicHandler) CreateSession(c *gin.Context) {
	evt, ok := h.publicEvent(c)
	if !ok {
		return
	}
	_, token, err := h.guests.Create(c.Request.Context(), evt.ID)
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
	ChecksumSHA256    string `json:"checksum_sha256" binding:"required,len=64,hexadecimal"`
	ClientUploadID    string `json:"client_upload_id" binding:"required,uuid4"`
	GuestSessionToken string `json:"guest_session_token" binding:"required"`
}

func (h *PublicHandler) CreateUpload(c *gin.Context) {
	var req uploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", err.Error()))
		return
	}
	evt, ok := h.publicEvent(c)
	if !ok {
		return
	}
	session, err := h.guests.Validate(c.Request.Context(), evt.ID, req.GuestSessionToken)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusUnauthorized, "invalid_guest_session", "guest session is invalid or expired"))
		return
	}
	clientUploadID, err := uuid.Parse(req.ClientUploadID)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", "client_upload_id must be a UUID"))
		return
	}
	target, err := h.media.CreateUpload(c.Request.Context(), media.UploadScope{
		EventID:        evt.ID,
		GuestSessionID: session.ID,
		Accepting:      evt.Status == event.StatusActive,
		MaxEventBytes:  evt.MaxMediaBytes,
	}, media.CreateInput{
		Filename:       req.Filename,
		MIMEType:       req.MIMEType,
		Size:           req.Size,
		ChecksumSHA256: req.ChecksumSHA256,
		ClientUploadID: clientUploadID,
		SessionToken:   req.GuestSessionToken,
	})
	if err != nil {
		if errors.Is(err, media.ErrDuplicate) {
			apierror.Respond(c, apierror.New(http.StatusConflict, "duplicate_media", "this file has already been uploaded to this event"))
			return
		}
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
	evt, ok := h.publicEvent(c)
	if !ok {
		return
	}
	session, err := h.guests.Validate(c.Request.Context(), evt.ID, req.GuestSessionToken)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusUnauthorized, "invalid_guest_session", "guest session is invalid or expired"))
		return
	}
	if err = h.media.Complete(c.Request.Context(), media.UploadScope{
		EventID:        evt.ID,
		GuestSessionID: session.ID,
		Accepting:      evt.Status == event.StatusActive,
		MaxEventBytes:  evt.MaxMediaBytes,
	}, id); err != nil {
		if errors.Is(err, media.ErrNotFound) {
			apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "upload not found"))
		} else if errors.Is(err, media.ErrDuplicate) {
			apierror.Respond(c, apierror.New(http.StatusConflict, "duplicate_media", "this file has already been uploaded to this event"))
		} else {
			apierror.Respond(c, apierror.New(http.StatusUnprocessableEntity, "upload_verification_failed", err.Error()))
		}
		return
	}
	c.Status(http.StatusNoContent)
}
