package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/realtime"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// --- test doubles -----------------------------------------------------------

type stubEventRepo struct {
	public map[string]event.Event
}

func (r *stubEventRepo) Create(context.Context, *event.Event) error { return nil }
func (r *stubEventRepo) GetByClientRequest(context.Context, uuid.UUID, uuid.UUID) (event.Event, error) {
	return event.Event{}, event.ErrNotFound
}
func (r *stubEventRepo) ListByHost(context.Context, uuid.UUID, event.ListFilter) ([]event.Event, int64, error) {
	return nil, 0, nil
}
func (r *stubEventRepo) GetOwned(context.Context, uuid.UUID, uuid.UUID) (event.Event, error) {
	return event.Event{}, event.ErrNotFound
}
func (r *stubEventRepo) GetPublic(_ context.Context, slug string) (event.Event, error) {
	evt, ok := r.public[slug]
	if !ok {
		return event.Event{}, event.ErrNotFound
	}
	return evt, nil
}
func (r *stubEventRepo) UpdateOwned(context.Context, uuid.UUID, uuid.UUID, map[string]interface{}) (event.Event, error) {
	return event.Event{}, event.ErrNotFound
}
func (r *stubEventRepo) DeleteOwned(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (r *stubEventRepo) CloseOwned(context.Context, uuid.UUID, uuid.UUID) error  { return nil }
func (r *stubEventRepo) CountActiveTrialsByHost(context.Context, uuid.UUID) (int64, error) {
	return 0, nil
}
func (r *stubEventRepo) CountTrialsSince(context.Context, uuid.UUID, time.Time) (int64, error) {
	return 0, nil
}

// loopbackBus is an in-process realtime bus, enough to drive the transport.
type loopbackBus struct {
	mu        sync.Mutex
	listeners map[uuid.UUID][]chan []byte
}

func newLoopbackBus() *loopbackBus {
	return &loopbackBus{listeners: make(map[uuid.UUID][]chan []byte)}
}

func (b *loopbackBus) Publish(_ context.Context, eventID uuid.UUID, payload []byte) error {
	b.mu.Lock()
	targets := append([]chan []byte(nil), b.listeners[eventID]...)
	b.mu.Unlock()
	for _, target := range targets {
		select {
		case target <- payload:
		default:
		}
	}
	return nil
}

func (b *loopbackBus) Subscribe(ctx context.Context, eventID uuid.UUID) (<-chan []byte, func(), error) {
	ch := make(chan []byte, 16)
	b.mu.Lock()
	b.listeners[eventID] = append(b.listeners[eventID], ch)
	b.mu.Unlock()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			b.mu.Lock()
			remaining := make([]chan []byte, 0, len(b.listeners[eventID]))
			for _, existing := range b.listeners[eventID] {
				if existing != ch {
					remaining = append(remaining, existing)
				}
			}
			b.listeners[eventID] = remaining
			b.mu.Unlock()
		})
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ch, stop, nil
}

func (b *loopbackBus) subscriberCount(eventID uuid.UUID) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.listeners[eventID])
}

type denyingLimiter struct{}

func (denyingLimiter) Allow(context.Context, string, int, time.Duration) (bool, error) {
	return false, nil
}

// --- harness ----------------------------------------------------------------

type streamHarness struct {
	router *gin.Engine
	hub    *realtime.Hub
	bus    *loopbackBus
	evt    event.Event
}

func newStreamHarness(t *testing.T, cfg StreamConfig, limiter StreamLimiter, galleryEnabled bool) *streamHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)

	evt := event.Event{ID: uuid.New(), Slug: "evt_test", GalleryEnabled: galleryEnabled, Status: event.StatusActive}
	events := event.NewService(&stubEventRepo{public: map[string]event.Event{evt.Slug: evt}}, 1<<30)

	bus := newLoopbackBus()
	hub := realtime.NewHub(bus, slog.New(slog.NewTextHandler(io.Discard, nil)), realtime.DefaultBuffer)
	t.Cleanup(hub.Close)

	router := NewRouter(RouterConfig{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		DatabaseReady: func(context.Context) error { return nil },
		Stream:        NewStreamHandler(events, nil, hub, limiter, cfg),
	})
	return &streamHarness{router: router, hub: hub, bus: bus, evt: evt}
}

// run serves the public stream until the deadline fires, then returns the
// raw SSE body. publish runs once the connection is established.
func (h *streamHarness) run(t *testing.T, publish func()) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/public/events/"+h.evt.Slug+"/stream", nil).WithContext(ctx)
	res := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.router.ServeHTTP(res, req)
	}()

	if publish != nil {
		require.Eventually(t, func() bool { return h.bus.subscriberCount(h.evt.ID) > 0 }, 2*time.Second, 5*time.Millisecond)
		publish()
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("stream did not close on its own")
	}
	return res.Body.String()
}

func publicMessage(t *testing.T, eventID uuid.UUID, kind realtime.Kind, payload any) realtime.Message {
	t.Helper()
	msg, err := realtime.NewMessage(eventID, kind, realtime.AudiencePublic, payload)
	require.NoError(t, err)
	return msg
}

// --- tests ------------------------------------------------------------------

func TestPublicStreamSendsSSEHeadersAndRetry(t *testing.T) {
	h := newStreamHarness(t, StreamConfig{MaxAge: 120 * time.Millisecond, Retry: 3 * time.Second}, nil, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/public/events/"+h.evt.Slug+"/stream", nil)
	res := httptest.NewRecorder()
	h.router.ServeHTTP(res, req)

	require.Equal(t, http.StatusOK, res.Code)
	require.Equal(t, "text/event-stream", res.Header().Get("Content-Type"))
	// no-transform and X-Accel-Buffering are what stop a proxy from holding
	// the body until the connection closes.
	require.Equal(t, "no-cache, no-transform", res.Header().Get("Cache-Control"))
	require.Equal(t, "no", res.Header().Get("X-Accel-Buffering"))
	require.Contains(t, res.Body.String(), "retry: 3000\n\n")
}

func TestPublicStreamWritesMessagesAsSSEFrames(t *testing.T) {
	h := newStreamHarness(t, StreamConfig{MaxAge: 300 * time.Millisecond}, nil, true)

	var sent realtime.Message
	body := h.run(t, func() {
		sent = publicMessage(t, h.evt.ID, realtime.KindMediaCreated, map[string]any{"ids": []string{"abc"}})
		require.NoError(t, h.hub.Publish(context.Background(), sent))
	})

	require.Contains(t, body, "id: "+sent.ID+"\n")
	require.Contains(t, body, "event: media.created\n")
	require.Contains(t, body, `data: {"ids":["abc"]}`)
	// Every frame must end with a blank line or the browser never dispatches it.
	require.True(t, strings.HasSuffix(body, "\n\n"))
}

func TestPublicStreamSendsHeartbeats(t *testing.T) {
	h := newStreamHarness(t, StreamConfig{Heartbeat: 30 * time.Millisecond, MaxAge: 200 * time.Millisecond}, nil, true)

	body := h.run(t, nil)
	require.GreaterOrEqual(t, strings.Count(body, ":ping\n\n"), 2, "idle connections must be kept alive through proxies")
}

func TestPublicStreamClosesWhenGalleryIsSwitchedOff(t *testing.T) {
	// MaxAge is long: the stream must end because the gallery closed, not
	// because it timed out.
	h := newStreamHarness(t, StreamConfig{MaxAge: 10 * time.Second}, nil, true)

	body := h.run(t, func() {
		msg := publicMessage(t, h.evt.ID, realtime.KindEventUpdated, map[string]any{"gallery_enabled": false})
		require.NoError(t, h.hub.Publish(context.Background(), msg))
	})
	require.Contains(t, body, "event: event.updated")
}

// stalledWriter blocks on its first write until released. A ResponseRecorder
// never blocks, so without this a "slow client" would always keep up and the
// overflow path could not be reached.
type stalledWriter struct {
	*httptest.ResponseRecorder
	reached  chan struct{}
	release  chan struct{}
	onceHit  sync.Once
	released bool
}

func newStalledWriter() *stalledWriter {
	return &stalledWriter{
		ResponseRecorder: httptest.NewRecorder(),
		reached:          make(chan struct{}),
		release:          make(chan struct{}),
	}
}

func (w *stalledWriter) Write(p []byte) (int, error) {
	if !w.released {
		w.onceHit.Do(func() { close(w.reached) })
		<-w.release
		w.released = true
	}
	return w.ResponseRecorder.Write(p)
}

func TestPublicStreamAsksASlowClientToResync(t *testing.T) {
	h := newStreamHarness(t, StreamConfig{MaxAge: 10 * time.Second}, nil, true)

	writer := newStalledWriter()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/public/events/"+h.evt.Slug+"/stream", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.router.ServeHTTP(writer, req)
	}()

	// The connection is subscribed but wedged on its first write, so every
	// message published now piles up behind it.
	<-writer.reached
	require.Eventually(t, func() bool { return h.bus.subscriberCount(h.evt.ID) > 0 }, 2*time.Second, 5*time.Millisecond)
	for i := 0; i < realtime.DefaultBuffer*3; i++ {
		msg := publicMessage(t, h.evt.ID, realtime.KindMediaCreated, map[string]int{"seq": i})
		require.NoError(t, h.hub.Publish(context.Background(), msg))
	}
	require.Eventually(t, func() bool { return h.hub.Subscribers(h.evt.ID) == 0 }, 2*time.Second, 10*time.Millisecond,
		"the hub must cut a connection loose rather than let it block the event")

	close(writer.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a dropped stream never closed")
	}
	require.Contains(t, writer.Body.String(), "event: resync", "a client that lost messages must be told its view is stale")
}

func TestPublicStreamRefusesWhenGalleryIsDisabled(t *testing.T) {
	h := newStreamHarness(t, StreamConfig{MaxAge: time.Second}, nil, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/public/events/"+h.evt.Slug+"/stream", nil)
	res := httptest.NewRecorder()
	h.router.ServeHTTP(res, req)

	require.Equal(t, http.StatusForbidden, res.Code)
	require.Contains(t, res.Body.String(), "gallery_disabled")
}

func TestPublicStreamRefusesUnknownEvent(t *testing.T) {
	h := newStreamHarness(t, StreamConfig{MaxAge: time.Second}, nil, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/public/events/evt_missing/stream", nil)
	res := httptest.NewRecorder()
	h.router.ServeHTTP(res, req)

	require.Equal(t, http.StatusNotFound, res.Code)
}

func TestPublicStreamRefusesAReconnectStorm(t *testing.T) {
	h := newStreamHarness(t, StreamConfig{MaxAge: time.Second}, denyingLimiter{}, true)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/public/events/"+h.evt.Slug+"/stream", nil)
	res := httptest.NewRecorder()
	h.router.ServeHTTP(res, req)

	require.Equal(t, http.StatusTooManyRequests, res.Code)
}

func TestPublicStreamRefusesBeyondCapacity(t *testing.T) {
	h := newStreamHarness(t, StreamConfig{MaxAge: 2 * time.Second, MaxPerEvent: 1}, nil, true)

	ctx, cancel := context.WithCancel(context.Background())
	held := make(chan struct{})
	go func() {
		defer close(held)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/public/events/"+h.evt.Slug+"/stream", nil).WithContext(ctx)
		h.router.ServeHTTP(httptest.NewRecorder(), req)
	}()
	require.Eventually(t, func() bool { return h.hub.Subscribers(h.evt.ID) == 1 }, 2*time.Second, 5*time.Millisecond)

	res := httptest.NewRecorder()
	h.router.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/v1/public/events/"+h.evt.Slug+"/stream", nil))
	require.Equal(t, http.StatusServiceUnavailable, res.Code)
	require.Contains(t, res.Body.String(), "stream_capacity")

	cancel()
	<-held
}

func TestStreamRoutesAreAbsentWhenRealtimeIsDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := NewRouter(RouterConfig{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		DatabaseReady: func(context.Context) error { return nil },
	})

	res := httptest.NewRecorder()
	router.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/v1/public/events/evt_test/stream", nil))
	require.Equal(t, http.StatusNotFound, res.Code, "a deployment with realtime off must not expose the route")
}
