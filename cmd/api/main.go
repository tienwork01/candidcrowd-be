package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/archive"
	"github.com/candidcrowd/candidcrowd-backend/internal/billing"
	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/config"
	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
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
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/paddle"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/r2"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/redis"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/candidcrowd/candidcrowd-backend/internal/realtime"
	"github.com/candidcrowd/candidcrowd-backend/internal/retention"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
	logWriter := io.Writer(os.Stdout)
	if cfg.Env == "development" {
		logFile, openErr := os.OpenFile("tmp/api.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if openErr == nil {
			defer logFile.Close()
			logWriter = io.MultiWriter(os.Stdout, logFile)
		}
	}
	logger := slog.New(slog.NewJSONHandler(logWriter, &slog.HandlerOptions{Level: level(cfg.LogLevel)}))
	slog.SetDefault(logger)
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

	planCatalog := catalog.NewService(catalog.NewGormRepository(db))
	// Every event is created together with its Free grant, in one transaction.
	events := event.NewService(event.NewGormRepository(db, entitlement.NewProvisioner(planCatalog)), cfg.EventMaxBytes, eventNotifiers...).
		WithLimits(cfg.MaxActiveEventsPerHost, cfg.MaxTrialEventsPer30Days, cfg.EventCreationLimitsEnabled)
	entitlements := entitlement.NewService(entitlement.NewGormRepository(db), planCatalog,
		entitlement.WithEnforcement(cfg.EntitlementEnforcementEnabled), entitlement.WithLogger(logger))
	logger.Info("event plans", "enforcement", cfg.EntitlementEnforcementEnabled)
	events.UsePlanGate(entitlements)
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
	// The Drive client is a read dependency of the public gallery and bulk exports,
	// not only of the archive job: once an original has been archived, serving it is the
	// only way a guest or host can still retrieve that media. It is therefore built on
	// every replica that has archiving configured, worker or not.
	var driveReader *googledrive.Client
	var archiveCron *cron.Cron
	if cfg.DailyArchiveEnabled {
		driveClient, driveErr := googledrive.New(ctx, cfg.GoogleDriveClientID, cfg.GoogleDriveSecret, cfg.GoogleDriveRefresh, cfg.GoogleDriveRootFolder)
		if driveErr != nil {
			return driveErr
		}
		driveReader = driveClient
	}

	var exportOpts []any
	if logger != nil {
		exportOpts = append(exportOpts, logger)
	}
	if driveReader != nil {
		exportOpts = append(exportOpts, exportjob.WithArchiveReader(driveReader))
	}
	exports := exportjob.NewService(exportjob.NewGormRepository(db), storage, exportOpts...)
	liveWall := livewall.NewService(livewall.NewGormRepository(db), 12*time.Hour, liveWallNotifiers...)

	// The plan registry decides which plans exist, their upgrade order and which
	// can be sold. Loading it from the database is what lets a new tier ship as
	// data; a failure keeps the compiled-in catalog rather than refusing to boot.
	if err := catalog.LoadRegistry(context.Background(), db); err != nil {
		logger.Warn("using the built-in plan registry", "error", err)
	}
	logger.Info("plan registry loaded", "sellable", catalog.SellablePlans())

	var (
		billingService *billing.Service
		billingHandler *billing.Handler
	)
	if cfg.BillingEnabled {
		licenseService := entitlement.NewPurchaseLicenseService(db)
		grantAdapter := &entitlementGrantAdapter{licenses: licenseService}
		catReader := &catalogReaderAdapter{catalog: planCatalog, db: db}
		ownerAdapter := &eventOwnershipAdapter{events: events, db: db}

		var checkoutGateway billing.CheckoutGateway
		var webhookDecoder billing.WebhookDecoder
		if cfg.BillingProvider == "paddle" {
			paddleClient := paddle.NewClient(cfg.PaddleEnvironment, cfg.PaddleAPIKey)
			checkoutGateway = paddle.NewCheckoutAdapter(paddleClient, cfg.PaddleCheckoutURL)
			webhookDecoder = paddle.NewWebhookVerifier(cfg.PaddleWebhookSecret)
		}

		billingService = billing.NewService(billing.Config{
			Repo:       billing.NewGormRepository(db),
			Checkout:   checkoutGateway,
			Decoder:    webhookDecoder,
			Grants:     grantAdapter,
			Licenses:   &billingReversalAdapter{licenses: licenseService, db: db},
			Plans:      catReader,
			Ownership:  ownerAdapter,
			EntSvc:     entitlements,
			Provider:   billing.Provider(cfg.BillingProvider),
			Currency:   cfg.BillingCurrency,
			Enabled:    cfg.BillingEnabled,
			SuccessURL: cfg.BillingSuccessURL,
			CancelURL:  cfg.BillingCancelURL,
			Logger:     logger,
		})

		billingHandler = billing.NewHandler(billingService, func(c *gin.Context) (uuid.UUID, error) {
			identity, err := auth.Get(c)
			if err != nil {
				return uuid.Nil, err
			}
			return profiles.UserID(c.Request.Context(), identity)
		})
		logger.Info("billing enabled", "provider", cfg.BillingProvider, "currency", cfg.BillingCurrency)
		// A missing product mapping only breaks checkout, so it must not stop
		// the API from serving everything else. It is loud instead of fatal.
		if gaps := billingService.ExecutionGaps(context.Background()); len(gaps) > 0 {
			logger.Error("billing cannot charge for some plans: no provider product mapping",
				"plans", gaps, "fix", "planctl product-map set --plan <code> --product-id <provider product>")
		}
	}

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

		if billingService != nil {
			if _, scheduleErr := cleanupCron.AddFunc(cfg.BillingReconcileSchedule, func() {
				jobCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				if applied, errs := billingService.ReconcileUnapplied(jobCtx, cfg.BillingReconcileBatch); len(errs) > 0 {
					logger.Error("billing reconciliation error", "errors", errs)
				} else if applied > 0 {
					logger.Info("billing reconciliation applied grants", "count", applied)
				}
			}); scheduleErr != nil {
				return scheduleErr
			}
			logger.Info("billing reconciliation scheduled", "schedule", cfg.BillingReconcileSchedule)
		}

		cleanupCron.Start()
		defer cleanupCron.Stop()

		// Retention runs daily. Off by default, it only logs which events are
		// past their storage period; turning it on deletes their media.
		retentionOpts := []retention.Option{
			retention.WithEnforcement(cfg.RetentionEnforcementEnabled),
			retention.WithGrace(cfg.RetentionGrace),
			retention.WithLogger(logger),
		}
		if driveReader != nil {
			retentionOpts = append(retentionOpts, retention.WithArchive(driveReader))
		}
		retentionService := retention.NewService(retention.NewGormRepository(db), storage, retentionOpts...)
		retentionCron := cron.New()
		if _, scheduleErr := retentionCron.AddFunc(cfg.RetentionSchedule, func() {
			jobCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			if _, jobErr := retentionService.Run(jobCtx, cfg.RetentionBatch, false); jobErr != nil {
				logger.Error("retention run failed", "error", jobErr)
			}
		}); scheduleErr != nil {
			return scheduleErr
		}
		retentionCron.Start()
		defer retentionCron.Stop()
		logger.Info("retention job scheduled", "schedule", cfg.RetentionSchedule, "enforcement", cfg.RetentionEnforcementEnabled, "grace", cfg.RetentionGrace)

		if driveReader != nil {
			archiveService := archive.New(database.NewArchiveRepository(db), storage, driveReader, cfg.DailyArchiveMinAge)
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

		logger.Info("background workers enabled", "media_concurrency", cfg.MediaWorkerConcurrency)
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
	publicHandler := httpapi.NewPublicHandler(events, guests, uploads, analytics, archiveReaders...)
	publicHandler.UsePlans(entitlements, cfg.EventMaxBytes)
	var catalogHandler *catalog.Handler
	if cfg.PlansCatalogEnabled {
		catalogHandler = catalog.NewHandler(planCatalog, catalog.Rollout{
			PricingUIEnabled:        cfg.PlansPricingUIEnabled,
			ManualActivationEnabled: cfg.ManualPlanActivationEnabled,
			EnforcementEnabled:      cfg.EntitlementEnforcementEnabled,
			CheckoutEnabled:         cfg.BillingEnabled,
		})
	}
	router := httpapi.NewRouter(httpapi.RouterConfig{
		Logger:                   logger,
		Auth:                     authn,
		Limiter:                  limiter,
		RateLimitEventCreateIP:   cfg.RateLimitEventCreateIP,
		RateLimitEventCreateHost: cfg.RateLimitEventCreateHost,
		AllowedOrigins:           cors.Origins(cfg.CORSAllowedOrigins),
		DatabaseReady:            sqlDB.PingContext,
		Profiles:                 profile.NewHandler(profiles),
		Events:                   eventHandler,
		HostMedia:                httpapi.NewHostMediaHandler(events, profiles, uploads, archiveReaders...).UsePlans(entitlements),
		Insights:                 httpapi.NewInsightsHandler(events, profiles, analytics).UsePlans(entitlements),
		Exports:                  httpapi.NewExportHandler(events, profiles, exports).UsePlans(entitlements),
		LiveWall:                 httpapi.NewLiveWallHandler(events, profiles, uploads, liveWall, realtimeHub).UsePlans(entitlements),
		Public:                   publicHandler,
		QRLogo:                   httpapi.NewQRLogoHandler(events, profiles, storage, cfg.PresignExpiry).UsePlans(entitlements),
		Plans:                    httpapi.NewPlanHandler(events, profiles, entitlements),
		Catalog:                  catalogHandler,
		Billing:                  billingHandler,
		Stream:                   streamHandler(cfg, events, profiles, realtimeHub, limiter),
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
