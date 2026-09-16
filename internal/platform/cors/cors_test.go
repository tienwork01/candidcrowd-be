package cors

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOrigins(t *testing.T) {
	origins := Origins(" http://localhost:3000 , https://qa.candidcrowd.com , ")
	require.Equal(t, []string{"http://localhost:3000", "https://qa.candidcrowd.com"}, origins)
}

func TestCORSMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	allowed := []string{"http://localhost:3000", "https://app.candidcrowd.com"}

	newRouter := func() *gin.Engine {
		r := gin.New()
		r.Use(Middleware(allowed))
		r.GET("/test", func(c *gin.Context) {
			c.Status(http.StatusOK)
		})
		r.POST("/test", func(c *gin.Context) {
			c.Status(http.StatusCreated)
		})
		return r
	}

	r := newRouter()

	t.Run("no origin header passes through", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		res := httptest.NewRecorder()
		r.ServeHTTP(res, req)
		require.Equal(t, http.StatusOK, res.Code)
		require.Empty(t, res.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("allowed origin gets CORS headers and 200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("Origin", "http://localhost:3000")
		res := httptest.NewRecorder()
		r.ServeHTTP(res, req)
		require.Equal(t, http.StatusOK, res.Code)
		require.Equal(t, "http://localhost:3000", res.Header().Get("Access-Control-Allow-Origin"))
		require.Equal(t, "Origin", res.Header().Get("Vary"))
		require.Contains(t, res.Header().Get("Access-Control-Allow-Headers"), "Authorization")
		require.Contains(t, res.Header().Get("Access-Control-Expose-Headers"), "X-Request-ID")
		require.Contains(t, res.Header().Get("Access-Control-Allow-Methods"), "GET, POST")
	})

	t.Run("preflight OPTIONS request on allowed origin returns 204", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/test", nil)
		req.Header.Set("Origin", "http://localhost:3000")
		res := httptest.NewRecorder()
		r.ServeHTTP(res, req)
		require.Equal(t, http.StatusNoContent, res.Code)
		require.Equal(t, "http://localhost:3000", res.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("disallowed origin returns 403 Forbidden", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("Origin", "http://evil.example.com")
		res := httptest.NewRecorder()
		r.ServeHTTP(res, req)
		require.Equal(t, http.StatusForbidden, res.Code)
		require.Empty(t, res.Header().Get("Access-Control-Allow-Origin"))
	})
}
