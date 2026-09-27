package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRouter_RequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))

	r := NewRouter(RouterConfig{
		Logger:        logger,
		DatabaseReady: func(context.Context) error { return nil },
	})

	t.Run("generates request ID when missing", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		res := httptest.NewRecorder()
		r.ServeHTTP(res, req)

		require.Equal(t, http.StatusOK, res.Code)
		reqID := res.Header().Get("X-Request-ID")
		require.NotEmpty(t, reqID)
	})

	t.Run("preserves existing request ID", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set("X-Request-ID", "custom-request-id-123")
		res := httptest.NewRecorder()
		r.ServeHTTP(res, req)

		require.Equal(t, http.StatusOK, res.Code)
		require.Equal(t, "custom-request-id-123", res.Header().Get("X-Request-ID"))
	})
}

// The batch confirmation route sits at the same position as the per-item
// route's :uploadId parameter. Gin has historically panicked on a static
// segment colliding with a wildcard there, so this pins down that both
// patterns coexist and that each dispatches to its own handler.
func TestUploadCompleteRoutesCoexist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	pub := r.Group("/api/v1/public/events/:slug")

	var hit string
	pub.POST("/uploads/:uploadId/complete", func(c *gin.Context) {
		hit = "single:" + c.Param("uploadId")
		c.Status(http.StatusNoContent)
	})
	pub.POST("/uploads/complete", func(c *gin.Context) {
		hit = "batch"
		c.Status(http.StatusOK)
	})

	t.Run("per-item route still resolves", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/public/events/evt_x/uploads/abc-123/complete", nil)
		res := httptest.NewRecorder()
		r.ServeHTTP(res, req)
		require.Equal(t, http.StatusNoContent, res.Code)
		require.Equal(t, "single:abc-123", hit)
	})

	t.Run("batch route resolves", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/public/events/evt_x/uploads/complete", nil)
		res := httptest.NewRecorder()
		r.ServeHTTP(res, req)
		require.Equal(t, http.StatusOK, res.Code)
		require.Equal(t, "batch", hit)
	})
}
