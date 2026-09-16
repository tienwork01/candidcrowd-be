package redis

import (
	"context"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

var rateLimitScript = goredis.NewScript(`
local current = redis.call('INCR', KEYS[1])
if current == 1 then
    redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return current
`)

type Limiter struct {
	client *goredis.Client
}

func Open(raw string) (*Limiter, error) {
	opts, err := goredis.ParseURL(raw)
	if err != nil {
		return nil, err
	}
	return &Limiter{client: goredis.NewClient(opts)}, nil
}

func (l Limiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	res, err := rateLimitScript.Run(ctx, l.client, []string{key}, window.Milliseconds()).Int64()
	if err != nil {
		return false, err
	}
	return res <= int64(limit), nil
}

func (l Limiter) Ping(ctx context.Context) error {
	return l.client.Ping(ctx).Err()
}

func (l Limiter) Close() error {
	return l.client.Close()
}
