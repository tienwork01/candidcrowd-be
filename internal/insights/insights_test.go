package insights

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// countingRepository records how often the expensive aggregates were run.
type countingRepository struct {
	Repository
	calls  int
	result Analytics
	err    error
}

func (r *countingRepository) Analytics(_ context.Context, _ uuid.UUID, expected int) (Analytics, error) {
	r.calls++
	if r.err != nil {
		return Analytics{}, r.err
	}
	out := r.result
	out.ExpectedGuestCount = expected
	return out, nil
}

type memoryCache struct {
	values map[string][]byte
	getErr error
	setErr error
	sets   int
	gets   int
}

func newMemoryCache() *memoryCache { return &memoryCache{values: map[string][]byte{}} }

func (c *memoryCache) GetJSON(_ context.Context, key string, dest any) (bool, error) {
	c.gets++
	if c.getErr != nil {
		return false, c.getErr
	}
	raw, ok := c.values[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, dest)
}

func (c *memoryCache) SetJSON(_ context.Context, key string, value any, _ time.Duration) error {
	c.sets++
	if c.setErr != nil {
		return c.setErr
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.values[key] = raw
	return nil
}

func TestAnalyticsServesRepeatedReadsFromCache(t *testing.T) {
	repo := &countingRepository{result: Analytics{Scans: 40, Contributors: 12, Media: 300}}
	cache := newMemoryCache()
	service := NewService(repo, WithCache(cache, time.Minute))
	eventID := uuid.New()

	first, err := service.Analytics(context.Background(), eventID, 100)
	require.NoError(t, err)
	require.EqualValues(t, 40, first.Scans)
	require.Equal(t, 1, repo.calls)
	require.Equal(t, 1, cache.sets)

	second, err := service.Analytics(context.Background(), eventID, 100)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, 1, repo.calls, "a cache hit must not rerun the aggregates")
}

// The expected guest count comes from the event row the caller already loaded,
// so editing it must take effect immediately rather than waiting out the TTL.
func TestAnalyticsTakesExpectedGuestCountFromTheCaller(t *testing.T) {
	repo := &countingRepository{result: Analytics{Scans: 40}}
	service := NewService(repo, WithCache(newMemoryCache(), time.Minute))
	eventID := uuid.New()

	_, err := service.Analytics(context.Background(), eventID, 100)
	require.NoError(t, err)

	updated, err := service.Analytics(context.Background(), eventID, 250)
	require.NoError(t, err)
	require.Equal(t, 250, updated.ExpectedGuestCount, "a cache hit must not serve a stale guest count")
	require.EqualValues(t, 40, updated.Scans)
	require.Equal(t, 1, repo.calls)
}

// Redis being unreachable must cost a cache hit, never the request.
func TestAnalyticsSurvivesAnUnreachableCache(t *testing.T) {
	repo := &countingRepository{result: Analytics{Scans: 7}}
	cache := newMemoryCache()
	cache.getErr = errors.New("redis unavailable")
	cache.setErr = errors.New("redis unavailable")
	service := NewService(repo, WithCache(cache, time.Minute))

	result, err := service.Analytics(context.Background(), uuid.New(), 10)
	require.NoError(t, err)
	require.EqualValues(t, 7, result.Scans)
	require.Equal(t, 1, repo.calls)
}

func TestAnalyticsWorksWithoutACache(t *testing.T) {
	repo := &countingRepository{result: Analytics{Scans: 3}}
	service := NewService(repo)

	for i := 0; i < 3; i++ {
		result, err := service.Analytics(context.Background(), uuid.New(), 10)
		require.NoError(t, err)
		require.EqualValues(t, 3, result.Scans)
	}
	require.Equal(t, 3, repo.calls)
}
