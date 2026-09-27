package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	// App
	Env      string
	HTTPAddr string
	LogLevel string

	// Database
	DatabaseURL    string
	DBMaxOpen      int
	DBMaxIdle      int
	DBConnLifetime time.Duration

	// Redis
	RedisURL string

	// Better Auth
	BetterAuthJWKS     string
	BetterAuthIssuer   string
	BetterAuthAudience string

	// CORS
	CORSAllowedOrigins string

	// Legal
	TermsVersion   string
	PrivacyVersion string

	// Cloudflare R2
	R2Endpoint    string
	R2Region      string
	R2Bucket      string
	R2AccessKey   string
	R2Secret      string
	PresignExpiry time.Duration

	// Media Limits
	MaxImageBytes       int64
	MaxVideoBytes       int64
	EventMaxBytes       int64
	UploadRateLimit     int
	RateWindow          time.Duration
	StaleUploadAge      time.Duration
	StaleUploadSchedule string

	// Background workers. Enabled by default so an existing single-process
	// deployment is unchanged; set RUN_WORKER=false on request-serving replicas
	// once a dedicated worker replica is deployed.
	RunWorker              bool
	MediaWorkerConcurrency int
	MediaWorkerIdle        time.Duration
	ExportIdle             time.Duration
	AnalyticsCacheTTL      time.Duration

	// Realtime (SSE). Disabling it unmounts the stream routes and leaves the
	// rest of the API untouched.
	RealtimeEnabled       bool
	RealtimeBuffer        int
	RealtimeHeartbeat     time.Duration
	RealtimeStreamMaxAge  time.Duration
	RealtimeRetry         time.Duration
	RealtimeMaxPerEvent   int
	RealtimeConnectRate   int
	RealtimeConnectWindow time.Duration

	// Weekly Google Drive archive. All values are environment supplied.
	DailyArchiveEnabled   bool
	DailyArchiveSchedule  string
	DailyArchiveMinAge    time.Duration
	GoogleDriveClientID   string
	GoogleDriveSecret     string
	GoogleDriveRefresh    string
	GoogleDriveRootFolder string
}

func Load() (Config, error) {
	_ = godotenv.Load()

	var errs []string
	parseDuration := func(k, d string) time.Duration {
		raw := str(k, d)
		val, err := time.ParseDuration(raw)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s has invalid duration %q: %v", k, raw, err))
		}
		return val
	}
	parseInt := func(k string, d int) int {
		raw := os.Getenv(k)
		if raw == "" {
			return d
		}
		val, err := strconv.Atoi(raw)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s has invalid integer %q: %v", k, raw, err))
			return d
		}
		return val
	}
	parseInt64 := func(k string, d int64) int64 {
		raw := os.Getenv(k)
		if raw == "" {
			return d
		}
		val, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s has invalid int64 %q: %v", k, raw, err))
			return d
		}
		return val
	}
	parseBool := func(k string, d bool) bool {
		raw := os.Getenv(k)
		if raw == "" {
			return d
		}
		val, err := strconv.ParseBool(raw)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s has invalid boolean %q", k, raw))
			return d
		}
		return val
	}

	c := Config{
		Env:      str("APP_ENV", "development"),
		HTTPAddr: str("HTTP_ADDR", ":8080"),
		LogLevel: str("LOG_LEVEL", "info"),

		DatabaseURL:    os.Getenv("DATABASE_URL"),
		DBMaxOpen:      parseInt("DB_MAX_OPEN_CONNS", 25),
		DBMaxIdle:      parseInt("DB_MAX_IDLE_CONNS", 10),
		DBConnLifetime: parseDuration("DB_CONN_MAX_LIFETIME", "30m"),

		RedisURL: os.Getenv("REDIS_URL"),

		BetterAuthJWKS:     os.Getenv("BETTER_AUTH_JWKS_URL"),
		BetterAuthIssuer:   os.Getenv("BETTER_AUTH_ISSUER"),
		BetterAuthAudience: os.Getenv("BETTER_AUTH_AUDIENCE"),

		CORSAllowedOrigins: os.Getenv("CORS_ALLOWED_ORIGINS"),

		TermsVersion:   str("TERMS_VERSION", "2026-01"),
		PrivacyVersion: str("PRIVACY_VERSION", "2026-01"),

		R2Endpoint:    os.Getenv("R2_ENDPOINT"),
		R2Region:      str("R2_REGION", "auto"),
		R2Bucket:      os.Getenv("R2_BUCKET"),
		R2AccessKey:   os.Getenv("R2_ACCESS_KEY_ID"),
		R2Secret:      os.Getenv("R2_SECRET_ACCESS_KEY"),
		PresignExpiry: parseDuration("R2_PRESIGN_EXPIRY", "15m"),

		MaxImageBytes:       parseInt64("UPLOAD_MAX_IMAGE_BYTES", 25<<20),
		MaxVideoBytes:       parseInt64("UPLOAD_MAX_VIDEO_BYTES", 500<<20),
		EventMaxBytes:       parseInt64("EVENT_MAX_MEDIA_BYTES", 5<<30),
		UploadRateLimit:     parseInt("UPLOAD_RATE_LIMIT", 20),
		RateWindow:          parseDuration("UPLOAD_RATE_WINDOW", "1m"),
		StaleUploadAge:      parseDuration("UPLOAD_STALE_AGE", "24h"),
		StaleUploadSchedule: str("UPLOAD_STALE_CLEANUP_SCHEDULE", "*/15 * * * *"),

		RunWorker:              parseBool("RUN_WORKER", true),
		MediaWorkerConcurrency: parseInt("MEDIA_WORKER_CONCURRENCY", 2),
		MediaWorkerIdle:        parseDuration("MEDIA_WORKER_IDLE", "5s"),
		ExportIdle:             parseDuration("EXPORT_WORKER_IDLE", "5s"),
		AnalyticsCacheTTL:      parseDuration("ANALYTICS_CACHE_TTL", "30s"),

		RealtimeEnabled:       parseBool("REALTIME_ENABLED", true),
		RealtimeBuffer:        parseInt("REALTIME_CLIENT_BUFFER", 32),
		RealtimeHeartbeat:     parseDuration("REALTIME_HEARTBEAT", "20s"),
		RealtimeStreamMaxAge:  parseDuration("REALTIME_STREAM_MAX_AGE", "30m"),
		RealtimeRetry:         parseDuration("REALTIME_RETRY", "3s"),
		RealtimeMaxPerEvent:   parseInt("REALTIME_MAX_CONNECTIONS_PER_EVENT", 500),
		RealtimeConnectRate:   parseInt("REALTIME_CONNECT_RATE", 30),
		RealtimeConnectWindow: parseDuration("REALTIME_CONNECT_WINDOW", "1m"),

		DailyArchiveEnabled:   parseBool("MEDIA_DAILY_ARCHIVE_ENABLED", false),
		DailyArchiveSchedule:  str("MEDIA_DAILY_ARCHIVE_SCHEDULE", "0 2 * * *"),
		DailyArchiveMinAge:    parseDuration("MEDIA_DAILY_ARCHIVE_MIN_AGE", "24h"),
		GoogleDriveClientID:   os.Getenv("GOOGLE_DRIVE_CLIENT_ID"),
		GoogleDriveSecret:     os.Getenv("GOOGLE_DRIVE_CLIENT_SECRET"),
		GoogleDriveRefresh:    os.Getenv("GOOGLE_DRIVE_REFRESH_TOKEN"),
		GoogleDriveRootFolder: os.Getenv("GOOGLE_DRIVE_ROOT_FOLDER_ID"),
	}

	var missing []string
	if c.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if c.RedisURL == "" {
		missing = append(missing, "REDIS_URL")
	}
	if c.BetterAuthJWKS == "" {
		missing = append(missing, "BETTER_AUTH_JWKS_URL")
	}
	if c.R2Endpoint == "" {
		missing = append(missing, "R2_ENDPOINT")
	}
	if c.R2Bucket == "" {
		missing = append(missing, "R2_BUCKET")
	}
	if c.Env != "development" && c.R2AccessKey == "" {
		missing = append(missing, "R2_ACCESS_KEY_ID")
	}
	if c.Env != "development" && c.R2Secret == "" {
		missing = append(missing, "R2_SECRET_ACCESS_KEY")
	}
	if c.DailyArchiveEnabled {
		for _, required := range []struct{ name, value string }{
			{"GOOGLE_DRIVE_CLIENT_ID", c.GoogleDriveClientID},
			{"GOOGLE_DRIVE_CLIENT_SECRET", c.GoogleDriveSecret},
			{"GOOGLE_DRIVE_REFRESH_TOKEN", c.GoogleDriveRefresh},
			{"GOOGLE_DRIVE_ROOT_FOLDER_ID", c.GoogleDriveRootFolder},
		} {
			if required.value == "" {
				missing = append(missing, required.name)
			}
		}
	}

	if len(missing) > 0 {
		errs = append(errs, fmt.Sprintf("missing required environment variables: %s", strings.Join(missing, ", ")))
	}

	if len(errs) > 0 {
		return c, fmt.Errorf("configuration errors:\n - %s", strings.Join(errs, "\n - "))
	}

	return c, nil
}

func str(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
