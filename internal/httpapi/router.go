package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/cors"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/redis"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type RouterConfig struct {
	Logger         *slog.Logger
	Auth           *auth.Verifier
	Limiter        *redis.Limiter
	AllowedOrigins []string
	DatabaseReady  func(context.Context) error
	Profiles       *profile.Handler
	Events         *event.Handler
	Public         *PublicHandler
}

func NewRouter(cfg RouterConfig) *gin.Engine {
	r := gin.New()
	r.Use(recovery(cfg.Logger), requestID(), requestLog(cfg.Logger), cors.Middleware(cfg.AllowedOrigins))

	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/readyz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := cfg.DatabaseReady(ctx); err != nil {
			apierror.Respond(c, apierror.New(http.StatusServiceUnavailable, "not_ready", "database unavailable"))
			return
		}
		if err := cfg.Limiter.Ping(ctx); err != nil {
			apierror.Respond(c, apierror.New(http.StatusServiceUnavailable, "not_ready", "redis unavailable"))
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	api := r.Group("/api/v1")
	me := api.Group("/me", cfg.Auth.Middleware())
	me.GET("", cfg.Profiles.Me)
	me.POST("/consents", cfg.Profiles.Accept)

	host := api.Group("/events", cfg.Auth.Middleware())
	host.POST("", cfg.Events.Create)
	host.GET("", cfg.Events.List)
	host.GET("/:id", cfg.Events.Get)
	host.PATCH("/:id", cfg.Events.Update)

	pub := api.Group("/public/events/:slug")
	pub.GET("", cfg.Public.Event)
	pub.GET("/media", cfg.Public.Media)
	pub.GET("/media/:mediaId/content", cfg.Public.MediaContent)
	pub.POST("/sessions", cfg.Public.CreateSession)
	pub.POST("/uploads", cfg.Public.CreateUpload)
	pub.POST("/uploads/:uploadId/complete", cfg.Public.Complete)

	return r
}

func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		reqID := c.GetHeader("X-Request-ID")
		if reqID == "" {
			reqID = uuid.NewString()
		}
		c.Set("request_id", reqID)
		c.Header("X-Request-ID", reqID)
		c.Next()
	}
}

func requestLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		path := c.FullPath()
		if path == "" {
			path = c.Request.URL.Path
		}
		reqID := c.GetString("request_id")
		if reqID == "" {
			reqID = c.GetHeader("X-Request-ID")
		}
		log.Info("http request",
			"method", c.Request.Method,
			"path", path,
			"status", c.Writer.Status(),
			"duration", time.Since(start).String(),
			"request_id", reqID,
		)
	}
}

func recovery(log *slog.Logger) gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered any) {
		log.Error("panic recovered", "error", recovered)
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "internal_error", "message": "an unexpected error occurred"}})
	})
}
