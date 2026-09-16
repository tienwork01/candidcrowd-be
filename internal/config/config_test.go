package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoad_Success(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/test")
	os.Setenv("REDIS_URL", "redis://localhost:6379/0")
	os.Setenv("BETTER_AUTH_JWKS_URL", "https://example.com/jwks")
	os.Setenv("R2_ENDPOINT", "https://r2.cloudflarestorage.com")
	os.Setenv("R2_BUCKET", "test-bucket")
	defer func() {
		os.Unsetenv("DATABASE_URL")
		os.Unsetenv("REDIS_URL")
		os.Unsetenv("BETTER_AUTH_JWKS_URL")
		os.Unsetenv("R2_ENDPOINT")
		os.Unsetenv("R2_BUCKET")
	}()

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "development", cfg.Env)
	require.Equal(t, ":8080", cfg.HTTPAddr)
	require.Equal(t, 25, cfg.DBMaxOpen)
	require.Equal(t, 15*time.Minute, cfg.PresignExpiry)
}

func TestLoad_MissingRequired(t *testing.T) {
	os.Clearenv()

	_, err := Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing required environment variables")
}

func TestLoad_InvalidValues(t *testing.T) {
	os.Setenv("DATABASE_URL", "postgres://localhost/test")
	os.Setenv("REDIS_URL", "redis://localhost:6379/0")
	os.Setenv("BETTER_AUTH_JWKS_URL", "https://example.com/jwks")
	os.Setenv("R2_ENDPOINT", "https://r2.cloudflarestorage.com")
	os.Setenv("R2_BUCKET", "test-bucket")
	os.Setenv("R2_PRESIGN_EXPIRY", "invalid-duration")
	os.Setenv("DB_MAX_OPEN_CONNS", "not-a-number")
	defer func() {
		os.Unsetenv("DATABASE_URL")
		os.Unsetenv("REDIS_URL")
		os.Unsetenv("BETTER_AUTH_JWKS_URL")
		os.Unsetenv("R2_ENDPOINT")
		os.Unsetenv("R2_BUCKET")
		os.Unsetenv("R2_PRESIGN_EXPIRY")
		os.Unsetenv("DB_MAX_OPEN_CONNS")
	}()

	_, err := Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid duration")
	require.Contains(t, err.Error(), "invalid integer")
}
