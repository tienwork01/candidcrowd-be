package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Env, HTTPAddr, LogLevel, DatabaseURL, RedisURL        string
	DBMaxOpen, DBMaxIdle                                  int
	DBConnLifetime                                        time.Duration
	BetterAuthJWKS, BetterAuthIssuer, BetterAuthAudience  string
	CORSAllowedOrigins                                    string
	TermsVersion, PrivacyVersion                          string
	R2Endpoint, R2Region, R2Bucket, R2AccessKey, R2Secret string
	PresignExpiry, RateWindow                             time.Duration
	MaxImageBytes, MaxVideoBytes, EventMaxBytes           int64
	UploadRateLimit                                       int
}

func Load() (Config, error) {
	_ = godotenv.Load()
	c := Config{Env: str("APP_ENV", "development"), HTTPAddr: str("HTTP_ADDR", ":8080"), LogLevel: str("LOG_LEVEL", "info"), DatabaseURL: os.Getenv("DATABASE_URL"), RedisURL: os.Getenv("REDIS_URL"), DBMaxOpen: integer("DB_MAX_OPEN_CONNS", 25), DBMaxIdle: integer("DB_MAX_IDLE_CONNS", 10), DBConnLifetime: duration("DB_CONN_MAX_LIFETIME", "30m"), BetterAuthJWKS: os.Getenv("BETTER_AUTH_JWKS_URL"), BetterAuthIssuer: os.Getenv("BETTER_AUTH_ISSUER"), BetterAuthAudience: os.Getenv("BETTER_AUTH_AUDIENCE"), CORSAllowedOrigins: os.Getenv("CORS_ALLOWED_ORIGINS"), TermsVersion: str("TERMS_VERSION", "2026-01"), PrivacyVersion: str("PRIVACY_VERSION", "2026-01"), R2Endpoint: os.Getenv("R2_ENDPOINT"), R2Region: str("R2_REGION", "auto"), R2Bucket: os.Getenv("R2_BUCKET"), R2AccessKey: os.Getenv("R2_ACCESS_KEY_ID"), R2Secret: os.Getenv("R2_SECRET_ACCESS_KEY"), PresignExpiry: duration("R2_PRESIGN_EXPIRY", "15m"), MaxImageBytes: int64val("UPLOAD_MAX_IMAGE_BYTES", 25<<20), MaxVideoBytes: int64val("UPLOAD_MAX_VIDEO_BYTES", 500<<20), EventMaxBytes: int64val("EVENT_MAX_MEDIA_BYTES", 5<<30), UploadRateLimit: integer("UPLOAD_RATE_LIMIT", 20), RateWindow: duration("UPLOAD_RATE_WINDOW", "1m")}
	if c.DatabaseURL == "" || c.RedisURL == "" || c.BetterAuthJWKS == "" || c.R2Endpoint == "" || c.R2Bucket == "" {
		return c, fmt.Errorf("required configuration is missing")
	}
	return c, nil
}
func str(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func integer(k string, d int) int {
	v, e := strconv.Atoi(str(k, ""))
	if e != nil {
		return d
	}
	return v
}
func int64val(k string, d int64) int64 {
	v, e := strconv.ParseInt(str(k, ""), 10, 64)
	if e != nil {
		return d
	}
	return v
}
func duration(k, d string) time.Duration {
	v, e := time.ParseDuration(str(k, d))
	if e != nil {
		panic(e)
	}
	return v
}
