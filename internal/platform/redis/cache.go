package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Cache holds short-lived computed values.
//
// It keeps a client of its own rather than sharing the Limiter's. Rate
// limiting protects uploads and stream connections, and a cache filling up
// during a dashboard refresh must not be able to exhaust the pool that work
// depends on.
type Cache struct {
	client *goredis.Client
}

func OpenCache(raw string) (*Cache, error) {
	opts, err := goredis.ParseURL(raw)
	if err != nil {
		return nil, fmt.Errorf("redis: parse cache url: %w", err)
	}
	return &Cache{client: goredis.NewClient(opts)}, nil
}

// GetJSON reports whether the key held a usable value. A miss, an unreachable
// Redis and a value that no longer matches dest are all reported the same way,
// because every caller responds to them identically: compute it again.
func (c *Cache) GetJSON(ctx context.Context, key string, dest any) (bool, error) {
	raw, err := c.client.Get(ctx, key).Bytes()
	if errors.Is(err, goredis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		// A stale shape left by an older deployment is not an error worth
		// failing a request over; it is simply not usable.
		return false, nil
	}
	return true, nil
}

func (c *Cache) SetJSON(ctx context.Context, key string, value any, ttl time.Duration) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.client.Set(ctx, key, raw, ttl).Err()
}

func (c *Cache) Ping(ctx context.Context) error { return c.client.Ping(ctx).Err() }

func (c *Cache) Close() error { return c.client.Close() }
