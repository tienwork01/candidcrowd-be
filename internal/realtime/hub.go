package realtime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
)

// Bus fans a message out to every API instance. The hub owns delivery inside
// one process; the bus owns the hop between processes.
type Bus interface {
	Publish(ctx context.Context, eventID uuid.UUID, payload []byte) error
	// Subscribe streams payloads for one event until the context is cancelled
	// or the returned stop function runs. The channel closes when either happens.
	Subscribe(ctx context.Context, eventID uuid.UUID) (<-chan []byte, func(), error)
}

// DefaultBuffer is how many messages a single connection may fall behind
// before it is cut loose. An upload burst must not let one stalled phone hold
// memory for the whole event.
const DefaultBuffer = 32

var ErrHubClosed = errors.New("realtime: hub is closed")

// Hub multiplexes one bus subscription per event across every connection this
// instance holds for that event.
type Hub struct {
	bus    Bus
	log    *slog.Logger
	buffer int

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	topics map[uuid.UUID]*topic
	closed bool
}

func NewHub(bus Bus, log *slog.Logger, buffer int) *Hub {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Hub{
		bus:    bus,
		log:    log,
		buffer: buffer,
		ctx:    ctx,
		cancel: cancel,
		topics: make(map[uuid.UUID]*topic),
	}
}

// Publish always travels through the bus, even when this instance holds
// subscribers for the event. One delivery path means a message can never be
// counted twice, and local and remote clients observe the same ordering.
func (h *Hub) Publish(ctx context.Context, msg Message) error {
	raw, err := Encode(msg)
	if err != nil {
		return err
	}
	return h.bus.Publish(ctx, msg.EventID, raw)
}

// Subscribe registers one connection. The caller must Close the subscription.
func (h *Hub) Subscribe(eventID uuid.UUID, audience Audience) (*Subscription, error) {
	if eventID == uuid.Nil {
		return nil, errors.New("realtime: event id is required")
	}
	if !audience.Valid() {
		return nil, errors.New("realtime: invalid audience")
	}

	sub := &Subscription{
		ch:       make(chan Message, h.buffer),
		audience: audience,
		eventID:  eventID,
		hub:      h,
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrHubClosed
	}
	t, existing := h.topics[eventID]
	if !existing {
		t = newTopic(h, eventID)
		h.topics[eventID] = t
	}
	h.mu.Unlock()

	t.add(sub)
	if !existing {
		// The bus subscription is established off the hot path so a reconnect
		// storm never serializes behind one network round trip. Messages
		// published in this window are missed by design: a client refetches on
		// connect, so a gap before the stream is live costs nothing.
		go t.run()
	}
	return sub, nil
}

// Subscribers reports how many connections this instance holds for an event.
// Connection caps and tests read it.
func (h *Hub) Subscribers(eventID uuid.UUID) int {
	h.mu.Lock()
	t, ok := h.topics[eventID]
	h.mu.Unlock()
	if !ok {
		return 0
	}
	return t.size()
}

// Close releases every connection. Graceful shutdown must call it before
// http.Server.Shutdown, which would otherwise wait on open streams.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	pending := make([]*topic, 0, len(h.topics))
	for _, t := range h.topics {
		pending = append(pending, t)
	}
	h.topics = make(map[uuid.UUID]*topic)
	h.mu.Unlock()

	h.cancel()
	for _, t := range pending {
		t.shutdown()
	}
}

// discard removes a topic only when the map still points at this instance. A
// replacement topic may already have been created for the same event.
func (h *Hub) discard(t *topic) {
	h.mu.Lock()
	if current, ok := h.topics[t.eventID]; ok && current == t {
		delete(h.topics, t.eventID)
	}
	h.mu.Unlock()
}

type topic struct {
	hub     *Hub
	eventID uuid.UUID
	ctx     context.Context
	cancel  context.CancelFunc

	mu          sync.Mutex
	subscribers map[*Subscription]struct{}
}

func newTopic(h *Hub, eventID uuid.UUID) *topic {
	ctx, cancel := context.WithCancel(h.ctx)
	return &topic{
		hub:         h,
		eventID:     eventID,
		ctx:         ctx,
		cancel:      cancel,
		subscribers: make(map[*Subscription]struct{}),
	}
}

func (t *topic) add(sub *Subscription) {
	t.mu.Lock()
	t.subscribers[sub] = struct{}{}
	t.mu.Unlock()
}

func (t *topic) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.subscribers)
}

func (t *topic) run() {
	defer t.shutdown()

	incoming, stop, err := t.hub.bus.Subscribe(t.ctx, t.eventID)
	if err != nil {
		// Closing the subscribers tells each connection to stop pretending it
		// is live, so the browser falls back to polling instead of sitting on
		// a silent stream.
		t.hub.log.Error("realtime: bus subscribe failed", "event_id", t.eventID, "error", err)
		return
	}
	defer stop()

	for {
		select {
		case <-t.ctx.Done():
			return
		case raw, ok := <-incoming:
			if !ok {
				return
			}
			msg, decodeErr := Decode(raw)
			if decodeErr != nil {
				t.hub.log.Warn("realtime: dropping malformed message", "event_id", t.eventID, "error", decodeErr)
				continue
			}
			t.deliver(msg)
		}
	}
}

func (t *topic) deliver(msg Message) {
	t.mu.Lock()
	var overflowed []*Subscription
	for sub := range t.subscribers {
		if !msg.Audience.Includes(sub.audience) {
			continue
		}
		select {
		case sub.ch <- msg:
		default:
			// A connection that cannot keep up is dropped rather than allowed
			// to block every other viewer of the event. It reports Dropped, so
			// the transport can ask that client to resynchronize.
			sub.dropped.Store(true)
			overflowed = append(overflowed, sub)
			delete(t.subscribers, sub)
		}
	}
	empty := len(t.subscribers) == 0
	t.mu.Unlock()

	for _, sub := range overflowed {
		sub.release()
	}
	if empty {
		t.cancel()
	}
}

// remove detaches one subscription and tears the topic down with the last one,
// so an idle event holds no Redis subscription.
func (t *topic) remove(sub *Subscription) {
	t.mu.Lock()
	delete(t.subscribers, sub)
	empty := len(t.subscribers) == 0
	t.mu.Unlock()
	if empty {
		t.cancel()
	}
}

func (t *topic) shutdown() {
	t.cancel()
	t.hub.discard(t)

	t.mu.Lock()
	pending := make([]*Subscription, 0, len(t.subscribers))
	for sub := range t.subscribers {
		pending = append(pending, sub)
	}
	t.subscribers = make(map[*Subscription]struct{})
	t.mu.Unlock()

	for _, sub := range pending {
		sub.release()
	}
}

// Subscription is one connection's view of an event.
type Subscription struct {
	ch       chan Message
	audience Audience
	eventID  uuid.UUID
	hub      *Hub
	once     sync.Once
	dropped  atomic.Bool
}

// Messages closes when the subscription ends, whether the caller closed it,
// the hub shut down, or this connection fell too far behind.
func (s *Subscription) Messages() <-chan Message { return s.ch }

// Dropped reports that messages were lost because this connection could not
// keep up. The transport should tell the client to refetch rather than
// pretend its view is current.
func (s *Subscription) Dropped() bool { return s.dropped.Load() }

func (s *Subscription) EventID() uuid.UUID { return s.eventID }

// Close is safe to call more than once.
func (s *Subscription) Close() {
	s.hub.mu.Lock()
	t := s.hub.topics[s.eventID]
	s.hub.mu.Unlock()
	if t != nil {
		t.remove(s)
	}
	s.release()
}

func (s *Subscription) release() {
	s.once.Do(func() { close(s.ch) })
}
