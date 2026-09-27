package redis

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

// busChannelPrefix keeps realtime traffic in its own keyspace so it never
// collides with rate-limit keys.
const busChannelPrefix = "realtime:event:"

// busStreamBuffer absorbs a short upload burst between Redis and the hub.
const busStreamBuffer = 64

// Bus publishes event-scoped realtime messages across API instances.
//
// It holds a client of its own rather than sharing the Limiter's. A pub/sub
// subscription occupies a connection for as long as the event has viewers, so
// a busy event must not be able to starve the rate limiter's pool.
type Bus struct {
	client *goredis.Client
}

func OpenBus(raw string) (*Bus, error) {
	opts, err := goredis.ParseURL(raw)
	if err != nil {
		return nil, fmt.Errorf("redis: parse bus url: %w", err)
	}
	return &Bus{client: goredis.NewClient(opts)}, nil
}

func channelFor(eventID uuid.UUID) string { return busChannelPrefix + eventID.String() }

func (b *Bus) Publish(ctx context.Context, eventID uuid.UUID, payload []byte) error {
	return b.client.Publish(ctx, channelFor(eventID), payload).Err()
}

// Subscribe streams one event's payloads. The returned channel closes when the
// context is cancelled or stop runs, so the caller never has to distinguish
// between a local teardown and a dropped Redis connection.
func (b *Bus) Subscribe(ctx context.Context, eventID uuid.UUID) (<-chan []byte, func(), error) {
	sub := b.client.Subscribe(ctx, channelFor(eventID))
	// Receive confirms the subscription is established, turning a dead Redis
	// into an error the caller can act on instead of a stream that is silently
	// never delivered.
	if _, err := sub.Receive(ctx); err != nil {
		_ = sub.Close()
		return nil, nil, fmt.Errorf("redis: subscribe %s: %w", eventID, err)
	}

	out := make(chan []byte, busStreamBuffer)
	go func() {
		defer close(out)
		incoming := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-incoming:
				if !ok {
					return
				}
				select {
				case out <- []byte(msg.Payload):
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, func() { _ = sub.Close() }, nil
}

func (b *Bus) Ping(ctx context.Context) error { return b.client.Ping(ctx).Err() }

func (b *Bus) Close() error { return b.client.Close() }
