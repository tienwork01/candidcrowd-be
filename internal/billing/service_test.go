package billing_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/billing"
	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- In-memory test fakes ---

type inMemoryRepo struct {
	mu             sync.Mutex
	purchases      map[uuid.UUID]billing.Purchase
	idempotency    map[string]uuid.UUID                   // "hostID:key" -> purchaseID
	prices         map[string]billing.ProviderPrice       // "provider:versionID:currency" -> price
	products       map[string]billing.ProviderProduct     // "provider:planCode" -> product
	events         map[string]billing.ProviderEventRecord // "provider:eventID" -> record
	eventProcessed map[uuid.UUID]bool
	eventFailed    map[uuid.UUID]string
}

func newInMemoryRepo() *inMemoryRepo {
	return &inMemoryRepo{
		purchases:      make(map[uuid.UUID]billing.Purchase),
		idempotency:    make(map[string]uuid.UUID),
		prices:         make(map[string]billing.ProviderPrice),
		products:       make(map[string]billing.ProviderProduct),
		events:         make(map[string]billing.ProviderEventRecord),
		eventProcessed: make(map[uuid.UUID]bool),
		eventFailed:    make(map[uuid.UUID]string),
	}
}

func (r *inMemoryRepo) CreatePurchase(_ context.Context, p billing.Purchase) (billing.Purchase, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := fmt.Sprintf("%s:%s", p.HostID, p.IdempotencyKey)
	if _, ok := r.idempotency[key]; ok {
		return billing.Purchase{}, errors.New("duplicate idempotency key")
	}
	r.purchases[p.ID] = p
	r.idempotency[key] = p.ID
	return p, nil
}

func (r *inMemoryRepo) IdempotentPurchase(_ context.Context, hostID uuid.UUID, idempotencyKey string) (*billing.Purchase, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := fmt.Sprintf("%s:%s", hostID, idempotencyKey)
	id, ok := r.idempotency[key]
	if !ok {
		return nil, nil
	}
	p := r.purchases[id]
	return &p, nil
}

func (r *inMemoryRepo) GetPurchase(_ context.Context, id uuid.UUID) (billing.Purchase, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.purchases[id]
	if !ok {
		return billing.Purchase{}, billing.ErrPurchaseNotFound
	}
	return p, nil
}

func (r *inMemoryRepo) UpdateCheckout(_ context.Context, id uuid.UUID, checkoutID, providerPriceID, checkoutURL string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.purchases[id]
	if !ok {
		return billing.ErrPurchaseNotFound
	}
	p.ProviderCheckoutID = &checkoutID
	p.ProviderPriceID = providerPriceID
	p.CheckoutURL = &checkoutURL
	p.Status = billing.StatusCheckoutCreated
	r.purchases[id] = p
	return nil
}

func (r *inMemoryRepo) SettlePurchase(_ context.Context, in billing.SettleInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.purchases[in.PurchaseID]
	if !ok {
		return billing.ErrPurchaseNotFound
	}
	if p.Status == billing.StatusSettled {
		return billing.ErrPurchaseAlreadySettled
	}
	now := time.Now().UTC()
	p.Status = billing.StatusSettled
	p.ProviderTransactionID = &in.ProviderTransactionID
	p.SettledSubtotalMinor = &in.SettledSubtotalMinor
	p.SettledTaxMinor = &in.SettledTaxMinor
	p.SettledTotalMinor = &in.SettledTotalMinor
	p.SettledCurrency = &in.SettledCurrency
	p.SettledAt = &now
	r.purchases[in.PurchaseID] = p
	return nil
}

func (r *inMemoryRepo) UpdateStatus(_ context.Context, id uuid.UUID, status billing.PurchaseStatus, failureCode *string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.purchases[id]
	if !ok {
		return billing.ErrPurchaseNotFound
	}
	p.Status = status
	p.FailureCode = failureCode
	r.purchases[id] = p
	return nil
}

func (r *inMemoryRepo) MarkEntitlementApplied(_ context.Context, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.purchases[id]
	if !ok {
		return billing.ErrPurchaseNotFound
	}
	now := time.Now().UTC()
	p.EntitlementAppliedAt = &now
	r.purchases[id] = p
	return nil
}

func (r *inMemoryRepo) OpenPurchaseForEvent(_ context.Context, eventID uuid.UUID) (*billing.Purchase, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.purchases {
		if p.EventID == eventID && (p.Status == billing.StatusPending || p.Status == billing.StatusCheckoutCreated) {
			cp := p
			return &cp, nil
		}
	}
	return nil, nil
}

func (r *inMemoryRepo) LatestEventPurchase(_ context.Context, eventID uuid.UUID) (*billing.Purchase, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var latest *billing.Purchase
	for _, p := range r.purchases {
		if p.EventID != eventID || !p.Status.PostSettlement() {
			continue
		}
		cp := p
		if latest == nil || cp.CreatedAt.After(latest.CreatedAt) {
			latest = &cp
		}
	}
	return latest, nil
}

func (r *inMemoryRepo) HasEligibleCommercialPurchase(_ context.Context, hostID, excludeEventID uuid.UUID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.purchases {
		if p.HostID == hostID && p.EventID != excludeEventID && p.Status.Paid() &&
			p.SettledTotalMinor != nil && *p.SettledTotalMinor > 0 {
			return true, nil
		}
	}
	return false, nil
}

func (r *inMemoryRepo) RecordReversal(_ context.Context, in billing.ReversalInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.purchases[in.PurchaseID]
	if !ok {
		return billing.ErrPurchaseNotFound
	}
	p.Status = in.Status
	p.RefundedAmountMinor = in.RefundedAmountMinor
	r.purchases[in.PurchaseID] = p
	return nil
}

func (r *inMemoryRepo) UnappliedSettled(_ context.Context, limit int) ([]billing.Purchase, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var res []billing.Purchase
	for _, p := range r.purchases {
		if p.Status == billing.StatusSettled && p.EntitlementAppliedAt == nil {
			res = append(res, p)
			if len(res) >= limit {
				break
			}
		}
	}
	return res, nil
}

func (r *inMemoryRepo) InsertProviderEvent(_ context.Context, rec billing.ProviderEventRecord) (duplicate bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := fmt.Sprintf("%s:%s", rec.Provider, rec.ExternalEventID)
	if _, ok := r.events[key]; ok {
		return true, nil
	}
	r.events[key] = rec
	return false, nil
}

func (r *inMemoryRepo) MarkProviderEventProcessed(_ context.Context, id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eventProcessed[id] = true
	return nil
}

func (r *inMemoryRepo) MarkProviderEventFailed(_ context.Context, id uuid.UUID, errMsg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eventFailed[id] = errMsg
	return nil
}

func (r *inMemoryRepo) FindProviderPrice(_ context.Context, provider billing.Provider, planVersionID uuid.UUID, currency string, _ int64) (billing.ProviderPrice, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := fmt.Sprintf("%s:%s:%s", provider, planVersionID, currency)
	p, ok := r.prices[key]
	if !ok {
		return billing.ProviderPrice{}, billing.ErrProviderPriceNotFound
	}
	return p, nil
}

func (r *inMemoryRepo) FindProviderProduct(_ context.Context, provider billing.Provider, planCode string) (billing.ProviderProduct, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	product, ok := r.products[fmt.Sprintf("%s:%s", provider, planCode)]
	if !ok {
		return billing.ProviderProduct{}, billing.ErrProviderProductNotFound
	}
	return product, nil
}

func (r *inMemoryRepo) PurchaseByProviderTransaction(_ context.Context, provider billing.Provider, txnID string) (*billing.Purchase, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.purchases {
		matches := (p.ProviderTransactionID != nil && *p.ProviderTransactionID == txnID) ||
			(p.ProviderCheckoutID != nil && *p.ProviderCheckoutID == txnID)
		if p.Provider == provider && matches {
			cp := p
			return &cp, nil
		}
	}
	return nil, nil
}

type fakeGateway struct {
	mu     sync.Mutex
	target billing.CheckoutTarget
	err    error
	calls  []billing.CreateCheckoutInput
}

func (g *fakeGateway) CreateCheckout(_ context.Context, in billing.CreateCheckoutInput) (billing.CheckoutTarget, error) {
	g.mu.Lock()
	g.calls = append(g.calls, in)
	g.mu.Unlock()
	return g.target, g.err
}

// fakeLicenses records the entitlement side of refunds and chargebacks.
type fakeLicenses struct {
	mu            sync.Mutex
	consumed      map[uuid.UUID]bool // purchases whose license an event already used
	revoked       []uuid.UUID
	eventStatuses map[uuid.UUID]billing.EventBillingStatus
}

func newFakeLicenses() *fakeLicenses {
	return &fakeLicenses{
		consumed:      make(map[uuid.UUID]bool),
		eventStatuses: make(map[uuid.UUID]billing.EventBillingStatus),
	}
}

func (l *fakeLicenses) RevokeUnconsumedLicense(_ context.Context, purchaseID uuid.UUID) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.consumed[purchaseID] {
		return false, nil
	}
	l.revoked = append(l.revoked, purchaseID)
	return true, nil
}

func (l *fakeLicenses) SetEventBillingStatus(_ context.Context, eventID uuid.UUID, status billing.EventBillingStatus) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.eventStatuses[eventID] = status
	return nil
}

type fakeDecoder struct {
	event billing.ProviderEvent
	err   error
}

func (d *fakeDecoder) DecodeWebhook(_ []byte, _ string) (billing.ProviderEvent, error) {
	return d.event, d.err
}

type fakeGrantActivator struct {
	mu    sync.Mutex
	calls []billing.GrantInput
	err   error
}

func (a *fakeGrantActivator) ActivateForPurchase(_ context.Context, in billing.GrantInput) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, in)
	return a.err
}

type fakeCatalogReader struct {
	versions map[string]billing.CatalogVersion
	prices   map[string]billing.CatalogPrice // "versionID:currency" -> price
}

func (c *fakeCatalogReader) ActiveVersion(_ context.Context, code string) (billing.CatalogVersion, error) {
	v, ok := c.versions[code]
	if !ok {
		return billing.CatalogVersion{}, errors.New("plan not found")
	}
	return v, nil
}

func (c *fakeCatalogReader) CurrentPrice(_ context.Context, planVersionID uuid.UUID, currency string) (billing.CatalogPrice, error) {
	key := fmt.Sprintf("%s:%s", planVersionID, currency)
	p, ok := c.prices[key]
	if !ok {
		return billing.CatalogPrice{}, billing.ErrPriceNotAvailable
	}
	return p, nil
}

type fakeOwnership struct {
	owners map[string]bool // "eventID:hostID"
	emails map[uuid.UUID]string
}

func (o *fakeOwnership) OwnerOf(_ context.Context, eventID, hostID uuid.UUID) error {
	key := fmt.Sprintf("%s:%s", eventID, hostID)
	if !o.owners[key] {
		return errors.New("not owner")
	}
	return nil
}

func (o *fakeOwnership) HostEmail(_ context.Context, hostID uuid.UUID) (string, error) {
	return o.emails[hostID], nil
}

type fakeEntitlementReader struct {
	entitlements map[uuid.UUID]entitlement.EventEntitlement
}

func (e *fakeEntitlementReader) Resolve(_ context.Context, eventID uuid.UUID) (entitlement.EventEntitlement, error) {
	ent, ok := e.entitlements[eventID]
	if !ok {
		return entitlement.EventEntitlement{}, errors.New("entitlement not found")
	}
	return ent, nil
}

// --- Test Fixture Setup ---

// testEnv is the whole billing service with every collaborator faked.
type testEnv struct {
	svc      *billing.Service
	repo     *inMemoryRepo
	gw       *fakeGateway
	dec      *fakeDecoder
	grants   *fakeGrantActivator
	cat      *fakeCatalogReader
	own      *fakeOwnership
	entSvc   *fakeEntitlementReader
	licenses *fakeLicenses
}

func newTestEnv() *testEnv {
	e := &testEnv{
		repo:   newInMemoryRepo(),
		gw:     &fakeGateway{target: billing.CheckoutTarget{ExternalCheckoutID: "ext_chk_123"}},
		dec:    &fakeDecoder{},
		grants: &fakeGrantActivator{},
		cat: &fakeCatalogReader{
			versions: make(map[string]billing.CatalogVersion),
			prices:   make(map[string]billing.CatalogPrice),
		},
		own: &fakeOwnership{
			owners: make(map[string]bool),
			emails: make(map[uuid.UUID]string),
		},
		entSvc: &fakeEntitlementReader{
			entitlements: make(map[uuid.UUID]entitlement.EventEntitlement),
		},
		licenses: newFakeLicenses(),
	}
	e.svc = billing.NewService(billing.Config{
		Repo:       e.repo,
		Checkout:   e.gw,
		Decoder:    e.dec,
		Grants:     e.grants,
		Licenses:   e.licenses,
		Plans:      e.cat,
		EntSvc:     e.entSvc,
		Ownership:  e.own,
		Provider:   billing.ProviderPaddle,
		Currency:   "USD",
		Enabled:    true,
		SuccessURL: "https://app.example.com/return",
		CancelURL:  "https://app.example.com/cancel",
	})
	return e
}

func setupTestEnvironment() (*billing.Service, *inMemoryRepo, *fakeGateway, *fakeDecoder, *fakeGrantActivator, *fakeCatalogReader, *fakeOwnership, *fakeEntitlementReader) {
	e := newTestEnv()
	return e.svc, e.repo, e.gw, e.dec, e.grants, e.cat, e.own, e.entSvc
}

// --- Unit Tests ---

func TestCreateCheckout_Success(t *testing.T) {
	svc, repo, _, _, _, cat, own, entSvc := setupTestEnvironment()
	ctx := context.Background()

	eventID := uuid.New()
	hostID := uuid.New()
	versionID := uuid.New()

	own.owners[fmt.Sprintf("%s:%s", eventID, hostID)] = true
	own.emails[hostID] = "host@example.com"
	entSvc.entitlements[eventID] = entitlement.EventEntitlement{EventID: eventID, PlanCode: catalog.PlanFree}

	cat.versions[string(catalog.PlanExperience)] = billing.CatalogVersion{ID: versionID, Code: string(catalog.PlanExperience)}
	cat.prices[fmt.Sprintf("%s:USD", versionID)] = billing.CatalogPrice{AmountMinor: 3900, Currency: "USD"}

	repo.prices[fmt.Sprintf("%s:%s:USD", billing.ProviderPaddle, versionID)] = billing.ProviderPrice{
		ID:              uuid.New(),
		Provider:        billing.ProviderPaddle,
		PlanVersionID:   versionID,
		Currency:        "USD",
		ProviderPriceID: "pri_paddle_123",
		Active:          true,
	}

	out, err := svc.CreateCheckout(ctx, billing.CreateCheckoutCommand{
		EventID:        eventID,
		HostID:         hostID,
		PlanCode:       catalog.PlanExperience,
		Currency:       "USD",
		IdempotencyKey: "idem_key_1",
	})

	require.NoError(t, err)
	assert.Equal(t, billing.StatusCheckoutCreated, out.Status)
	assert.Equal(t, "ext_chk_123", out.CheckoutSessionID)

	p, err := repo.GetPurchase(ctx, out.PurchaseID)
	require.NoError(t, err)
	assert.Equal(t, int64(3900), p.QuotedAmountMinor)
	assert.Equal(t, "pri_paddle_123", repo.prices[fmt.Sprintf("%s:%s:USD", billing.ProviderPaddle, versionID)].ProviderPriceID)
}

func TestCreateCheckout_ReplayIdempotency(t *testing.T) {
	svc, repo, _, _, _, cat, own, entSvc := setupTestEnvironment()
	ctx := context.Background()

	eventID := uuid.New()
	hostID := uuid.New()
	versionID := uuid.New()

	own.owners[fmt.Sprintf("%s:%s", eventID, hostID)] = true
	entSvc.entitlements[eventID] = entitlement.EventEntitlement{EventID: eventID, PlanCode: catalog.PlanFree}
	cat.versions[string(catalog.PlanExperience)] = billing.CatalogVersion{ID: versionID, Code: string(catalog.PlanExperience)}
	cat.prices[fmt.Sprintf("%s:USD", versionID)] = billing.CatalogPrice{AmountMinor: 3900, Currency: "USD"}
	repo.prices[fmt.Sprintf("%s:%s:USD", billing.ProviderPaddle, versionID)] = billing.ProviderPrice{
		ID:              uuid.New(),
		Provider:        billing.ProviderPaddle,
		PlanVersionID:   versionID,
		Currency:        "USD",
		ProviderPriceID: "pri_123",
		Active:          true,
	}

	cmd := billing.CreateCheckoutCommand{
		EventID:        eventID,
		HostID:         hostID,
		PlanCode:       catalog.PlanExperience,
		Currency:       "USD",
		IdempotencyKey: "idem_replay",
	}

	out1, err := svc.CreateCheckout(ctx, cmd)
	require.NoError(t, err)

	out2, err := svc.CreateCheckout(ctx, cmd)
	require.NoError(t, err)

	assert.Equal(t, out1.PurchaseID, out2.PurchaseID)
	assert.Equal(t, out1.CheckoutSessionID, out2.CheckoutSessionID)
}

func TestCreateCheckout_RejectsIdempotencyKeyWithDifferentPayload(t *testing.T) {
	svc, repo, _, _, _, _, _, _ := setupTestEnvironment()
	hostID := uuid.New()
	existingID := uuid.New()
	repo.purchases[existingID] = billing.Purchase{
		ID: existingID, EventID: uuid.New(), HostID: hostID, PlanCode: catalog.PlanExperience,
		QuotedCurrency: "USD", IdempotencyKey: "same-key",
	}
	repo.idempotency[fmt.Sprintf("%s:%s", hostID, "same-key")] = existingID

	_, err := svc.CreateCheckout(context.Background(), billing.CreateCheckoutCommand{
		EventID: uuid.New(), HostID: hostID, PlanCode: catalog.PlanSignature,
		Currency: "USD", IdempotencyKey: "same-key",
	})
	require.ErrorIs(t, err, billing.ErrIdempotencyConflict)
}

func TestCreateCheckout_NonOwnerRejected(t *testing.T) {
	svc, _, _, _, _, _, _, _ := setupTestEnvironment()
	ctx := context.Background()

	_, err := svc.CreateCheckout(ctx, billing.CreateCheckoutCommand{
		EventID:        uuid.New(),
		HostID:         uuid.New(),
		PlanCode:       catalog.PlanExperience,
		Currency:       "USD",
		IdempotencyKey: "idem_non_owner",
	})

	assert.ErrorIs(t, err, billing.ErrPurchaseNotFound)
}

func TestCreateCheckout_CannotPurchaseFreePlan(t *testing.T) {
	svc, _, _, _, _, _, own, entSvc := setupTestEnvironment()
	ctx := context.Background()

	eventID := uuid.New()
	hostID := uuid.New()
	own.owners[fmt.Sprintf("%s:%s", eventID, hostID)] = true
	entSvc.entitlements[eventID] = entitlement.EventEntitlement{EventID: eventID, PlanCode: catalog.PlanFree}

	_, err := svc.CreateCheckout(ctx, billing.CreateCheckoutCommand{
		EventID:        eventID,
		HostID:         hostID,
		PlanCode:       catalog.PlanFree,
		Currency:       "USD",
		IdempotencyKey: "idem_free",
	})

	assert.ErrorIs(t, err, billing.ErrInvalidPlanTransition)
}

func TestCreateCheckout_CannotDowngrade(t *testing.T) {
	svc, _, _, _, _, _, own, entSvc := setupTestEnvironment()
	ctx := context.Background()

	eventID := uuid.New()
	hostID := uuid.New()
	own.owners[fmt.Sprintf("%s:%s", eventID, hostID)] = true
	entSvc.entitlements[eventID] = entitlement.EventEntitlement{EventID: eventID, PlanCode: catalog.PlanSignature}

	_, err := svc.CreateCheckout(ctx, billing.CreateCheckoutCommand{
		EventID:        eventID,
		HostID:         hostID,
		PlanCode:       catalog.PlanExperience,
		Currency:       "USD",
		IdempotencyKey: "idem_downgrade",
	})

	assert.ErrorIs(t, err, billing.ErrInvalidPlanTransition)
}

func TestCreateCheckout_MissingProviderPriceMapping(t *testing.T) {
	svc, _, _, _, _, cat, own, entSvc := setupTestEnvironment()
	ctx := context.Background()

	eventID := uuid.New()
	hostID := uuid.New()
	versionID := uuid.New()

	own.owners[fmt.Sprintf("%s:%s", eventID, hostID)] = true
	entSvc.entitlements[eventID] = entitlement.EventEntitlement{EventID: eventID, PlanCode: catalog.PlanFree}
	cat.versions[string(catalog.PlanExperience)] = billing.CatalogVersion{ID: versionID, Code: string(catalog.PlanExperience)}
	cat.prices[fmt.Sprintf("%s:USD", versionID)] = billing.CatalogPrice{AmountMinor: 3900, Currency: "USD"}
	// deliberately don't configure provider price

	_, err := svc.CreateCheckout(ctx, billing.CreateCheckoutCommand{
		EventID:        eventID,
		HostID:         hostID,
		PlanCode:       catalog.PlanExperience,
		Currency:       "USD",
		IdempotencyKey: "idem_no_price",
	})

	assert.ErrorIs(t, err, billing.ErrProviderPriceNotFound)
}

func TestHandleProviderEvent_Settled_GrantsEntitlement(t *testing.T) {
	svc, repo, _, dec, grants, _, _, _ := setupTestEnvironment()
	ctx := context.Background()

	eventID := uuid.New()
	hostID := uuid.New()
	purchaseID := uuid.New()
	versionID := uuid.New()

	repo.purchases[purchaseID] = billing.Purchase{
		ID:                purchaseID,
		EventID:           eventID,
		HostID:            hostID,
		PlanCode:          catalog.PlanExperience,
		PlanVersionID:     versionID,
		QuotedAmountMinor: 3900,
		QuotedCurrency:    "USD",
		ProviderPriceID:   "pri_paddle_123",
		Status:            billing.StatusCheckoutCreated,
		Provider:          billing.ProviderPaddle,
	}

	repo.prices[fmt.Sprintf("%s:%s:USD", billing.ProviderPaddle, versionID)] = billing.ProviderPrice{
		ID:              uuid.New(),
		Provider:        billing.ProviderPaddle,
		PlanVersionID:   versionID,
		Currency:        "USD",
		ProviderPriceID: "pri_paddle_123",
		Active:          true,
	}

	dec.event = billing.ProviderEvent{
		ExternalEventID:       "evt_paddle_1",
		Type:                  billing.EventPaymentSettled,
		ExternalTransactionID: "txn_paddle_999",
		PurchaseID:            purchaseID,
		PriceID:               "pri_paddle_123",
		Subtotal:              billing.Money{AmountMinor: 3900, Currency: "USD"},
		Tax:                   billing.Money{AmountMinor: 0, Currency: "USD"},
		Total:                 billing.Money{AmountMinor: 3900, Currency: "USD"},
		OccurredAt:            time.Now(),
	}

	err := svc.HandleProviderEvent(ctx, []byte(`{"event":"test"}`), "signature")
	require.NoError(t, err)

	p, err := repo.GetPurchase(ctx, purchaseID)
	require.NoError(t, err)
	assert.Equal(t, billing.StatusSettled, p.Status)
	assert.NotNil(t, p.EntitlementAppliedAt)

	require.Len(t, grants.calls, 1)
	assert.Equal(t, eventID, grants.calls[0].EventID)
	assert.Equal(t, "experience", grants.calls[0].PlanCode)
	assert.Equal(t, purchaseID, grants.calls[0].PurchaseID)
}

func TestHandleProviderEvent_DuplicateWebhook_NoDoubleGrant(t *testing.T) {
	svc, repo, _, dec, grants, _, _, _ := setupTestEnvironment()
	ctx := context.Background()

	eventID := uuid.New()
	purchaseID := uuid.New()
	versionID := uuid.New()

	repo.purchases[purchaseID] = billing.Purchase{
		ID:                purchaseID,
		EventID:           eventID,
		PlanCode:          catalog.PlanExperience,
		PlanVersionID:     versionID,
		QuotedAmountMinor: 3900,
		QuotedCurrency:    "USD",
		ProviderPriceID:   "pri_123",
		Status:            billing.StatusCheckoutCreated,
		Provider:          billing.ProviderPaddle,
	}
	repo.prices[fmt.Sprintf("%s:%s:USD", billing.ProviderPaddle, versionID)] = billing.ProviderPrice{
		ProviderPriceID: "pri_123",
		Active:          true,
	}

	dec.event = billing.ProviderEvent{
		ExternalEventID:       "evt_dup_1",
		Type:                  billing.EventPaymentSettled,
		ExternalTransactionID: "txn_123",
		PurchaseID:            purchaseID,
		PriceID:               "pri_123",
		Subtotal:              billing.Money{AmountMinor: 3900, Currency: "USD"},
		Tax:                   billing.Money{AmountMinor: 0, Currency: "USD"},
		Total:                 billing.Money{AmountMinor: 3900, Currency: "USD"},
	}

	payload := []byte(`{"event_id":"evt_dup_1"}`)

	// First delivery
	err := svc.HandleProviderEvent(ctx, payload, "sig")
	require.NoError(t, err)
	assert.Len(t, grants.calls, 1)

	// Second delivery (duplicate)
	err = svc.HandleProviderEvent(ctx, payload, "sig")
	require.NoError(t, err)
	// Still exactly 1 grant call
	assert.Len(t, grants.calls, 1)
}

func TestHandleProviderEvent_PriceMismatch_IntegrityError(t *testing.T) {
	svc, repo, _, dec, grants, _, _, _ := setupTestEnvironment()
	ctx := context.Background()

	purchaseID := uuid.New()
	versionID := uuid.New()

	repo.purchases[purchaseID] = billing.Purchase{
		ID:              purchaseID,
		PlanCode:        catalog.PlanExperience,
		PlanVersionID:   versionID,
		QuotedCurrency:  "USD",
		ProviderPriceID: "pri_expected",
		Status:          billing.StatusCheckoutCreated,
		Provider:        billing.ProviderPaddle,
	}
	repo.prices[fmt.Sprintf("%s:%s:USD", billing.ProviderPaddle, versionID)] = billing.ProviderPrice{
		ProviderPriceID: "pri_expected",
		Active:          true,
	}

	dec.event = billing.ProviderEvent{
		ExternalEventID: "evt_tampered",
		Type:            billing.EventPaymentSettled,
		PurchaseID:      purchaseID,
		PriceID:         "pri_wrong_price_id", // mismatch!
	}

	err := svc.HandleProviderEvent(ctx, []byte(`{"event_id":"evt_tampered"}`), "sig")
	assert.ErrorIs(t, err, billing.ErrPaymentIntegrityMismatch)
	assert.Empty(t, grants.calls)
}

func TestHandleProviderEvent_RefundDispute_DoesNotRevokeMedia(t *testing.T) {
	svc, repo, _, dec, _, _, _, _ := setupTestEnvironment()
	ctx := context.Background()

	purchaseID := uuid.New()
	repo.purchases[purchaseID] = billing.Purchase{
		ID:       purchaseID,
		PlanCode: catalog.PlanExperience,
		Status:   billing.StatusSettled,
		Provider: billing.ProviderPaddle,
	}

	dec.event = billing.ProviderEvent{
		ExternalEventID: "evt_refund_1",
		Type:            billing.EventPaymentRefunded,
		PurchaseID:      purchaseID,
	}

	err := svc.HandleProviderEvent(ctx, []byte(`{"event_id":"evt_refund_1"}`), "sig")
	require.NoError(t, err)

	p, err := repo.GetPurchase(ctx, purchaseID)
	require.NoError(t, err)
	assert.Equal(t, billing.StatusRefunded, p.Status)
}

func TestReconcileUnapplied(t *testing.T) {
	svc, repo, _, _, grants, _, _, _ := setupTestEnvironment()
	ctx := context.Background()

	eventID := uuid.New()
	purchaseID := uuid.New()

	now := time.Now().UTC()
	repo.purchases[purchaseID] = billing.Purchase{
		ID:                   purchaseID,
		EventID:              eventID,
		PlanCode:             catalog.PlanExperience,
		Status:               billing.StatusSettled,
		SettledAt:            &now,
		EntitlementAppliedAt: nil, // unapplied!
	}

	applied, errs := svc.ReconcileUnapplied(ctx, 10)
	assert.Empty(t, errs)
	assert.Equal(t, 1, applied)

	p, err := repo.GetPurchase(ctx, purchaseID)
	require.NoError(t, err)
	assert.NotNil(t, p.EntitlementAppliedAt)
	assert.Len(t, grants.calls, 1)
}
