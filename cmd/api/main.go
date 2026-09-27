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
	"github.com/candidcrowd/candidcrowd-backend/internal/exportjob"
	"github.com/candidcrowd/candidcrowd-backend/internal/guest"
	"github.com/candidcrowd/candidcrowd-backend/internal/httpapi"
	"github.com/candidcrowd/candidcrowd-backend/internal/insights"
	"github.com/candidcrowd/candidcrowd-backend/internal/livewall"
	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/candidcrowd/candidcrowd-backend/internal/mediajob"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/cors"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/database"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/googledrive"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/r2"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/redis"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/candidcrowd/candidcrowd-backend/internal/realtime"
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
	// Realtime is optional. When it is off, no bus connection is opened, no
	// notifier is attached, and the stream routes are never mounted.
	var (
		realtimeHub       *realtime.Hub
		realtimeBus       *redis.Bus
		eventNotifiers    []event.Notifier
		mediaNotifiers    []media.Notifier
		liveWallNotifiers []livewall.Notifier
	)
	if cfg.RealtimeEnabled {
		bus, busErr := redis.OpenBus(cfg.RedisURL)
		if busErr != nil {
			return busErr
		}
		realtimeBus = bus
		realtimeHub = realtime.NewHub(bus, logger, cfg.RealtimeBuffer)
		notifier := realtime.NewNotifier(realtimeHub, logger)
		eventNotifiers = append(eventNotifiers, notifier)
		mediaNotifiers = append(mediaNotifiers, notifier)
		liveWallNotifiers = append(liveWallNotifiers, notifier)
		logger.Info("realtime enabled", "heartbeat", cfg.RealtimeHeartbeat, "stream_max_age", cfg.RealtimeStreamMaxAge)
	}
	defer func() {
		if realtimeBus != nil {
			if closeErr := realtimeBus.Close(); closeErr != nil {
				logger.Error("close realtime bus", "error", closeErr)
			}
		}
	}()

	events := event.NewService(event.NewGormRepository(db), cfg.EventMaxBytes, eventNotifiers...)
	profiles := profile.NewService(profile.NewGormRepository(db), cfg.TermsVersion, cfg.PrivacyVersion)
	eventHandler := event.NewHandler(events, profiles)
	guests := guest.NewService(guest.NewGormRepository(db), 24*time.Hour)
	uploads := media.NewService(database.NewMediaRepository(db), storage, limiter, cfg.PresignExpiry, cfg.RateWindow, cfg.UploadRateLimit, cfg.MaxImageBytes, cfg.MaxVideoBytes, mediaNotifiers...)
	// Analytics runs three full-event aggregates. A host dashboard refreshes
	// far more often than those numbers change, so repeated reads are served
	// from Redis for a short window.
	analyticsCache, cacheErr := redis.OpenCache(cfg.RedisURL)
	if cacheErr != nil {
		return cacheErr
	}
	defer func() {
		if closeErr := analyticsCache.Close(); closeErr != nil {
			logger.Error("close analytics cache", "error", closeErr)
		}
	}()
	analytics := insights.NewService(insights.NewGormRepository(db),
		insights.WithCache(analyticsCache, cfg.AnalyticsCacheTTL))
	exports := exportjob.NewService(exportjob.NewGormRepository(db), storage, logger)
	liveWall := livewall.NewService(livewall.NewGormRepository(db), 12*time.Hour, liveWallNotifiers...)
	// Background work is gated so it can be moved off the request-serving
	// replicas without a second binary: run one replica with RUN_WORKER=true
	// and set RUN_WORKER=false on the rest. It defaults to on, so a
	// single-process deployment behaves exactly as before.
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()

	if cfg.RunWorker {
		// Thumbnail rendering decodes whole images, so it runs from a durable
		// queue with a bounded pool rather than a goroutine per upload.
		mediaWorker := mediajob.NewWorker(mediajob.NewGormRepository(db), uploads, logger, mediajob.Config{
			Concurrency: cfg.MediaWorkerConcurrency,
			Idle:        cfg.MediaWorkerIdle,
		})
		go mediaWorker.Run(workerCtx)

		// A loop rather than a cron: exports are claimed back to back so a
		// queue drains at once, and only one runs at a time so a ZIP being
		// streamed through this process cannot overlap with the next tick.
		go exports.Run(workerCtx, cfg.ExportIdle)

		cleanupCron := cron.New()
		if _, scheduleErr := cleanupCron.AddFunc(cfg.StaleUploadSchedule, func() {
			jobCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if jobErr := uploads.ExpireStale(jobCtx, time.Now().Add(-cfg.StaleUploadAge), 100); jobErr != nil {
				logger.Error("stale upload cleanup failed", "error", jobErr)
			}
		}); scheduleErr != nil {
			return scheduleErr
		}
		cleanupCron.Start()
		defer cleanupCron.Stop()

		logger.Info("background workers enabled", "media_concurrency", cfg.MediaWorkerConcurrency)
	}

	// The Drive client is a read dependency of the public gallery, not only of
	// the archive job: once an original has been archived, serving it is the
	// only way a guest can still see that media. It is therefore built on
	// every replica that has archiving configured, worker or not.
	var driveReader *googledrive.Client
	var archiveCron *cron.Cron
	if cfg.DailyArchiveEnabled {
		driveClient, driveErr := googledrive.New(ctx, cfg.GoogleDriveClientID, cfg.GoogleDriveSecret, cfg.GoogleDriveRefresh, cfg.GoogleDriveRootFolder)
		if driveErr != nil {
			return driveErr
		}
		driveReader = driveClient
		if cfg.RunWorker {
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
	}
	if archiveCron != nil {
		defer archiveCron.Stop()
	}
	// Built as a slice rather than passed straight through: driveReader is a
	// typed nil pointer when archiving is off, and a typed nil placed in an
	// interface is not itself nil, so handlers would see a reader that is
	// present but unusable.
	var archiveReaders []httpapi.ArchiveReader
	if driveReader != nil {
		archiveReaders = append(archiveReaders, driveReader)
	}
	router := httpapi.NewRouter(httpapi.RouterConfig{
		Logger:         logger,
		Auth:           authn,
		Limiter:        limiter,
		AllowedOrigins: cors.Origins(cfg.CORSAllowedOrigins),
		DatabaseReady:  sqlDB.PingContext,
		Profiles:       profile.NewHandler(profiles),
		Events:         eventHandler,
		HostMedia:      httpapi.NewHostMediaHandler(events, profiles, uploads, archiveReaders...),
		Insights:       httpapi.NewInsightsHandler(events, profiles, analytics),
		Exports:        httpapi.NewExportHandler(events, profiles, exports),
		LiveWall:       httpapi.NewLiveWallHandler(events, profiles, uploads, liveWall, realtimeHub),
		Public:         httpapi.NewPublicHandler(events, guests, uploads, analytics, archiveReaders...),
		Stream:         streamHandler(cfg, events, profiles, realtimeHub, limiter),
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
	// Live streams are long-lived by design and would otherwise hold
	// Shutdown open for its whole timeout. Release them first; every client
	// reconnects to the next instance.
	if realtimeHub != nil {
		realtimeHub.Close()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}

func streamHandler(cfg config.Config, events *event.Service, profiles *profile.Service, hub *realtime.Hub, limiter *redis.Limiter) *httpapi.StreamHandler {
	if hub == nil {
		return nil
	}
	return httpapi.NewStreamHandler(events, profiles, hub, limiter, httpapi.StreamConfig{
		Heartbeat:     cfg.RealtimeHeartbeat,
		MaxAge:        cfg.RealtimeStreamMaxAge,
		Retry:         cfg.RealtimeRetry,
		MaxPerEvent:   cfg.RealtimeMaxPerEvent,
		ConnectRate:   cfg.RealtimeConnectRate,
		ConnectWindow: cfg.RealtimeConnectWindow,
	})
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
