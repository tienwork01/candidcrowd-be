package r2_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/platform/r2"
	"github.com/joho/godotenv"
	"github.com/stretchr/testify/require"
)

func TestR2_PipePut(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	_ = godotenv.Load("../../../.env")
	endpoint := os.Getenv("R2_ENDPOINT")
	if endpoint == "" {
		t.Skip("R2 not configured")
	}
	region := os.Getenv("R2_REGION")
	bucket := os.Getenv("R2_BUCKET")
	accessKey := os.Getenv("R2_ACCESS_KEY_ID")
	secret := os.Getenv("R2_SECRET_ACCESS_KEY")

	client, err := r2.New(context.Background(), endpoint, region, bucket, accessKey, secret)
	require.NoError(t, err)

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		_, _ = pw.Write([]byte("hello streamed world from pipe"))
	}()

	testKey := "test/pipe-test-" + time.Now().Format("20060102150405") + ".txt"
	err = client.Put(context.Background(), testKey, "text/plain", pr)
	require.NoError(t, err)

	// Clean up
	_ = client.Delete(context.Background(), testKey)
}

func TestR2_PipePut_Multipart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	_ = godotenv.Load("../../../.env")
	endpoint := os.Getenv("R2_ENDPOINT")
	if endpoint == "" {
		t.Skip("R2 not configured")
	}
	region := os.Getenv("R2_REGION")
	bucket := os.Getenv("R2_BUCKET")
	accessKey := os.Getenv("R2_ACCESS_KEY_ID")
	secret := os.Getenv("R2_SECRET_ACCESS_KEY")

	client, err := r2.New(context.Background(), endpoint, region, bucket, accessKey, secret)
	require.NoError(t, err)

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		chunk := make([]byte, 1024*1024) // 1MB
		for i := 0; i < 7; i++ {           // 7MB total (> 5MB part size)
			if _, werr := pw.Write(chunk); werr != nil {
				return
			}
		}
	}()

	testKey := "test/multipart-test-" + time.Now().Format("20060102150405") + ".bin"
	err = client.Put(context.Background(), testKey, "application/octet-stream", pr)
	require.NoError(t, err)

	info, err := client.Head(context.Background(), testKey)
	require.NoError(t, err)
	require.Equal(t, int64(7*1024*1024), info.Size)

	// Clean up
	_ = client.Delete(context.Background(), testKey)
}

func TestR2_PresignDownload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	_ = godotenv.Load("../../../.env")
	endpoint := os.Getenv("R2_ENDPOINT")
	if endpoint == "" {
		t.Skip("R2 not configured")
	}
	region := os.Getenv("R2_REGION")
	bucket := os.Getenv("R2_BUCKET")
	accessKey := os.Getenv("R2_ACCESS_KEY_ID")
	secret := os.Getenv("R2_SECRET_ACCESS_KEY")

	client, err := r2.New(context.Background(), endpoint, region, bucket, accessKey, secret)
	require.NoError(t, err)

	testKey := "test/disposition-test.txt"
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		_, _ = pw.Write([]byte("test"))
	}()
	err = client.Put(context.Background(), testKey, "text/plain", pr)
	require.NoError(t, err)
	defer client.Delete(context.Background(), testKey)

	url, err := client.PresignDownload(context.Background(), testKey, "friendly-file.txt", 10*time.Minute)
	require.NoError(t, err)

	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()

	cd := resp.Header.Get("Content-Disposition")
	t.Logf("Content-Disposition returned by R2: %s", cd)
	require.Contains(t, cd, "friendly-file.txt")
}


