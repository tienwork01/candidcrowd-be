package realtime_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/platform/redis"
	"github.com/candidcrowd/candidcrowd-backend/internal/realtime"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The hub's own tests replace the bus with a fake, and the bus tests exercise
// Redis without a hub. This file covers the seam between them: a change
// published on one API instance reaching a connection held by another.
//
//	docker run -d --rm -p 6390:6379 redis:7-alpine
//	REDIS_TEST_URL=redis://127.0.0.1:6390/0 go test ./internal/realtime/
func hubOverRedis(t *testing.T) *realtime.Hub {
	t.Helper()
	raw := os.Getenv("REDIS_TEST_URL")
	if raw == "" {
		t.Skip("set REDIS_TEST_URL to run the realtime integration tests")
	}
	bus, err := redis.OpenBus(raw)
	require.NoError(t, err)

	// Assigning a *redis.Bus here is also the compile-time proof that the
	// platform adapter satisfies the port the hub declares.
	hub := realtime.NewHub(bus, slog.New(slog.NewTextHandler(io.Discard, nil)), realtime.DefaultBuffer)
	t.Cleanup(func() {
		hub.Close()
		require.NoError(t, bus.Close())
	})
	return hub
}

func awaitMessage(t *testing.T, sub *realtime.Subscription) realtime.Message {
	t.Helper()
	select {
	case msg, ok := <-sub.Messages():
		require.True(t, ok, "stream closed before a message arrived")
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a message over Redis")
		return realtime.Message{}
	}
}

func TestHubDeliversAcrossInstancesOverRedis(t *testing.T) {
	publisher := hubOverRedis(t)
	subscriber := hubOverRedis(t)

	eventID := uuid.New()
	host, err := subscriber.Subscribe(eventID, realtime.AudienceHost)
	require.NoError(t, err)
	guest, err := subscriber.Subscribe(eventID, realtime.AudiencePublic)
	require.NoError(t, err)

	// The bus subscription is established off the hot path, so wait for it
	// before publishing rather than racing it.
	require.Eventually(t, func() bool {
		probe, probeErr := realtime.NewMessage(eventID, realtime.KindEventUpdated, realtime.AudienceHost, map[string]bool{"probe": true})
		require.NoError(t, probeErr)
		require.NoError(t, publisher.Publish(context.Background(), probe))
		select {
		case <-host.Messages():
			return true
		case <-time.After(100 * time.Millisecond):
			return false
		}
	}, 5*time.Second, 10*time.Millisecond)

	// A host-only change must not reach the guest stream even when both
	// connections watch the same event on the same instance.
	hidden, err := realtime.NewMessage(eventID, realtime.KindMediaUpdated, realtime.AudienceHost, map[string]string{"status": "hidden"})
	require.NoError(t, err)
	require.NoError(t, publisher.Publish(context.Background(), hidden))
	require.Equal(t, hidden.ID, awaitMessage(t, host).ID)

	shared, err := realtime.NewMessage(eventID, realtime.KindMediaCreated, realtime.AudienceHost|realtime.AudiencePublic, map[string]string{"id": "m1"})
	require.NoError(t, err)
	require.NoError(t, publisher.Publish(context.Background(), shared))

	// The guest receives the shared message and never saw the hidden one.
	require.Equal(t, shared.ID, awaitMessage(t, guest).ID)
	require.Equal(t, shared.ID, awaitMessage(t, host).ID)
}

func TestHubCloseOverRedisReleasesConnections(t *testing.T) {
	hub := hubOverRedis(t)

	sub, err := hub.Subscribe(uuid.New(), realtime.AudienceHost)
	require.NoError(t, err)

	hub.Close()
	select {
	case _, open := <-sub.Messages():
		require.False(t, open, "shutdown must close live streams so graceful shutdown is not blocked")
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown left a Redis-backed stream open")
	}
}
