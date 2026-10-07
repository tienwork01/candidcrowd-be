//go:build integration

// Runs against any S3-compatible store, e.g. MinIO:
//
//	S3_TEST_ENDPOINT=http://127.0.0.1:9000 S3_TEST_BUCKET=test \
//	S3_TEST_ACCESS_KEY=... S3_TEST_SECRET_KEY=... go test -tags integration ./internal/platform/r2/
package r2_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/candidcrowd/candidcrowd-backend/internal/platform/r2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDeletePrefixRemovesOnlyThatEvent(t *testing.T) {
	endpoint := os.Getenv("S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3_TEST_ENDPOINT is not set")
	}
	ctx := context.Background()
	client, err := r2.New(ctx, endpoint, "us-east-1", os.Getenv("S3_TEST_BUCKET"),
		os.Getenv("S3_TEST_ACCESS_KEY"), os.Getenv("S3_TEST_SECRET_KEY"))
	require.NoError(t, err)

	target, other := uuid.New(), uuid.New()
	put := func(key string) {
		require.NoError(t, client.Put(ctx, key, "text/plain", strings.NewReader("x")))
	}
	for range 5 {
		put(fmt.Sprintf("events/%s/media/%s/original.jpg", target, uuid.New()))
	}
	put(fmt.Sprintf("events/%s/exports/%s.zip", target, uuid.New()))
	put(fmt.Sprintf("events/%s/assets/qr-logo/%s.png", target, uuid.New()))
	keep := fmt.Sprintf("events/%s/media/%s/original.jpg", other, uuid.New())
	put(keep)
	t.Cleanup(func() { _ = client.Delete(ctx, keep) })

	deleted, err := client.DeletePrefix(ctx, "events/"+target.String()+"/")
	require.NoError(t, err)
	require.Equal(t, 7, deleted)

	_, err = client.Head(ctx, keep)
	require.NoError(t, err, "objects of another event are untouched")

	again, err := client.DeletePrefix(ctx, "events/"+target.String()+"/")
	require.NoError(t, err)
	require.Zero(t, again, "a second sweep finds nothing and still succeeds")

	for _, unscoped := range []string{"", "/", "events/", "events"} {
		_, err := client.DeletePrefix(ctx, unscoped)
		require.Error(t, err, "prefix %q must be refused", unscoped)
	}
}
