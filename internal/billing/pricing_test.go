package billing_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/billing"
	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Catalog list prices, which are also the first-time commercial values.
const (
	experienceList = int64(3900)
	signatureList  = int64(6900)
)

// Loyalty price list, earned by an eligible paid purchase on another event.
const (
	experienceReturning = int64(3200)
	signatureReturning  = int64(5900)
)

type pricingFixture struct {
	*testEnv
	eventID  uuid.UUID
	hostID   uuid.UUID
	versions map[catalog.PlanCode]uuid.UUID
}

// newPricingFixture seeds one event owned by one host, sitting on the given
// plan, with the whole sellable catalog priced.
func newPricingFixture(t *testing.T, current catalog.PlanCode) *pricingFixture {
	t.Helper()
	f := &pricingFixture{
		testEnv:  newTestEnv(),
		eventID:  uuid.New(),
		hostID:   uuid.New(),
		versions: map[catalog.PlanCode]uuid.UUID{},
	}
	f.own.owners[fmt.Sprintf("%s:%s", f.eventID, f.hostID)] = true
	f.entSvc.entitlements[f.eventID] = entitlement.EventEntitlement{EventID: f.eventID, PlanCode: current}

	// The catalog publishes both the list price and the loyalty price; the
	// pricing engine holds no price table of its own.
	for plan, prices := range map[catalog.PlanCode][2]int64{
		catalog.PlanExperience: {experienceList, experienceReturning},
		catalog.PlanSignature:  {signatureList, signatureReturning},
	} {
		versionID := uuid.New()
		returning := prices[1]
		f.versions[plan] = versionID
		f.cat.versions[string(plan)] = billing.CatalogVersion{ID: versionID, Code: string(plan)}
		f.cat.prices[fmt.Sprintf("%s:USD", versionID)] = billing.CatalogPrice{
			AmountMinor: prices[0], ReturningAmountMinor: &returning, Currency: "USD",
		}
	}
	return f
}

// settledPurchase records a completed purchase, which is how the pricing engine
// learns the commercial context an event was sold in.
func (f *pricingFixture) settledPurchase(eventID uuid.UUID, plan catalog.PlanCode, base, discount int64, pctx billing.PricingContext, status billing.PurchaseStatus) uuid.UUID {
	id := uuid.New()
	total := base - discount
	f.repo.purchases[id] = billing.Purchase{
		ID: id, EventID: eventID, HostID: f.hostID, PlanCode: plan,
		PlanVersionID: f.versions[plan], BaseAmountMinor: base,
		DiscountAmountMinor: discount, QuotedAmountMinor: total,
		QuotedCurrency: "USD", PricingContext: pctx, Status: status,
		SettledTotalMinor: &total, CreatedAt: time.Now().UTC(),
	}
	return id
}

func (f *pricingFixture) quote(t *testing.T, target catalog.PlanCode) billing.Quote {
	t.Helper()
	q, err := f.svc.QuoteEventPurchase(context.Background(), f.eventID, f.hostID, target, "USD")
	require.NoError(t, err)
	return q
}

func TestQuoteFirstPurchaseUsesCatalogPrices(t *testing.T) {
	for target, want := range map[catalog.PlanCode]int64{
		catalog.PlanExperience: experienceList,
		catalog.PlanSignature:  signatureList,
	} {
		f := newPricingFixture(t, catalog.PlanFree)
		q := f.quote(t, target)
		require.Equal(t, want, q.FinalAmountMinor)
		require.Equal(t, want, q.BaseAmountMinor)
		require.Zero(t, q.UpgradeCreditMinor)
		require.Equal(t, billing.PricingFirstPurchase, q.Reason)
		require.Equal(t, billing.ContextFirst, q.Context)
	}
}

func TestQuoteReturningPricesNeedAPaidPurchaseOnAnotherEvent(t *testing.T) {
	for target, want := range map[catalog.PlanCode]int64{
		catalog.PlanExperience: experienceReturning,
		catalog.PlanSignature:  signatureReturning,
	} {
		f := newPricingFixture(t, catalog.PlanFree)
		f.settledPurchase(uuid.New(), catalog.PlanExperience, experienceList, 0, billing.ContextFirst, billing.StatusSettled)
		q := f.quote(t, target)
		require.Equal(t, want, q.FinalAmountMinor)
		require.Equal(t, billing.PricingReturning, q.Reason)
		require.Equal(t, billing.ContextReturning, q.Context)
	}
}

func TestQuoteReturningIgnoresRefundedAndFreePurchases(t *testing.T) {
	for _, tc := range []struct {
		name   string
		base   int64
		status billing.PurchaseStatus
	}{
		{"refunded", experienceList, billing.StatusRefunded},
		{"disputed", experienceList, billing.StatusDisputed},
		{"zero value", 0, billing.StatusSettled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPricingFixture(t, catalog.PlanFree)
			f.settledPurchase(uuid.New(), catalog.PlanExperience, tc.base, 0, billing.ContextFirst, tc.status)
			q := f.quote(t, catalog.PlanExperience)
			require.Equal(t, experienceList, q.FinalAmountMinor)
			require.Equal(t, billing.PricingFirstPurchase, q.Reason)
		})
	}
}

// A purchase cannot discount itself. The Experience purchase on this very event
// unlocks loyalty pricing for the host's *next* event, and the upgrade of this
// event stays in the context it was sold in.
func TestQuoteSameEventUpgradeStaysInFirstTimeContext(t *testing.T) {
	f := newPricingFixture(t, catalog.PlanExperience)
	f.settledPurchase(f.eventID, catalog.PlanExperience, experienceList, 0, billing.ContextFirst, billing.StatusSettled)

	q := f.quote(t, catalog.PlanSignature)

	require.Equal(t, billing.PricingUpgrade, q.Reason)
	require.Equal(t, billing.ContextFirst, q.Context)
	require.Equal(t, catalog.PlanExperience, q.CurrentPlanCode)
	require.Equal(t, signatureList, q.BaseAmountMinor)
	require.Equal(t, experienceList, q.UpgradeCreditMinor)
	require.Equal(t, int64(3000), q.FinalAmountMinor)
}

func TestQuoteReturningUpgradeUsesLoyaltyValues(t *testing.T) {
	f := newPricingFixture(t, catalog.PlanExperience)
	f.settledPurchase(f.eventID, catalog.PlanExperience, experienceReturning, 0, billing.ContextReturning, billing.StatusSettled)

	q := f.quote(t, catalog.PlanSignature)

	require.Equal(t, billing.ContextReturning, q.Context)
	require.Equal(t, signatureReturning, q.BaseAmountMinor)
	require.Equal(t, experienceReturning, q.UpgradeCreditMinor)
	require.Equal(t, int64(2700), q.FinalAmountMinor)
}

// A coupon is a discount on the purchase it was used for. It must not shrink
// the commercial value of the tier, or a $10 coupon on Experience would turn
// into a $10 surcharge on the upgrade.
func TestQuoteUpgradeCreditIgnoresCouponOnTheFundingPurchase(t *testing.T) {
	f := newPricingFixture(t, catalog.PlanExperience)
	f.settledPurchase(f.eventID, catalog.PlanExperience, experienceList, 1000, billing.ContextFirst, billing.StatusSettled)

	q := f.quote(t, catalog.PlanSignature)

	require.Equal(t, experienceList, q.UpgradeCreditMinor, "credit is the tier's value, not the cash paid")
	require.Equal(t, int64(3000), q.FinalAmountMinor)
}

func TestQuoteUpgradeAfterRefundGivesNoCredit(t *testing.T) {
	f := newPricingFixture(t, catalog.PlanExperience)
	f.settledPurchase(f.eventID, catalog.PlanExperience, experienceList, 0, billing.ContextFirst, billing.StatusRefunded)

	q := f.quote(t, catalog.PlanSignature)

	require.Zero(t, q.UpgradeCreditMinor)
	require.Equal(t, signatureList, q.FinalAmountMinor)
}

func TestQuoteUpgradeAfterPartialRefundKeepsFullTierCredit(t *testing.T) {
	f := newPricingFixture(t, catalog.PlanExperience)
	f.settledPurchase(f.eventID, catalog.PlanExperience, experienceList, 0, billing.ContextFirst, billing.StatusPartiallyRefunded)

	q := f.quote(t, catalog.PlanSignature)

	require.Equal(t, experienceList, q.UpgradeCreditMinor)
	require.Equal(t, int64(3000), q.FinalAmountMinor)
}

// A comped or manually granted tier was never paid for, but the event does hold
// it. Charging the full Signature price would sell the same tier twice.
func TestQuoteUpgradeOfAnUnpaidTierCreditsTheTierValue(t *testing.T) {
	f := newPricingFixture(t, catalog.PlanExperience)

	q := f.quote(t, catalog.PlanSignature)

	require.Equal(t, experienceList, q.UpgradeCreditMinor)
	require.Equal(t, int64(3000), q.FinalAmountMinor)
}

func TestQuoteKeepsTheBreakdownConsistent(t *testing.T) {
	f := newPricingFixture(t, catalog.PlanExperience)
	f.settledPurchase(f.eventID, catalog.PlanExperience, experienceList, 0, billing.ContextFirst, billing.StatusSettled)

	q := f.quote(t, catalog.PlanSignature)

	require.Equal(t, q.BaseAmountMinor-q.DiscountAmountMinor, q.FinalAmountMinor)
	require.GreaterOrEqual(t, q.DiscountAmountMinor, q.UpgradeCreditMinor)
	require.Equal(t, "USD", q.Currency)
	require.NotEmpty(t, q.PricingVersion)
}

func TestHundredPercentPromotionKeepsSignatureTier(t *testing.T) {
	f := newPricingFixture(t, catalog.PlanFree)
	q := f.quote(t, catalog.PlanSignature)

	q, err := billing.ApplyPercentagePromotion(q, 100)
	require.NoError(t, err)
	require.Equal(t, catalog.PlanSignature, q.PlanCode)
	require.Equal(t, int64(0), q.FinalAmountMinor)
	require.Equal(t, signatureList, q.BaseAmountMinor, "a promotion discounts, it does not devalue the tier")
	require.Equal(t, billing.PricingPromotion, q.Reason)
}
