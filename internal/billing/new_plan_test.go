package billing_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/candidcrowd/candidcrowd-backend/internal/billing"
	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A tier this binary has never heard of, added the way a real one would be:
// a row in the plan registry plus a catalog version and price.
const (
	planPremier  = catalog.PlanCode("premier")
	premierList  = int64(12900)
	premierLoyal = int64(10900)
)

// withPremierPlan registers a fourth tier for one test and restores the catalog
// afterwards.
func withPremierPlan(t *testing.T) {
	t.Helper()
	original := catalog.Plans()
	catalog.SetRegistry(append(append([]catalog.PlanDefinition{}, original...),
		catalog.PlanDefinition{Code: planPremier, TierRank: 3, Sellable: true}))
	t.Cleanup(func() { catalog.SetRegistry(original) })
}

func (f *pricingFixture) addPlan(plan catalog.PlanCode, list, returning int64) {
	versionID := uuid.New()
	f.versions[plan] = versionID
	f.cat.versions[string(plan)] = billing.CatalogVersion{ID: versionID, Code: string(plan)}
	f.cat.prices[fmt.Sprintf("%s:USD", versionID)] = billing.CatalogPrice{
		AmountMinor: list, ReturningAmountMinor: &returning, Currency: "USD",
	}
}

// Everything below exercises a plan that appears in no Go source file except
// this test. If any of it needed a code change, these would not compile or pass.

func TestNewPlanIsSellableWithoutCodeChanges(t *testing.T) {
	withPremierPlan(t)
	f := newPricingFixture(t, catalog.PlanFree)
	f.addPlan(planPremier, premierList, premierLoyal)

	require.Equal(t,
		[]catalog.PlanCode{catalog.PlanExperience, catalog.PlanSignature, planPremier},
		catalog.SellablePlans())

	q := f.quote(t, planPremier)

	require.Equal(t, premierList, q.FinalAmountMinor)
	require.Equal(t, billing.PricingFirstPurchase, q.Reason)
}

func TestNewPlanGetsLoyaltyPricingFromTheCatalog(t *testing.T) {
	withPremierPlan(t)
	f := newPricingFixture(t, catalog.PlanFree)
	f.addPlan(planPremier, premierList, premierLoyal)
	f.settledPurchase(uuid.New(), catalog.PlanExperience, experienceList, 0, billing.ContextFirst, billing.StatusSettled)

	q := f.quote(t, planPremier)

	require.Equal(t, premierLoyal, q.FinalAmountMinor)
	require.Equal(t, billing.ContextReturning, q.Context)
}

func TestUpgradeIntoANewTopTierPricesItself(t *testing.T) {
	withPremierPlan(t)
	f := newPricingFixture(t, catalog.PlanSignature)
	f.addPlan(planPremier, premierList, premierLoyal)
	f.settledPurchase(f.eventID, catalog.PlanSignature, signatureList, 0, billing.ContextFirst, billing.StatusSettled)

	q := f.quote(t, planPremier)

	require.Equal(t, billing.PricingUpgrade, q.Reason)
	require.Equal(t, premierList, q.BaseAmountMinor)
	require.Equal(t, signatureList, q.UpgradeCreditMinor)
	require.Equal(t, premierList-signatureList, q.FinalAmountMinor)
}

// Inserting a tier between two existing ones must not break the upgrade order
// or let anyone move sideways or down.
func TestTransitionsFollowRegistryRankNotHardcodedOrder(t *testing.T) {
	original := catalog.Plans()
	catalog.SetRegistry([]catalog.PlanDefinition{
		{Code: catalog.PlanFree, TierRank: 0},
		{Code: catalog.PlanExperience, TierRank: 1, Sellable: true},
		{Code: planPremier, TierRank: 2, Sellable: true},
		{Code: catalog.PlanSignature, TierRank: 3, Sellable: true},
	})
	t.Cleanup(func() { catalog.SetRegistry(original) })

	experience, premier, signature := catalog.PlanExperience, planPremier, catalog.PlanSignature
	require.NoError(t, entitlement.CheckTransition(&experience, premier))
	require.NoError(t, entitlement.CheckTransition(&premier, signature))
	require.Error(t, entitlement.CheckTransition(&signature, premier))

	ent := entitlement.EventEntitlement{PlanCode: catalog.PlanExperience}
	require.Equal(t, []catalog.PlanCode{planPremier, catalog.PlanSignature}, ent.UpgradeOptions())
}

// A plan can exist without being for sale — a trial tier, or one being retired
// from the price list while its grants stay valid.
func TestANonSellablePlanIsNeverQuoted(t *testing.T) {
	original := catalog.Plans()
	catalog.SetRegistry(append(append([]catalog.PlanDefinition{}, original...),
		catalog.PlanDefinition{Code: planPremier, TierRank: 3, Sellable: false}))
	t.Cleanup(func() { catalog.SetRegistry(original) })

	f := newPricingFixture(t, catalog.PlanFree)
	f.addPlan(planPremier, premierList, premierLoyal)

	assert.NotContains(t, catalog.SellablePlans(), planPremier)
	_, err := f.svc.QuoteEventPurchase(context.Background(), f.eventID, f.hostID, planPremier, "USD")
	assert.ErrorIs(t, err, billing.ErrInvalidPlanTransition)
}

// A currency with no loyalty price published falls back to the list price
// instead of pricing from a map that only knows USD.
func TestAPlanWithoutALoyaltyPriceKeepsItsListPrice(t *testing.T) {
	f := newPricingFixture(t, catalog.PlanFree)
	versionID := f.versions[catalog.PlanExperience]
	f.cat.prices[fmt.Sprintf("%s:USD", versionID)] = billing.CatalogPrice{
		AmountMinor: experienceList, Currency: "USD",
	}
	f.settledPurchase(uuid.New(), catalog.PlanSignature, signatureList, 0, billing.ContextFirst, billing.StatusSettled)

	q := f.quote(t, catalog.PlanExperience)

	require.Equal(t, billing.ContextReturning, q.Context)
	require.Equal(t, experienceList, q.FinalAmountMinor)
}
