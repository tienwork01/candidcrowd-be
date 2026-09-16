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

	r := NewRouter(logger, nil, nil, nil, func(context.Context) error { return nil }, nil, nil, nil)

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
