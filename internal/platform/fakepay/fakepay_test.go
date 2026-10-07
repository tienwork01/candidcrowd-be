package fakepay_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/billing"
	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/fakepay"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// In the portability drill, we test plugging in fakepay without touching
// internal/billing, catalog, or entitlement.

type testRepo struct {
	purchases map[uuid.UUID]billing.Purchase
	prices    map[string]billing.ProviderPrice
	events    map[string]bool
}

func (r *testRepo) CreatePurchase(_ context.Context, p billing.Purchase) (billing.Purchase, error) {
	r.purchases[p.ID] = p
	return p, nil
}
func (r *testRepo) IdempotentPurchase(_ context.Context, _ uuid.UUID, _ string) (*billing.Purchase, error) {
	return nil, nil
}
func (r *testRepo) GetPurchase(_ context.Context, id uuid.UUID) (billing.Purchase, error) {
	p, ok := r.purchases[id]
	if !ok {
		return billing.Purchase{}, billing.ErrPurchaseNotFound
	}
	return p, nil
}
func (r *testRepo) UpdateCheckout(_ context.Context, id uuid.UUID, cid, priceID, url string) error {
	p := r.purchases[id]
	p.ProviderCheckoutID = &cid
	p.ProviderPriceID = priceID
	p.CheckoutURL = &url
	p.Status = billing.StatusCheckoutCreated
	r.purchases[id] = p
	return nil
}
func (r *testRepo) SettlePurchase(_ context.Context, in billing.SettleInput) error {
	p := r.purchases[in.PurchaseID]
	p.Status = billing.StatusSettled
	r.purchases[in.PurchaseID] = p
	return nil
}
func (r *testRepo) UpdateStatus(_ context.Context, id uuid.UUID, s billing.PurchaseStatus, _ *string) error {
	p := r.purchases[id]
	p.Status = s
	r.purchases[id] = p
	return nil
}
func (r *testRepo) MarkEntitlementApplied(_ context.Context, id uuid.UUID) error {
	now := time.Now()
	p := r.purchases[id]
	p.EntitlementAppliedAt = &now
	r.purchases[id] = p
	return nil
}
func (r *testRepo) OpenPurchaseForEvent(_ context.Context, _ uuid.UUID) (*billing.Purchase, error) {
	return nil, nil
}
func (r *testRepo) HasEligibleCommercialPurchase(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return false, nil
}
func (r *testRepo) LatestEventPurchase(context.Context, uuid.UUID) (*billing.Purchase, error) {
	return nil, nil
}
func (r *testRepo) RecordReversal(_ context.Context, in billing.ReversalInput) error {
	p := r.purchases[in.PurchaseID]
	p.Status = in.Status
	p.RefundedAmountMinor = in.RefundedAmountMinor
	r.purchases[in.PurchaseID] = p
	return nil
}
func (r *testRepo) FindProviderProduct(_ context.Context, _ billing.Provider, _ string) (billing.ProviderProduct, error) {
	return billing.ProviderProduct{}, billing.ErrProviderProductNotFound
}
func (r *testRepo) UnappliedSettled(_ context.Context, _ int) ([]billing.Purchase, error) {
	return nil, nil
}
func (r *testRepo) InsertProviderEvent(_ context.Context, rec billing.ProviderEventRecord) (bool, error) {
	key := fmt.Sprintf("%s:%s", rec.Provider, rec.ExternalEventID)
	if r.events[key] {
		return true, nil
	}
	r.events[key] = true
	return false, nil
}
func (r *testRepo) MarkProviderEventProcessed(_ context.Context, _ uuid.UUID) error { return nil }
func (r *testRepo) MarkProviderEventFailed(_ context.Context, _ uuid.UUID, _ string) error {
	return nil
}
func (r *testRepo) FindProviderPrice(_ context.Context, provider billing.Provider, vid uuid.UUID, curr string, _ int64) (billing.ProviderPrice, error) {
	key := fmt.Sprintf("%s:%s:%s", provider, vid, curr)
	p, ok := r.prices[key]
	if !ok {
		return billing.ProviderPrice{}, billing.ErrProviderPriceNotFound
	}
	return p, nil
}
func (r *testRepo) PurchaseByProviderTransaction(_ context.Context, _ billing.Provider, _ string) (*billing.Purchase, error) {
	return nil, nil
}

type testPlans struct{ vid uuid.UUID }

func (p *testPlans) ActiveVersion(_ context.Context, code string) (billing.CatalogVersion, error) {
	return billing.CatalogVersion{ID: p.vid, Code: code}, nil
}
func (p *testPlans) CurrentPrice(_ context.Context, _ uuid.UUID, curr string) (billing.CatalogPrice, error) {
	return billing.CatalogPrice{AmountMinor: 4900, Currency: curr}, nil
}

type testOwnership struct{}

func (testOwnership) OwnerOf(_ context.Context, _, _ uuid.UUID) error { return nil }
func (testOwnership) HostEmail(_ context.Context, _ uuid.UUID) (string, error) {
	return "host@test.com", nil
}

type testEntitlements struct{}

func (testEntitlements) Resolve(_ context.Context, eid uuid.UUID) (entitlement.EventEntitlement, error) {
	return entitlement.EventEntitlement{EventID: eid, PlanCode: catalog.PlanFree}, nil
}

type testGrants struct {
	granted []billing.GrantInput
}

func (g *testGrants) ActivateForPurchase(_ context.Context, in billing.GrantInput) error {
	g.granted = append(g.granted, in)
	return nil
}

func TestProviderReplacementDrill(t *testing.T) {
	ctx := context.Background()
	providerFakepay := billing.Provider("fakepay")

	gw := fakepay.NewGateway("https://fakepay.test")
	decoder := fakepay.NewDecoder()

	repo := &testRepo{
		purchases: make(map[uuid.UUID]billing.Purchase),
		prices:    make(map[string]billing.ProviderPrice),
		events:    make(map[string]bool),
	}

	planVersionID := uuid.New()
	eventID := uuid.New()
	hostID := uuid.New()

	repo.prices[fmt.Sprintf("%s:%s:USD", providerFakepay, planVersionID)] = billing.ProviderPrice{
		ID:              uuid.New(),
		Provider:        providerFakepay,
		PlanVersionID:   planVersionID,
		Currency:        "USD",
		ProviderPriceID: "fake_pri_experience",
		Active:          true,
	}

	grants := &testGrants{}

	svc := billing.NewService(billing.Config{
		Repo:       repo,
		Checkout:   gw,
		Decoder:    decoder,
		Grants:     grants,
		Plans:      &testPlans{vid: planVersionID},
		Ownership:  testOwnership{},
		EntSvc:     testEntitlements{},
		Provider:   providerFakepay,
		Currency:   "USD",
		Enabled:    true,
		SuccessURL: "https://candidcrowd.test/return",
	})

	// 1. Create checkout through Fakepay
	out, err := svc.CreateCheckout(ctx, billing.CreateCheckoutCommand{
		EventID:        eventID,
		HostID:         hostID,
		PlanCode:       catalog.PlanExperience,
		Currency:       "USD",
		IdempotencyKey: "drill_key_1",
	})
	require.NoError(t, err)
	assert.Contains(t, out.CheckoutSessionID, "fake_chk_")
	assert.Equal(t, billing.StatusCheckoutCreated, out.Status)

	// 2. Settle payment via webhook
	webhookPayload, err := json.Marshal(map[string]any{
		"event_id":       "evt_fake_999",
		"type":           "payment.completed",
		"transaction_id": "txn_fake_888",
		"purchase_id":    out.PurchaseID.String(),
		"price_id":       "fake_pri_experience",
		"amount_minor":   4900,
		"currency":       "USD",
	})
	require.NoError(t, err)

	err = svc.HandleProviderEvent(ctx, webhookPayload, "valid_fake_sig")
	require.NoError(t, err)

	// 3. Verify purchase was settled and entitlement granted
	p, err := repo.GetPurchase(ctx, out.PurchaseID)
	require.NoError(t, err)
	assert.Equal(t, billing.StatusSettled, p.Status)
	assert.NotNil(t, p.EntitlementAppliedAt)

	require.Len(t, grants.granted, 1)
	assert.Equal(t, eventID, grants.granted[0].EventID)
	assert.Equal(t, "experience", grants.granted[0].PlanCode)
}
