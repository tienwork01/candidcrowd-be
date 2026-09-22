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

	"github.com/candidcrowd/candidcrowd-backend/internal/archive"
	"github.com/candidcrowd/candidcrowd-backend/internal/config"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/guest"
	"github.com/candidcrowd/candidcrowd-backend/internal/httpapi"
	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/cors"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/database"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/googledrive"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/r2"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/redis"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/gin-gonic/gin"
	"github.com/robfig/cron/v3"
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
	limiter, err := redis.Open(cfg.RedisURL)
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
	events := event.NewService(event.NewGormRepository(db), cfg.EventMaxBytes)
	profiles := profile.NewService(profile.NewGormRepository(db), cfg.TermsVersion, cfg.PrivacyVersion)
	eventHandler := event.NewHandler(events, profiles)
	guests := guest.NewService(guest.NewGormRepository(db), 24*time.Hour)
	uploads := media.NewService(database.NewMediaRepository(db), storage, limiter, cfg.PresignExpiry, cfg.RateWindow, cfg.UploadRateLimit, cfg.MaxImageBytes, cfg.MaxVideoBytes)
	var driveReader *googledrive.Client
	var archiveCron *cron.Cron
	if cfg.DailyArchiveEnabled {
		driveClient, driveErr := googledrive.New(ctx, cfg.GoogleDriveClientID, cfg.GoogleDriveSecret, cfg.GoogleDriveRefresh, cfg.GoogleDriveRootFolder)
		if driveErr != nil {
			return driveErr
		}
		driveReader = driveClient
		archiveService := archive.New(database.NewArchiveRepository(db), storage, driveClient, cfg.DailyArchiveMinAge)
		archiveCron = cron.New()
		if _, scheduleErr := archiveCron.AddFunc(cfg.DailyArchiveSchedule, func() {
			jobCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			if jobErr := archiveService.Run(jobCtx, 50); jobErr != nil {
				logger.Error("daily media archive failed", "error", jobErr)
			}
		}); scheduleErr != nil {
			return scheduleErr
		}
		archiveCron.Start()
		logger.Info("daily media archive enabled", "schedule", cfg.DailyArchiveSchedule)
	}
	if archiveCron != nil {
		defer archiveCron.Stop()
	}
	router := httpapi.NewRouter(httpapi.RouterConfig{
		Logger:         logger,
		Auth:           authn,
		Limiter:        limiter,
		AllowedOrigins: cors.Origins(cfg.CORSAllowedOrigins),
		DatabaseReady:  sqlDB.PingContext,
		Profiles:       profile.NewHandler(profiles),
		Events:         eventHandler,
		Public:         httpapi.NewPublicHandler(events, guests, uploads, driveReader),
	})
	server := &http.Server{Addr: cfg.HTTPAddr, Handler: router, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		logger.Info("http server started", "addr", cfg.HTTPAddr)
		if serverErr := server.ListenAndServe(); serverErr != nil && !errors.Is(serverErr, http.ErrServerClosed) {
			logger.Error("http server failed", "error", serverErr)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}
func level(levelStr string) slog.Level {
	if levelStr == "debug" {
		return slog.LevelDebug
	}
	if levelStr == "warn" {
		return slog.LevelWarn
	}
	if levelStr == "error" {
		return slog.LevelError
	}
	return slog.LevelInfo
}
