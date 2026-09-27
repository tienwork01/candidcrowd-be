package redis

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The bus is the one piece the realtime hub's unit tests replace with a fake,
// so its behaviour is verified against a real Redis. The suite skips unless
// REDIS_TEST_URL is set, keeping `go test ./...` green without a broker and
// avoiding an accidental connection to a development instance.
//
//	docker compose -f docker-compose.test.yml up -d redis
//	REDIS_TEST_URL=redis://127.0.0.1:6380/0 go test ./internal/platform/redis/
func testBus(t *testing.T) *Bus {
	t.Helper()
	raw := os.Getenv("REDIS_TEST_URL")
	if raw == "" {
		t.Skip("set REDIS_TEST_URL to run the Redis bus tests")
	}
	bus, err := OpenBus(raw)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bus.Close()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, bus.Ping(ctx), "REDIS_TEST_URL is set but unreachable")
	return bus
}

func receivePayload(t *testing.T, stream <-chan []byte) string {
	t.Helper()
	select {
	case payload, ok := <-stream:
		require.True(t, ok, "stream closed before a payload arrived")
		return string(payload)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a payload")
		return ""
	}
}

func TestBusDeliversAcrossInstances(t *testing.T) {
	// Two Bus values stand in for two API instances: the whole point of the
	// bus is that a change observed by one process reaches viewers held by
	// another.
	publisher := testBus(t)
	subscriber := testBus(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	eventID := uuid.New()
	stream, stop, err := subscriber.Subscribe(ctx, eventID)
	require.NoError(t, err)
	defer stop()

	require.NoError(t, publisher.Publish(ctx, eventID, []byte(`{"kind":"media.created"}`)))
	require.JSONEq(t, `{"kind":"media.created"}`, receivePayload(t, stream))
}

func TestBusIsolatesEvents(t *testing.T) {
	bus := testBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watched, other := uuid.New(), uuid.New()
	stream, stop, err := bus.Subscribe(ctx, watched)
	require.NoError(t, err)
	defer stop()

	require.NoError(t, bus.Publish(ctx, other, []byte(`{"kind":"media.deleted"}`)))
	require.NoError(t, bus.Publish(ctx, watched, []byte(`{"kind":"media.created"}`)))

	// Receiving the second payload first proves the first never arrived:
	// one event's traffic must never leak into another event's stream.
	require.JSONEq(t, `{"kind":"media.created"}`, receivePayload(t, stream))
}

func TestBusStopClosesStream(t *testing.T) {
	bus := testBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, stop, err := bus.Subscribe(ctx, uuid.New())
	require.NoError(t, err)

	stop()
	select {
	case _, open := <-stream:
		require.False(t, open, "stop must close the stream")
	case <-time.After(3 * time.Second):
		t.Fatal("stop left the stream open, which would leak a connection per idle event")
	}
	require.NotPanics(t, stop, "stop must be safe to call again from a deferred cleanup")
}

func TestBusContextCancelClosesStream(t *testing.T) {
	bus := testBus(t)
	ctx, cancel := context.WithCancel(context.Background())

	stream, stop, err := bus.Subscribe(ctx, uuid.New())
	require.NoError(t, err)
	defer stop()

	cancel()
	select {
	case _, open := <-stream:
		require.False(t, open, "cancelling the context must close the stream")
	case <-time.After(3 * time.Second):
		t.Fatal("a cancelled context left the stream open")
	}
}

func TestBusSubscribeFailsWhenBrokerIsUnreachable(t *testing.T) {
	if os.Getenv("REDIS_TEST_URL") == "" {
		t.Skip("set REDIS_TEST_URL to run the Redis bus tests")
	}
	// Port 1 never accepts connections. Subscribe confirms the subscription
	// before returning, so an unreachable broker must surface as an error
	// rather than a stream that is silently never delivered.
	bus, err := OpenBus("redis://127.0.0.1:1/0")
	require.NoError(t, err)
	defer func() { _ = bus.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, _, err = bus.Subscribe(ctx, uuid.New())
	require.Error(t, err)
}

func TestBusRejectsMalformedURL(t *testing.T) {
	_, err := OpenBus("not-a-redis-url")
	require.Error(t, err)
}
