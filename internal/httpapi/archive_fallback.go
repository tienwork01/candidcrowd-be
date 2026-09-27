package httpapi

import (
	"context"
	"io"
	"net/http"

	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ArchiveReader streams an original from long-term storage by its opaque
// provider reference.
type ArchiveReader interface {
	OpenRead(context.Context, string) (io.ReadCloser, error)
}

// serveArchivedOriginal answers with the long-term copy of a record whose hot
// original has been removed, and reports whether it did.
//
// Once the archive job has verified a copy it deletes the object from R2, so
// for those records there is no key left to sign and streaming is the only way
// to serve them. Both the host gallery and the guest gallery need this: the
// host previously had no fallback at all, which would have left older events
// showing broken images the first time archiving was switched on.
func serveArchivedOriginal(c *gin.Context, reader ArchiveReader, mediaService *media.Service, eventID, mediaID uuid.UUID) bool {
	if reader == nil {
		return false
	}
	location, err := mediaService.ArchivedLocation(c.Request.Context(), eventID, mediaID)
	if err != nil || location == "" {
		return false
	}
	body, err := reader.OpenRead(c.Request.Context(), location)
	if err != nil {
		return false
	}
	defer body.Close()
	c.Header("Cache-Control", "private, no-store")
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, body)
	return true
}
