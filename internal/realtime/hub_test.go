package realtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// fakeBus is an in-process stand-in for Redis. It records how many
// subscriptions are open so tests can assert that an idle event releases its
// bus connection.
type fakeBus struct {
	mu          sync.Mutex
	listeners   map[uuid.UUID][]chan []byte
	activeSubs  int
	totalSubs   int
	subscribeFn func(uuid.UUID) error
}

func newFakeBus() *fakeBus {
	return &fakeBus{listeners: make(map[uuid.UUID][]chan []byte)}
}

func (b *fakeBus) Publish(_ context.Context, eventID uuid.UUID, payload []byte) error {
	b.mu.Lock()
	targets := append([]chan []byte(nil), b.listeners[eventID]...)
	b.mu.Unlock()
	for _, target := range targets {
		target <- payload
	}
	return nil
}

func (b *fakeBus) Subscribe(ctx context.Context, eventID uuid.UUID) (<-chan []byte, func(), error) {
	if b.subscribeFn != nil {
		if err := b.subscribeFn(eventID); err != nil {
			return nil, nil, err
		}
	}
	ch := make(chan []byte, 16)

	b.mu.Lock()
	b.listeners[eventID] = append(b.listeners[eventID], ch)
	b.activeSubs++
	b.totalSubs++
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
			b.activeSubs--
			b.mu.Unlock()
		})
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ch, stop, nil
}

func (b *fakeBus) active() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.activeSubs
}

func (b *fakeBus) total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.totalSubs
}

func quietHub(bus Bus, buffer int) *Hub {
	return NewHub(bus, slog.New(slog.NewTextHandler(io.Discard, nil)), buffer)
}

// waitForBus blocks until the hub has established its bus subscription.
// Subscribe starts that off the hot path, so publishing before it is live is
// a legitimate (and deliberately lossy) race.
func waitForBus(t *testing.T, bus *fakeBus, want int) {
	t.Helper()
	require.Eventually(t, func() bool { return bus.active() == want }, time.Second, 5*time.Millisecond)
}

func mustPublish(t *testing.T, hub *Hub, eventID uuid.UUID, audience Audience, payload any) Message {
	t.Helper()
	msg, err := NewMessage(eventID, KindMediaCreated, audience, payload)
	require.NoError(t, err)
	require.NoError(t, hub.Publish(context.Background(), msg))
	return msg
}

func receive(t *testing.T, sub *Subscription) Message {
	t.Helper()
	select {
	case msg, ok := <-sub.Messages():
		require.True(t, ok, "subscription closed unexpectedly")
		return msg
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
		return Message{}
	}
}

func requireNoMessage(t *testing.T, sub *Subscription) {
	t.Helper()
	select {
	case msg, ok := <-sub.Messages():
		t.Fatalf("unexpected delivery (open=%v): %+v", ok, msg)
	case <-time.After(75 * time.Millisecond):
	}
}

func TestHubDeliversToEverySubscriberOfAnEvent(t *testing.T) {
	bus := newFakeBus()
	hub := quietHub(bus, DefaultBuffer)
	defer hub.Close()

	eventID := uuid.New()
	first, err := hub.Subscribe(eventID, AudienceHost)
	require.NoError(t, err)
	second, err := hub.Subscribe(eventID, AudienceHost)
	require.NoError(t, err)
	waitForBus(t, bus, 1)

	// One bus subscription serves every connection watching the event.
	require.Equal(t, 1, bus.total())
	require.Equal(t, 2, hub.Subscribers(eventID))

	sent := mustPublish(t, hub, eventID, AudienceHost, map[string]string{"id": "m1"})
	require.Equal(t, sent.ID, receive(t, first).ID)
	require.Equal(t, sent.ID, receive(t, second).ID)
}

func TestHubIsolatesEvents(t *testing.T) {
	bus := newFakeBus()
	hub := quietHub(bus, DefaultBuffer)
	defer hub.Close()

	watched, other := uuid.New(), uuid.New()
	sub, err := hub.Subscribe(watched, AudienceHost)
	require.NoError(t, err)
	waitForBus(t, bus, 1)

	mustPublish(t, hub, other, AudienceHost, map[string]string{"id": "m1"})
	requireNoMessage(t, sub)
}

func TestHubFiltersByAudience(t *testing.T) {
	bus := newFakeBus()
	hub := quietHub(bus, DefaultBuffer)
	defer hub.Close()

	eventID := uuid.New()
	host, err := hub.Subscribe(eventID, AudienceHost)
	require.NoError(t, err)
	guest, err := hub.Subscribe(eventID, AudiencePublic)
	require.NoError(t, err)
	waitForBus(t, bus, 1)

	// A hidden item is host-only: a guest stream must never observe it.
	hostOnly := mustPublish(t, hub, eventID, AudienceHost, map[string]string{"status": "hidden"})
	require.Equal(t, hostOnly.ID, receive(t, host).ID)
	requireNoMessage(t, guest)

	shared := mustPublish(t, hub, eventID, AudienceHost|AudiencePublic, map[string]string{"status": "ready"})
	require.Equal(t, shared.ID, receive(t, host).ID)
	require.Equal(t, shared.ID, receive(t, guest).ID)
}

func TestHubDropsSubscriberThatCannotKeepUp(t *testing.T) {
	bus := newFakeBus()
	hub := quietHub(bus, 2)
	defer hub.Close()

	eventID := uuid.New()
	slow, err := hub.Subscribe(eventID, AudienceHost)
	require.NoError(t, err)
	waitForBus(t, bus, 1)

	for i := 0; i < 20; i++ {
		mustPublish(t, hub, eventID, AudienceHost, map[string]int{"seq": i})
	}

	// The channel closes once the connection is cut loose, so draining it
	// terminates instead of blocking the test.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range slow.Messages() {
		}
	}()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("a stalled connection was never cut loose")
	}
	require.True(t, slow.Dropped(), "a dropped connection must report it so the transport can force a resync")
}

func TestHubKeepsHealthySubscriberWhenAnotherIsDropped(t *testing.T) {
	bus := newFakeBus()
	hub := quietHub(bus, 2)
	defer hub.Close()

	eventID := uuid.New()
	stalled, err := hub.Subscribe(eventID, AudienceHost)
	require.NoError(t, err)
	healthy, err := hub.Subscribe(eventID, AudienceHost)
	require.NoError(t, err)
	waitForBus(t, bus, 1)

	// The healthy connection reads each message before the next is published,
	// which is what a keeping-up client does. The stalled one never reads, so
	// only it may overflow.
	for i := 0; i < 6; i++ {
		sent := mustPublish(t, hub, eventID, AudienceHost, map[string]int{"seq": i})
		require.Equal(t, sent.ID, receive(t, healthy).ID)
	}

	require.Eventually(t, func() bool { return stalled.Dropped() }, time.Second, 10*time.Millisecond)
	require.False(t, healthy.Dropped(), "one stalled phone must not disturb every other viewer")
	require.Equal(t, 1, hub.Subscribers(eventID), "only the stalled connection should have been cut loose")
}

func TestLastSubscriberLeavingReleasesTheBus(t *testing.T) {
	bus := newFakeBus()
	hub := quietHub(bus, DefaultBuffer)
	defer hub.Close()

	eventID := uuid.New()
	first, err := hub.Subscribe(eventID, AudienceHost)
	require.NoError(t, err)
	second, err := hub.Subscribe(eventID, AudiencePublic)
	require.NoError(t, err)
	waitForBus(t, bus, 1)

	first.Close()
	require.Equal(t, 1, bus.active(), "the bus subscription must survive while a viewer remains")
	require.Equal(t, 1, hub.Subscribers(eventID))

	second.Close()
	waitForBus(t, bus, 0)
	require.Equal(t, 0, hub.Subscribers(eventID))

	// A later viewer of the same event gets a fresh subscription.
	third, err := hub.Subscribe(eventID, AudienceHost)
	require.NoError(t, err)
	waitForBus(t, bus, 1)
	require.Equal(t, 2, bus.total())
	third.Close()
}

func TestSubscriptionCloseIsIdempotent(t *testing.T) {
	bus := newFakeBus()
	hub := quietHub(bus, DefaultBuffer)
	defer hub.Close()

	sub, err := hub.Subscribe(uuid.New(), AudienceHost)
	require.NoError(t, err)
	waitForBus(t, bus, 1)

	require.NotPanics(t, func() {
		sub.Close()
		sub.Close()
		sub.Close()
	})
	_, open := <-sub.Messages()
	require.False(t, open)
}

func TestHubCloseReleasesEveryConnection(t *testing.T) {
	bus := newFakeBus()
	hub := quietHub(bus, DefaultBuffer)

	first, err := hub.Subscribe(uuid.New(), AudienceHost)
	require.NoError(t, err)
	second, err := hub.Subscribe(uuid.New(), AudiencePublic)
	require.NoError(t, err)
	waitForBus(t, bus, 2)

	hub.Close()

	for _, sub := range []*Subscription{first, second} {
		select {
		case _, open := <-sub.Messages():
			require.False(t, open, "hub shutdown must close every stream")
		case <-time.After(time.Second):
			t.Fatal("hub shutdown left a stream open, which would stall graceful shutdown")
		}
	}
	waitForBus(t, bus, 0)

	_, err = hub.Subscribe(uuid.New(), AudienceHost)
	require.ErrorIs(t, err, ErrHubClosed)
	require.NotPanics(t, hub.Close)
}

func TestBusSubscribeFailureClosesTheStream(t *testing.T) {
	bus := newFakeBus()
	bus.subscribeFn = func(uuid.UUID) error { return errors.New("redis is down") }
	hub := quietHub(bus, DefaultBuffer)
	defer hub.Close()

	sub, err := hub.Subscribe(uuid.New(), AudienceHost)
	require.NoError(t, err)

	// The connection must not sit on a silent stream believing it is live;
	// closing it lets the client fall back to polling.
	select {
	case _, open := <-sub.Messages():
		require.False(t, open)
	case <-time.After(time.Second):
		t.Fatal("a failed bus subscription left the stream open")
	}
	require.False(t, sub.Dropped(), "a bus failure is not the client falling behind")
}

func TestHubRejectsInvalidSubscriptions(t *testing.T) {
	hub := quietHub(newFakeBus(), DefaultBuffer)
	defer hub.Close()

	_, err := hub.Subscribe(uuid.Nil, AudienceHost)
	require.Error(t, err)

	_, err = hub.Subscribe(uuid.New(), 0)
	require.Error(t, err)
}

func TestHubPublishRejectsInvalidMessage(t *testing.T) {
	hub := quietHub(newFakeBus(), DefaultBuffer)
	defer hub.Close()

	require.Error(t, hub.Publish(context.Background(), Message{Kind: KindMediaCreated}))
}

func TestHubHandlesConcurrentSubscribeAndPublish(t *testing.T) {
	bus := newFakeBus()
	hub := quietHub(bus, DefaultBuffer)
	defer hub.Close()

	eventID := uuid.New()
	var wg sync.WaitGroup

	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub, err := hub.Subscribe(eventID, AudienceHost)
			if err != nil {
				return
			}
			drained := make(chan struct{})
			go func() {
				defer close(drained)
				for range sub.Messages() {
				}
			}()
			time.Sleep(time.Millisecond)
			sub.Close()
			<-drained
		}()
	}
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			msg, err := NewMessage(eventID, KindMediaCreated, AudienceHost, map[string]int{"seq": seq})
			if err != nil {
				return
			}
			_ = hub.Publish(context.Background(), msg)
		}(i)
	}

	wg.Wait()
	require.Eventually(t, func() bool { return hub.Subscribers(eventID) == 0 }, time.Second, 10*time.Millisecond)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
