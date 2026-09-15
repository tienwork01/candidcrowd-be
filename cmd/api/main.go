package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/config"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/guest"
	"github.com/candidcrowd/candidcrowd-backend/internal/httpapi"
	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	platformcors "github.com/candidcrowd/candidcrowd-backend/internal/platform/cors"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/database"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/r2"
	redispkg "github.com/candidcrowd/candidcrowd-backend/internal/platform/redis"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/gin-gonic/gin"
)

func main() {
	if err := run(); err != nil {
		slog.Error("application stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Env != "development" {
		gin.SetMode(gin.ReleaseMode)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level(cfg.LogLevel)}))
	db, sqlDB, err := database.Open(cfg.DatabaseURL, cfg.DBMaxOpen, cfg.DBMaxIdle, cfg.DBConnLifetime)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := sqlDB.Close(); closeErr != nil {
			logger.Error("close database", "error", closeErr)
		}
	}()
	limiter, err := redispkg.Open(cfg.RedisURL)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := limiter.Close(); closeErr != nil {
			logger.Error("close redis", "error", closeErr)
		}
	}()
	ctx := context.Background()
	authn, err := auth.New(ctx, cfg.BetterAuthJWKS, cfg.BetterAuthIssuer, cfg.BetterAuthAudience)
	if err != nil {
		return err
	}
	storage, err := r2.New(ctx, cfg.R2Endpoint, cfg.R2Region, cfg.R2Bucket, cfg.R2AccessKey, cfg.R2Secret)
	if err != nil {
		return err
	}
	events := event.NewService(db, cfg.EventMaxBytes)
	profiles := profile.NewService(db, cfg.TermsVersion, cfg.PrivacyVersion)
	eventHandler := event.NewHandler(events, profiles)
	guests := guest.NewService(db, 24*time.Hour)
	uploads := media.NewService(db, storage, limiter, cfg.PresignExpiry, cfg.RateWindow, cfg.UploadRateLimit, cfg.MaxImageBytes, cfg.MaxVideoBytes)
	router := httpapi.NewRouter(logger, authn, limiter, platformcors.Origins(cfg.CORSAllowedOrigins), sqlDB.PingContext, profile.NewHandler(profiles), eventHandler, httpapi.NewPublicHandler(events, guests, uploads))
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: router, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		logger.Info("http server started", "addr", cfg.HTTPAddr)
		if e := server.ListenAndServe(); e != nil && !errors.Is(e, http.ErrServerClosed) {
			logger.Error("http server failed", "error", e)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}
func level(v string) slog.Level {
	if v == "debug" {
		return slog.LevelDebug
	}
	if v == "warn" {
		return slog.LevelWarn
	}
	if v == "error" {
		return slog.LevelError
	}
	return slog.LevelInfo
}
