package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/candidcrowd/candidcrowd-backend/internal/billing"
	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/cors"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/redis"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type RouterConfig struct {
	Logger                   *slog.Logger
	Auth                     *auth.Verifier
	Limiter                  *redis.Limiter
	RateLimitEventCreateIP   int
	RateLimitEventCreateHost int
	AllowedOrigins           []string
	DatabaseReady            func(context.Context) error
	Profiles                 *profile.Handler
	Events                   *event.Handler
	HostMedia                *HostMediaHandler
	Insights                 *InsightsHandler
	Exports                  *ExportHandler
	LiveWall                 *LiveWallHandler
	Public                   *PublicHandler
	QRLogo                   *QRLogoHandler
	Plans                    *PlanHandler
	// Catalog is nil when PLANS_CATALOG_ENABLED is off, and /plans is not mounted.
	Catalog *catalog.Handler
	// Billing is nil when BILLING_ENABLED is off, and checkout routes are not mounted.
	Billing *billing.Handler
	// Stream is nil when realtime is disabled for the deployment, in which
	// case the stream routes are simply not mounted.
	Stream *StreamHandler
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
	me.GET("/event-plans", cfg.Plans.HostEventPlans)
	if cfg.Catalog != nil {
		api.GET("/plans", cfg.Catalog.List)
	}

	host := api.Group("/events", cfg.Auth.Middleware())

	var createHandlers []gin.HandlerFunc
	if cfg.Limiter != nil {
		ipLimit := cfg.RateLimitEventCreateIP
		if ipLimit <= 0 {
			ipLimit = 10
		}
		hostLimit := cfg.RateLimitEventCreateHost
		if hostLimit <= 0 {
			hostLimit = 2
		}
		createHandlers = append(createHandlers,
			rateLimitMiddleware(cfg.Limiter, "rate:ip:event_create", resolveClientIP, ipLimit, time.Minute),
			rateLimitMiddleware(cfg.Limiter, "rate:host:event_create", func(c *gin.Context) string {
				identity, err := auth.Get(c)
				if err == nil && identity.BetterAuthUserID != "" {
					return identity.BetterAuthUserID
				}
				return resolveClientIP(c)
			}, hostLimit, time.Minute),
		)
	}
	createHandlers = append(createHandlers, cfg.Events.Create)
	host.POST("", createHandlers...)
	host.POST("/:id/close", cfg.Events.Close)
	host.GET("", cfg.Events.List)
	host.GET("/:id", cfg.Events.Get)
	host.PATCH("/:id", cfg.Events.Update)
	host.POST("/:id/qr-logo", cfg.QRLogo.Upload)
	host.POST("/:id/qr-logo/upload-target", cfg.QRLogo.CreateUploadTarget)
	host.POST("/:id/qr-logo/:assetId/complete", cfg.QRLogo.Complete)
	host.DELETE("/:id", cfg.Events.Delete)
	host.GET("/:id/plan", cfg.Plans.EventPlan)
	host.GET("/:id/usage", cfg.Plans.EventUsage)
	host.POST("/:id/exports", cfg.Exports.Create)
	host.GET("/:id/exports/latest", cfg.Exports.Latest)
	host.GET("/:id/exports/:exportId", cfg.Exports.Get)
	host.GET("/:id/analytics", cfg.Insights.Analytics)
	host.GET("/:id/qr-sources", cfg.Insights.ListSources)
	host.POST("/:id/qr-sources", cfg.Insights.CreateSource)
	host.PATCH("/:id/qr-sources/:sourceId", cfg.Insights.UpdateSource)
	host.DELETE("/:id/qr-sources/:sourceId", cfg.Insights.DeleteSource)
	host.GET("/:id/media", cfg.HostMedia.List)
	host.GET("/:id/media/:mediaId/content", cfg.HostMedia.Content)
	host.PATCH("/:id/media/:mediaId", cfg.HostMedia.UpdateStatus)
	host.POST("/:id/media/batch-status", cfg.HostMedia.BatchUpdateStatus)
	host.DELETE("/:id/media/:mediaId", cfg.HostMedia.Delete)
	host.POST("/:id/media/batch-delete", cfg.HostMedia.BatchDelete)
	host.POST("/:id/live-wall-sessions", cfg.LiveWall.Create)
	host.GET("/:id/live-wall-sessions/:sessionId", cfg.LiveWall.Get)
	host.PATCH("/:id/live-wall-sessions/:sessionId", cfg.LiveWall.Update)
	host.POST("/:id/live-wall-sessions/:sessionId/end", cfg.LiveWall.End)
	host.POST("/:id/live-wall-sessions/:sessionId/commands", cfg.LiveWall.Command)
	if cfg.Billing != nil {
		host.GET("/:id/offers", cfg.Billing.Offers)
		host.POST("/:id/checkout", cfg.Billing.CreateCheckout)
		billingGroup := api.Group("/billing", cfg.Auth.Middleware())
		billingGroup.GET("/purchases/:purchaseId", cfg.Billing.GetPurchase)
		api.POST("/webhooks/paddle", cfg.Billing.Webhook)
	}

	pub := api.Group("/public/events/:slug")
	pub.GET("", cfg.Public.Event)
	if cfg.Stream != nil {
		host.GET("/:id/stream", cfg.Stream.Host)
		pub.GET("/stream", cfg.Stream.Public)
	}
	pub.GET("/media", cfg.Public.Media)
	pub.GET("/qr-logo/:assetId", cfg.QRLogo.Content)
	pub.GET("/media/:mediaId/content", cfg.Public.MediaContent)
	pub.POST("/sessions", cfg.Public.CreateSession)
	pub.POST("/uploads", cfg.Public.CreateUpload)
	pub.POST("/uploads/:uploadId/complete", cfg.Public.Complete)
	// Batch confirmation. The per-item route above is kept so existing clients
	// keep working; this one spares a guest one round trip per photo.
	pub.POST("/uploads/complete", cfg.Public.CompleteBatch)
	r.GET("/api/v1/public/live-wall-sessions/:token", cfg.LiveWall.Player)
	r.GET("/api/v1/public/live-wall-sessions/:token/stream", cfg.LiveWall.Stream)
	r.GET("/api/v1/public/live-wall-sessions/:token/media/:mediaId/content", cfg.LiveWall.Content)

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

func resolveClientIP(c *gin.Context) string {
	if cfIP := c.GetHeader("CF-Connecting-IP"); cfIP != "" {
		return cfIP
	}
	return c.ClientIP()
}

func rateLimitMiddleware(limiter *redis.Limiter, keyPrefix string, keyFunc func(c *gin.Context) string, limit int, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if limiter == nil || limit <= 0 {
			c.Next()
			return
		}
		key := keyPrefix + ":" + keyFunc(c)
		allowed, err := limiter.Allow(c.Request.Context(), key, limit, window)
		if err != nil {
			// Fail-open: Redis connectivity issue must not block valid users
			c.Next()
			return
		}
		if !allowed {
			apierror.Respond(c, apierror.New(http.StatusTooManyRequests, "rate_limit_exceeded", "too many requests, please try again later"))
			c.Abort()
			return
		}
		c.Next()
	}
}
