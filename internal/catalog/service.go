package catalog

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
)

type Service struct {
	repo Repository
	now  func() time.Time
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo, now: time.Now}
}

type PublicPrice struct {
	Currency       string `json:"currency"`
	AmountMinor    int64  `json:"amount_minor"`
	CompareAtMinor *int64 `json:"compare_at_minor"`
}

// PublicLimits is the part of a plan's limits that is disclosed to buyers.
// Item limits of fair-use plans are abuse thresholds and stay internal.
type PublicLimits struct {
	MaxPhotoItems *int64 `json:"max_photo_items,omitempty"`
	MaxVideoItems *int64 `json:"max_video_items,omitempty"`
	MaxMediaBytes int64  `json:"max_media_bytes"`
	RetentionDays int    `json:"retention_days"`
}

type PublicPlan struct {
	Code     PlanCode     `json:"code"`
	Name     string       `json:"name"`
	Version  int          `json:"version"`
	Price    *PublicPrice `json:"price"`
	Features []Feature    `json:"features"`
	FairUse  bool         `json:"fair_use"`
	Limits   PublicLimits `json:"limits"`
}

// List returns the active plans in upgrade order with their current price in
// currency. A plan without a price in that currency is listed with a nil price
// rather than dropped, so the catalog's shape does not depend on currency.
func (s *Service) List(ctx context.Context, currency string) ([]PublicPlan, error) {
	versions, err := s.ActiveVersions(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(versions))
	for i, v := range versions {
		ids[i] = v.ID
	}
	prices, err := s.repo.CurrentPrices(ctx, ids, currency, s.now())
	if err != nil {
		return nil, err
	}
	byVersion := make(map[uuid.UUID]Price, len(prices))
	for _, p := range prices {
		byVersion[p.PlanVersionID] = p
	}

	plans := make([]PublicPlan, 0, len(versions))
	for _, v := range versions {
		ent := v.Entitlements
		plan := PublicPlan{
			Code:     v.Code,
			Name:     v.DisplayName,
			Version:  v.Version,
			Features: append([]Feature{}, ent.Features...),
			FairUse:  ent.FairUse,
			Limits: PublicLimits{
				MaxMediaBytes: ent.Limits.MaxMediaBytes,
				RetentionDays: ent.Limits.RetentionDays,
			},
		}
		if !ent.FairUse {
			photos, videos := ent.Limits.MaxPhotoItems, ent.Limits.MaxVideoItems
			plan.Limits.MaxPhotoItems = &photos
			plan.Limits.MaxVideoItems = &videos
		}
		if p, ok := byVersion[v.ID]; ok {
			plan.Price = &PublicPrice{Currency: p.Currency, AmountMinor: p.AmountMinor, CompareAtMinor: p.CompareAtMinor}
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// ActiveVersions returns every active plan version in upgrade order.
func (s *Service) ActiveVersions(ctx context.Context) ([]Version, error) {
	versions, err := s.repo.ActiveVersions(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Code.Rank() < versions[j].Code.Rank() })
	return versions, nil
}

// ActiveVersion resolves the version new grants of code are issued from.
func (s *Service) ActiveVersion(ctx context.Context, code PlanCode) (Version, error) {
	if !code.Valid() {
		return Version{}, ErrNotFound
	}
	return s.repo.ActiveVersion(ctx, code)
}

// LowestPlanWith names the cheapest active plan that includes f, so a denial
// can tell the host which plan unlocks it. It returns "" when none does.
func (s *Service) LowestPlanWith(ctx context.Context, f Feature) (PlanCode, error) {
	versions, err := s.ActiveVersions(ctx)
	if err != nil {
		return "", err
	}
	for _, v := range versions {
		if v.Entitlements.Has(f) {
			return v.Code, nil
		}
	}
	return "", nil
}
