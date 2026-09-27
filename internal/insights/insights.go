package insights

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrNotFound = errors.New("insights: not found")

type QRSource struct {
	ID        uuid.UUID `json:"id"`
	EventID   uuid.UUID `json:"event_id"`
	Code      string    `json:"code"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type SourceMetric struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	Scans        int64  `json:"scans"`
	Contributors int64  `json:"contributors"`
	Uploads      int64  `json:"uploads"`
}

type Analytics struct {
	ExpectedGuestCount int            `json:"expected_guest_count"`
	Scans              int64          `json:"scans"`
	Contributors       int64          `json:"contributors"`
	Media              int64          `json:"media"`
	Photos             int64          `json:"photos"`
	Videos             int64          `json:"videos"`
	Sources            []SourceMetric `json:"sources"`
}

type Repository interface {
	CreateSource(context.Context, QRSource) (QRSource, error)
	ListSources(context.Context, uuid.UUID) ([]QRSource, error)
	UpdateSource(context.Context, uuid.UUID, uuid.UUID, string) (QRSource, error)
	DeleteSource(context.Context, uuid.UUID, uuid.UUID) error
	SourceExists(context.Context, uuid.UUID, string) (bool, error)
	Analytics(context.Context, uuid.UUID, int) (Analytics, error)
}

// Cache stores computed analytics briefly. It is optional: with no cache the
// service simply recomputes, and a cache that errors is treated as a miss.
type Cache interface {
	GetJSON(ctx context.Context, key string, dest any) (bool, error)
	SetJSON(ctx context.Context, key string, value any, ttl time.Duration) error
}

type Service struct {
	repo     Repository
	cache    Cache
	cacheTTL time.Duration
}

func NewService(repo Repository, options ...Option) *Service {
	s := &Service{repo: repo, cacheTTL: 30 * time.Second}
	for _, option := range options {
		option(s)
	}
	return s
}

type Option func(*Service)

// WithCache serves repeated dashboard reads from Redis. Analytics runs three
// aggregates over an event's whole history, and a host dashboard refreshes far
// more often than the numbers meaningfully change.
func WithCache(cache Cache, ttl time.Duration) Option {
	return func(s *Service) {
		s.cache = cache
		if ttl > 0 {
			s.cacheTTL = ttl
		}
	}
}

var sourceCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)

func normalizeCode(code string) (string, error) {
	code = strings.ToLower(strings.TrimSpace(code))
	if !sourceCodePattern.MatchString(code) {
		return "", fmt.Errorf("source code must be 1-48 lowercase letters, numbers, hyphens, or underscores")
	}
	return code, nil
}

func (s *Service) CreateSource(ctx context.Context, eventID uuid.UUID, code, name string) (QRSource, error) {
	code, err := normalizeCode(code)
	if err != nil {
		return QRSource{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return QRSource{}, fmt.Errorf("source name must be 1-80 characters")
	}
	return s.repo.CreateSource(ctx, QRSource{ID: uuid.New(), EventID: eventID, Code: code, Name: name})
}

func (s *Service) ListSources(ctx context.Context, eventID uuid.UUID) ([]QRSource, error) {
	return s.repo.ListSources(ctx, eventID)
}

func (s *Service) UpdateSource(ctx context.Context, eventID, sourceID uuid.UUID, name string) (QRSource, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return QRSource{}, fmt.Errorf("source name must be 1-80 characters")
	}
	return s.repo.UpdateSource(ctx, eventID, sourceID, name)
}

func (s *Service) DeleteSource(ctx context.Context, eventID, sourceID uuid.UUID) error {
	return s.repo.DeleteSource(ctx, eventID, sourceID)
}

func (s *Service) ValidateSource(ctx context.Context, eventID uuid.UUID, code string) (*string, error) {
	if strings.TrimSpace(code) == "" {
		return nil, nil
	}
	code, err := normalizeCode(code)
	if err != nil {
		return nil, err
	}
	exists, err := s.repo.SourceExists(ctx, eventID, code)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	return &code, nil
}

// Analytics is eventually consistent by design: a cached answer may lag the
// database by up to the TTL. Participation numbers are a dashboard reading,
// not a value anything transacts on, and the cost of computing them exactly is
// three full-event aggregates per refresh.
func (s *Service) Analytics(ctx context.Context, eventID uuid.UUID, expectedGuestCount int) (Analytics, error) {
	key := "analytics:event:" + eventID.String()
	if s.cache != nil {
		var cached Analytics
		// A cache failure must never fail the request; it only means the
		// numbers are computed this time.
		if hit, err := s.cache.GetJSON(ctx, key, &cached); err == nil && hit {
			// The expected guest count comes from the event row the caller
			// already loaded, so it is always current even on a cache hit.
			cached.ExpectedGuestCount = expectedGuestCount
			return cached, nil
		}
	}
	result, err := s.repo.Analytics(ctx, eventID, expectedGuestCount)
	if err != nil {
		return Analytics{}, err
	}
	if s.cache != nil {
		_ = s.cache.SetJSON(ctx, key, result, s.cacheTTL)
	}
	return result, nil
}
