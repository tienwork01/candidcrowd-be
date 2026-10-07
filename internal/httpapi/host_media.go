package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// mediaLinkExpiry bounds a signed gallery link. It outlives a realtime stream
// (REALTIME_STREAM_MAX_AGE defaults to 30 minutes), so the refetch that
// follows a stream reconnect always hands the client fresh links.
const mediaLinkExpiry = 30 * time.Minute

// HostMediaHandler exposes a host-only media view. It intentionally remains
// separate from the public gallery so hosts can review media even when guest
// gallery access is disabled.
type HostMediaHandler struct {
	events   *event.Service
	profiles *profile.Service
	media    *media.Service
	// archiveReader serves originals that have been moved to long-term
	// storage. Without it a host reviewing an older event would be shown a
	// signed link to an object that no longer exists.
	archiveReader ArchiveReader
	gate          planGate
}

// UsePlans makes the handler respect each event's plan.
func (h *HostMediaHandler) UsePlans(plans *entitlement.Service) *HostMediaHandler {
	h.gate = planGate{plans: plans}
	return h
}

func NewHostMediaHandler(events *event.Service, profiles *profile.Service, mediaService *media.Service, readers ...ArchiveReader) *HostMediaHandler {
	h := &HostMediaHandler{events: events, profiles: profiles, media: mediaService}
	if len(readers) > 0 {
		h.archiveReader = readers[0]
	}
	return h
}

func (h *HostMediaHandler) ownedEvent(c *gin.Context) (event.Event, bool) {
	return resolveOwnedEvent(c, h.events, h.profiles)
}

func (h *HostMediaHandler) List(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "24"))
	page, err := h.media.ListGallery(c.Request.Context(), evt.ID, media.GalleryFilter(c.DefaultQuery("filter", "all")), media.GallerySort(c.DefaultQuery("sort", "newest")), limit, c.Query("cursor"), func(id uuid.UUID) string {
		return "/api/v1/events/" + evt.ID.String() + "/media/" + id.String() + "/content"
	})
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_query", err.Error()))
		return
	}
	// A browser image request cannot attach the host's Bearer token, so the
	// links are signed here, after ownedEvent has authorized this host.
	if err := h.media.SignPage(c.Request.Context(), page.Data, mediaLinkExpiry); err != nil {
		apierror.Respond(c, err)
		return
	}
	body := gin.H{
		"data": page.Data,
		"page": gin.H{"next_cursor": page.NextCursor, "has_more": page.HasMore},
	}
	// Omitted rather than sent as null on cursor pages, so a client that keeps
	// the totals from the first page is never asked to overwrite them.
	if page.Counts != nil {
		body["counts"] = page.Counts
	}
	c.JSON(http.StatusOK, body)
}

type mediaStatusRequest struct {
	Status media.Status `json:"status" binding:"required"`
}

type mediaBatchRequest struct {
	IDs    []string     `json:"ids" binding:"required,min=1,max=100"`
	Status media.Status `json:"status"`
}

func parseMediaIDs(values []string) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, 0, len(values))
	seen := make(map[uuid.UUID]struct{}, len(values))
	for _, value := range values {
		id, err := uuid.Parse(value)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[id]; !exists {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("at least one media id is required")
	}
	return ids, nil
}

func (h *HostMediaHandler) UpdateStatus(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	mediaID, err := uuid.Parse(c.Param("mediaId"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "media id must be a UUID"))
		return
	}
	var req mediaStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", err.Error()))
		return
	}
	// Hiding and showing one photo is a safety tool and is never paid for;
	// featuring it is.
	if req.Status == media.StatusFeatured && !h.gate.require(c, evt.ID, catalog.FeatureFeatureMedia) {
		return
	}
	updated, err := h.media.UpdateStatus(c.Request.Context(), evt.ID, mediaID, req.Status)
	if err != nil {
		if errors.Is(err, media.ErrNotFound) {
			apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "media not found"))
		} else {
			apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", err.Error()))
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": updated.ID, "status": updated.Status})
}

func (h *HostMediaHandler) BatchUpdateStatus(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	var req mediaBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", err.Error()))
		return
	}
	ids, err := parseMediaIDs(req.IDs)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "media ids must be UUIDs"))
		return
	}
	features := []catalog.Feature{catalog.FeatureBulkModeration}
	if req.Status == media.StatusFeatured {
		features = append(features, catalog.FeatureFeatureMedia)
	}
	if !h.gate.require(c, evt.ID, features...) {
		return
	}
	if err = h.media.UpdateStatuses(c.Request.Context(), evt.ID, ids, req.Status); err != nil {
		if errors.Is(err, media.ErrNotFound) {
			apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "one or more media items were not found"))
		} else {
			apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", err.Error()))
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"updated_count": len(ids), "ids": ids})
}

func (h *HostMediaHandler) Delete(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	mediaID, err := uuid.Parse(c.Param("mediaId"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "media id must be a UUID"))
		return
	}
	if err = h.media.DeleteForEvent(c.Request.Context(), evt.ID, mediaID); err != nil {
		if errors.Is(err, media.ErrNotFound) {
			apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "media not found"))
		} else {
			apierror.Respond(c, err)
		}
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *HostMediaHandler) BatchDelete(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	var req mediaBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", err.Error()))
		return
	}
	ids, err := parseMediaIDs(req.IDs)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "media ids must be UUIDs"))
		return
	}
	if err = h.media.DeleteManyForEvent(c.Request.Context(), evt.ID, ids); err != nil {
		if errors.Is(err, media.ErrNotFound) {
			apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "one or more media items were not found"))
		} else {
			apierror.Respond(c, err)
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted_count": len(ids), "ids": ids})
}

func (h *HostMediaHandler) Content(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	mediaID, err := uuid.Parse(c.Param("mediaId"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "media id must be a UUID"))
		return
	}
	url, err := h.media.ReadURL(c.Request.Context(), evt.ID, mediaID, 5*time.Minute)
	if err != nil {
		if serveArchivedOriginal(c, h.archiveReader, h.media, evt.ID, mediaID) {
			return
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
