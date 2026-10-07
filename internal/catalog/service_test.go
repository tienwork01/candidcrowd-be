package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type memoryRepository struct {
	versions []Version
	prices   []Price
}

func (r *memoryRepository) ActiveVersions(context.Context) ([]Version, error) {
	var out []Version
	for _, v := range r.versions {
		if v.Status == VersionActive {
			out = append(out, v)
		}
	}
	return out, nil
}

func (r *memoryRepository) ActiveVersion(_ context.Context, code PlanCode) (Version, error) {
	for _, v := range r.versions {
		if v.Code == code && v.Status == VersionActive {
			return v, nil
		}
	}
	return Version{}, ErrNotFound
}

func (r *memoryRepository) CurrentPrices(_ context.Context, ids []uuid.UUID, currency string, at time.Time) ([]Price, error) {
	best := map[uuid.UUID]Price{}
	for _, p := range r.prices {
		if p.Currency != currency || p.ValidFrom.After(at) || (p.ValidUntil != nil && !p.ValidUntil.After(at)) {
			continue
		}
		if cur, ok := best[p.PlanVersionID]; !ok || p.ValidFrom.After(cur.ValidFrom) {
			best[p.PlanVersionID] = p
		}
	}
	var out []Price
	for _, id := range ids {
		if p, ok := best[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func seededRepository() *memoryRepository {
	free := Version{ID: uuid.New(), Code: PlanFree, Version: 1, DisplayName: "Free", Status: VersionActive,
		Entitlements: Entitlements{Limits: Limits{MaxMediaItems: 33, MaxPhotoItems: 30, MaxVideoItems: 3, MaxMediaBytes: 500 << 20, MaxLiveWallItems: 20, RetentionDays: 7}}}
	experience := Version{ID: uuid.New(), Code: PlanExperience, Version: 1, DisplayName: "Experience", Status: VersionActive,
		Entitlements: Entitlements{FairUse: true, Features: []Feature{FeatureZIPExport, FeatureFullLiveWall},
			Limits: Limits{MaxMediaItems: 2000, MaxPhotoItems: 2000, MaxVideoItems: 500, MaxMediaBytes: 50 << 30, RetentionDays: 365}}}
	signature := Version{ID: uuid.New(), Code: PlanSignature, Version: 1, DisplayName: "Signature", Status: VersionActive,
		Entitlements: Entitlements{FairUse: true, Features: []Feature{FeatureZIPExport, FeatureFullLiveWall, FeatureThroughTheMoment},
			Limits: Limits{MaxMediaItems: 5000, MaxMediaBytes: 150 << 30, RetentionDays: 730}}}
	retired := Version{ID: uuid.New(), Code: PlanSignature, Version: 0, Status: VersionRetired}
	past := time.Now().Add(-time.Hour)
	expired := time.Now().Add(-time.Minute)
	compare := int64(4900)
	return &memoryRepository{
		// Deliberately out of order: the service owns the upgrade order.
		versions: []Version{signature, retired, free, experience},
		prices: []Price{
			{PlanVersionID: free.ID, Currency: "USD", AmountMinor: 0, ValidFrom: past},
			{PlanVersionID: experience.ID, Currency: "USD", AmountMinor: 4900, ValidFrom: past.Add(-time.Hour), ValidUntil: &expired},
			{PlanVersionID: experience.ID, Currency: "USD", AmountMinor: 3900, CompareAtMinor: &compare, ValidFrom: past},
			{PlanVersionID: signature.ID, Currency: "EUR", AmountMinor: 6500, ValidFrom: past},
		},
	}
}

func TestListOrdersPlansAndPicksCurrentPrice(t *testing.T) {
	plans, err := NewService(seededRepository()).List(context.Background(), "USD")
	require.NoError(t, err)
	require.Len(t, plans, 3)
	require.Equal(t, []PlanCode{PlanFree, PlanExperience, PlanSignature}, []PlanCode{plans[0].Code, plans[1].Code, plans[2].Code})

	require.Equal(t, int64(3900), plans[1].Price.AmountMinor)
	require.Equal(t, int64(4900), *plans[1].Price.CompareAtMinor)
	require.Nil(t, plans[2].Price, "a plan without a price in the currency is listed without one")
}

func TestListDisclosesItemLimitsOnlyForNonFairUsePlans(t *testing.T) {
	plans, err := NewService(seededRepository()).List(context.Background(), "USD")
	require.NoError(t, err)

	free := plans[0]
	require.Equal(t, int64(30), *free.Limits.MaxPhotoItems)
	require.Equal(t, int64(3), *free.Limits.MaxVideoItems)
	require.Equal(t, 7, free.Limits.RetentionDays)

	experience := plans[1]
	require.True(t, experience.FairUse)
	require.Nil(t, experience.Limits.MaxPhotoItems)
	require.Nil(t, experience.Limits.MaxVideoItems)
	require.Equal(t, int64(50<<30), experience.Limits.MaxMediaBytes)
}

func TestLowestPlanWith(t *testing.T) {
	svc := NewService(seededRepository())
	code, err := svc.LowestPlanWith(context.Background(), FeatureZIPExport)
	require.NoError(t, err)
	require.Equal(t, PlanExperience, code)

	code, err = svc.LowestPlanWith(context.Background(), FeatureThroughTheMoment)
	require.NoError(t, err)
	require.Equal(t, PlanSignature, code)

	code, err = svc.LowestPlanWith(context.Background(), FeatureQRSourceAnalytics)
	require.NoError(t, err)
	require.Empty(t, code)
}

func TestActiveVersionRejectsUnknownCode(t *testing.T) {
	_, err := NewService(seededRepository()).ActiveVersion(context.Background(), "gold")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestHandlerServesRolloutAndValidatesCurrency(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/plans", NewHandler(NewService(seededRepository()), Rollout{PricingUIEnabled: true}).List)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/plans?currency=usd", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Plans   []PublicPlan `json:"plans"`
		Rollout Rollout      `json:"rollout"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Plans, 3)
	require.True(t, body.Rollout.PricingUIEnabled)
	require.False(t, body.Rollout.ManualActivationEnabled)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/plans?currency=dollars", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestEntitlementsRoundTripThroughJSONColumn(t *testing.T) {
	in := Entitlements{FairUse: true, Features: []Feature{FeatureZIPExport}, Limits: Limits{MaxMediaBytes: 42, RetentionDays: 7}}
	v, err := in.Value()
	require.NoError(t, err)
	var out Entitlements
	require.NoError(t, out.Scan([]byte(v.(string))))
	require.Equal(t, in, out)

	empty, err := Entitlements{}.Value()
	require.NoError(t, err)
	require.Contains(t, empty, `"features":[]`, "an empty feature list is stored as [] not null")
}
