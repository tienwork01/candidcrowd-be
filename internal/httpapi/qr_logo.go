package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
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

const maxQRLogoBytes int64 = 1 << 20
const qrLogoCacheControl = "private, max-age=1800"

// QRLogoHandler manages the small, host-owned image embedded in a styled QR.
// It deliberately uses a separate object namespace from guest media.
type QRLogoHandler struct {
	events   *event.Service
	profiles *profile.Service
	storage  media.Storage
	expiry   time.Duration
	gate     planGate
}

// UsePlans makes the handler respect each event's plan.
func (h *QRLogoHandler) UsePlans(plans *entitlement.Service) *QRLogoHandler {
	h.gate = planGate{plans: plans}
	return h
}

func NewQRLogoHandler(events *event.Service, profiles *profile.Service, storage media.Storage, expiry time.Duration) *QRLogoHandler {
	return &QRLogoHandler{events: events, profiles: profiles, storage: storage, expiry: expiry}
}

type qrLogoUploadRequest struct {
	MIMEType string `json:"mime_type" binding:"required"`
	Size     int64  `json:"size" binding:"required,gt=0"`
}

func qrLogoExtension(mime string) (string, bool) {
	switch mime {
	case "image/png":
		return ".png", true
	case "image/jpeg":
		return ".jpg", true
	case "image/webp":
		return ".webp", true
	case "image/svg+xml":
		return ".svg", true
	default:
		return "", false
	}
}

func qrLogoFormat(mime string) (string, bool) {
	switch mime {
	case "image/png":
		return "png", true
	case "image/jpeg":
		return "jpg", true
	case "image/webp":
		return "webp", true
	case "image/svg+xml":
		return "svg", true
	default:
		return "", false
	}
}

func qrLogoMIME(format string) (string, bool) {
	switch format {
	case "png":
		return "image/png", true
	case "jpg":
		return "image/jpeg", true
	case "webp":
		return "image/webp", true
	case "svg":
		return "image/svg+xml", true
	default:
		return "", false
	}
}

func qrLogoURL(slug string, assetID uuid.UUID, mime string) string {
	format, _ := qrLogoFormat(mime)
	return "/api/v1/public/events/" + slug + "/qr-logo/" + assetID.String() + "?format=" + format
}

func (h *QRLogoHandler) ownedEvent(c *gin.Context) (event.Event, bool) {
	return resolveOwnedEvent(c, h.events, h.profiles)
}

func qrLogoKey(eventID, assetID uuid.UUID, mime string) (string, error) {
	ext, ok := qrLogoExtension(mime)
	if !ok {
		return "", fmt.Errorf("unsupported logo MIME type")
	}
	return filepath.ToSlash(fmt.Sprintf("events/%s/assets/qr-logo/%s%s", eventID, assetID, ext)), nil
}

func (h *QRLogoHandler) CreateUploadTarget(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok || !h.gate.require(c, evt.ID, catalog.FeatureFullCustomization) {
		return
	}
	var req qrLogoUploadRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Size > maxQRLogoBytes {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_logo", "logo must be a supported image up to 1 MB"))
		return
	}
	assetID := uuid.New()
	key, err := qrLogoKey(evt.ID, assetID, req.MIMEType)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_logo", err.Error()))
		return
	}
	uploadURL, err := h.storage.PresignPut(c.Request.Context(), key, req.MIMEType, h.expiry)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"asset_id": assetID, "upload_url": uploadURL, "expires_at": time.Now().Add(h.expiry), "required_headers": gin.H{"Content-Type": req.MIMEType, "Cache-Control": qrLogoCacheControl}})
}

// Upload accepts the small QR branding asset through the API and writes it to
// private R2 storage. Unlike event media, this is capped at 1 MiB; proxying it
// avoids a browser-to-R2 CORS dependency while preserving direct R2 uploads
// for the large guest media flow.
func (h *QRLogoHandler) Upload(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok || !h.gate.require(c, evt.ID, catalog.FeatureFullCustomization) {
		return
	}
	// Leave room for multipart boundaries and headers in addition to the file.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxQRLogoBytes+64<<10)
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_logo", "logo must be a supported image up to 1 MB"))
		return
	}
	defer file.Close()
	if header.Size <= 0 || header.Size > maxQRLogoBytes {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_logo", "logo must be a supported image up to 1 MB"))
		return
	}
	mime := header.Header.Get("Content-Type")
	assetID := uuid.New()
	key, err := qrLogoKey(evt.ID, assetID, mime)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_logo", err.Error()))
		return
	}
	if err := h.storage.Put(c.Request.Context(), key, mime, file); err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"logo_url": qrLogoURL(evt.Slug, assetID, mime)})
}

func (h *QRLogoHandler) Complete(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok || !h.gate.require(c, evt.ID, catalog.FeatureFullCustomization) {
		return
	}
	assetID, err := uuid.Parse(c.Param("assetId"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "asset id must be a UUID"))
		return
	}
	var req qrLogoUploadRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Size > maxQRLogoBytes {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_logo", "logo must be a supported image up to 1 MB"))
		return
	}
	key, err := qrLogoKey(evt.ID, assetID, req.MIMEType)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_logo", err.Error()))
		return
	}
	info, err := h.storage.Head(c.Request.Context(), key)
	if err != nil || info.Size != req.Size || info.ContentType != req.MIMEType {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_logo", "uploaded logo could not be verified"))
		return
	}
	c.JSON(http.StatusOK, gin.H{"logo_url": qrLogoURL(evt.Slug, assetID, req.MIMEType)})
}

func (h *QRLogoHandler) Content(c *gin.Context) {
	evt, err := h.events.GetPublic(c.Request.Context(), c.Param("slug"))
	if errors.Is(err, event.ErrNotFound) {
		apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "event not found"))
		return
	}
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	assetID, err := uuid.Parse(c.Param("assetId"))
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	// The UUID is server-generated and returned only to the authenticated host
	// that uploaded it. The format lets us read one immutable R2 object rather
	// than probing every supported extension on each QR redraw.
	format := c.Query("format")
	if format == "" {
		// Compatibility for logo URLs saved before the format query existed.
		for _, candidate := range []string{"png", "jpg", "webp", "svg"} {
			mime, _ := qrLogoMIME(candidate)
			key, _ := qrLogoKey(evt.ID, assetID, mime)
			body, info, openErr := h.storage.OpenRead(c.Request.Context(), key)
			if openErr != nil {
				continue
			}
			defer body.Close()
			c.Header("Cache-Control", "private, max-age=31536000, immutable")
			c.DataFromReader(http.StatusOK, info.Size, info.ContentType, body, nil)
			return
		}
		c.Status(http.StatusNotFound)
		return
	}
	mime, ok := qrLogoMIME(format)
	if !ok {
		c.Status(http.StatusNotFound)
		return
	}
	key, _ := qrLogoKey(evt.ID, assetID, mime)
	body, info, err := h.storage.OpenRead(c.Request.Context(), key)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	defer body.Close()
	// Asset IDs are never overwritten, making aggressive private browser caching
	// safe and avoiding an R2 roundtrip each time the editor opens.
	c.Header("Cache-Control", "private, max-age=31536000, immutable")
	c.DataFromReader(http.StatusOK, info.Size, info.ContentType, body, nil)
}
