package redis

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	goredis "github.com/go-redis/redis/v8"
)

type Limiter struct{ client *goredis.Client }

func Open(raw string) (*Limiter, error) {
	u, e := url.Parse(raw)
	if e != nil {
		return nil, e
	}
	n, err := strconv.Atoi(strings.TrimPrefix(u.Path, "/"))
	if err != nil {
		n = 0
	}
	return &Limiter{client: goredis.NewClient(&goredis.Options{Addr: u.Host, Password: func() string {
		if u.User != nil {
			p, _ := u.User.Password()
			return p
		}
		return ""
	}(), DB: n})}, nil
}
func (l Limiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	n, e := l.client.Incr(ctx, key).Result()
	if e != nil {
		return false, e
	}
	if n == 1 {
		if e = l.client.Expire(ctx, key, window).Err(); e != nil {
			return false, e
		}
	}
	return n <= int64(limit), nil
}
func (l Limiter) Ping(ctx context.Context) error { return l.client.Ping(ctx).Err() }
func (l Limiter) Close() error                   { return l.client.Close() }
