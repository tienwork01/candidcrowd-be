package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/cors"
	redispkg "github.com/candidcrowd/candidcrowd-backend/internal/platform/redis"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/gin-gonic/gin"
)

func NewRouter(logger *slog.Logger, authn *auth.Verifier, limiter *redispkg.Limiter, allowedOrigins []string, databaseReady func(context.Context) error, profiles *profile.Handler, events *event.Handler, public *PublicHandler) *gin.Engine {
	r := gin.New()
	r.Use(recovery(logger), requestLog(logger), cors.Middleware(allowedOrigins))
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/readyz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := databaseReady(ctx); err != nil {
			RespondError(c, NewError(http.StatusServiceUnavailable, "not_ready", "database unavailable"))
			return
		}
		if err := limiter.Ping(ctx); err != nil {
			RespondError(c, NewError(http.StatusServiceUnavailable, "not_ready", "redis unavailable"))
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
	api := r.Group("/api/v1")
	me := api.Group("/me", authn.Middleware())
	me.GET("", profiles.Me)
	me.POST("/consents", profiles.Accept)
	host := api.Group("/events", authn.Middleware())
	host.POST("", events.Create)
	host.GET("", events.List)
	host.GET("/:id", events.Get)
	pub := api.Group("/public/events/:slug")
	pub.GET("", public.Event)
	pub.POST("/sessions", public.CreateSession)
	pub.POST("/uploads", public.CreateUpload)
	pub.POST("/uploads/:uploadId/complete", public.Complete)
	return r
}
func requestLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		log.Info("http request", "method", c.Request.Method, "path", c.FullPath(), "status", c.Writer.Status(), "duration", time.Since(start).String(), "request_id", c.GetHeader("X-Request-ID"))
	}
}
func recovery(log *slog.Logger) gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered any) {
		log.Error("panic recovered", "error", recovered)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "internal_error", "message": "an unexpected error occurred"}})
	})
}
