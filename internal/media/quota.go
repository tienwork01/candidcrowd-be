package media

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
)

// ReserveLimits tunes one reservation. The event row carries the plan's
// limits; these only adjust how they apply to this request.
type ReserveLimits struct {
	// MaxEventBytes replaces the event's byte limit when positive. It keeps
	// uploads on the pre-plan cap while entitlement enforcement is off.
	MaxEventBytes int64
	// EnforceItems applies the media/photo/video item limits. Counters are
	// maintained either way, so they are accurate when enforcement starts.
	EnforceItems bool
}

// Quota resources reported by QuotaError.
const (
	ResourceBytes = "bytes"
	ResourceMedia = "media"
	ResourcePhoto = "photo"
	ResourceVideo = "video"
)

// QuotaError is returned when a reservation would take the event past one of
// its limits. Resource says which one, so a guest can be told the event is out
// of storage rather than out of photos.
type QuotaError struct {
	Resource string
	Limit    int64
	Usage    int64
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("event %s quota exceeded (%d of %d)", e.Resource, e.Usage, e.Limit)
}

func (e *QuotaError) APIError() *apierror.Error {
	message := "This event has reached its media limit."
	if e.Resource == ResourceBytes {
		message = "This event has run out of storage."
	}
	return apierror.New(http.StatusUnprocessableEntity, "event_quota_exceeded", message).
		WithDetails(map[string]any{"resource": e.Resource, "limit": e.Limit, "usage": e.Usage})
}

type uploadClosedError struct{}

func (uploadClosedError) Error() string { return "event is no longer accepting uploads" }

func (uploadClosedError) APIError() *apierror.Error {
	return apierror.New(http.StatusUnprocessableEntity, "event_upload_closed", "This event is no longer accepting uploads.")
}

// ErrUploadClosed is returned once an event's upload window has ended.
var ErrUploadClosed error = uploadClosedError{}

// mediaKind reports whether a MIME type counts as a photo or a video.
func mediaKind(mimeType string) (photo, video int) {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return 1, 0
	case strings.HasPrefix(mimeType, "video/"):
		return 0, 1
	}
	return 0, 0
}

// MediaKind is mediaKind for persistence adapters.
func MediaKind(mimeType string) (photo, video int) { return mediaKind(mimeType) }
