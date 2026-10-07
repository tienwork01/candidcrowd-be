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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Provider execution: the quote decides the amount, the provider executes ---

func TestCheckoutPricesInlineWhenNoFixedPriceMapping(t *testing.T) {
	f := newPricingFixture(t, catalog.PlanExperience)
	f.settledPurchase(f.eventID, catalog.PlanExperience, experienceList, 0, billing.ContextFirst, billing.StatusSettled)
	f.repo.products[fmt.Sprintf("%s:%s", billing.ProviderPaddle, catalog.PlanSignature)] = billing.ProviderProduct{
		Provider: billing.ProviderPaddle, PlanCode: catalog.PlanSignature,
		ProviderProductID: "pro_signature", Active: true,
	}
	f.gw.target = billing.CheckoutTarget{ExternalCheckoutID: "txn_up", ProviderPriceID: "pri_inline_generated"}

	out, err := f.svc.CreateCheckout(context.Background(), billing.CreateCheckoutCommand{
		EventID: f.eventID, HostID: f.hostID, PlanCode: catalog.PlanSignature,
		Currency: "USD", IdempotencyKey: "idem_inline",
	})
	require.NoError(t, err)

	require.Len(t, f.gw.calls, 1)
	call := f.gw.calls[0]
	assert.Empty(t, call.ProviderPriceID, "no fixed mapping exists for an upgrade amount")
	assert.Equal(t, "pro_signature", call.ProviderProductID)
	assert.Equal(t, int64(3000), call.AmountMinor, "the provider executes the quote, it does not reprice")
	assert.Equal(t, "USD", call.Currency)

	// The price the provider created is snapshotted, so settlement can verify it.
	p, err := f.repo.GetPurchase(context.Background(), out.PurchaseID)
	require.NoError(t, err)
	assert.Equal(t, "pri_inline_generated", p.ProviderPriceID)
	assert.Equal(t, int64(3000), p.QuotedAmountMinor)
	assert.Equal(t, experienceList, p.UpgradeCreditMinor)
	assert.Equal(t, billing.ContextFirst, p.PricingContext)
}

func TestCheckoutFailsWhenNeitherPriceNorProductIsConfigured(t *testing.T) {
	f := newPricingFixture(t, catalog.PlanFree)

	_, err := f.svc.CreateCheckout(context.Background(), billing.CreateCheckoutCommand{
		EventID: f.eventID, HostID: f.hostID, PlanCode: catalog.PlanExperience,
		Currency: "USD", IdempotencyKey: "idem_unconfigured",
	})
	assert.ErrorIs(t, err, billing.ErrProviderPriceNotFound)
	assert.Empty(t, f.gw.calls, "nothing is sent to the provider without an execution target")
}

// --- Duplicate checkout protection ---

func TestDoubleClickCreatesOneProviderTransaction(t *testing.T) {
	f := newPricingFixture(t, catalog.PlanFree)
	f.repo.products[fmt.Sprintf("%s:%s", billing.ProviderPaddle, catalog.PlanExperience)] = billing.ProviderProduct{
		Provider: billing.ProviderPaddle, PlanCode: catalog.PlanExperience,
		ProviderProductID: "pro_experience", Active: true,
	}
	ctx := context.Background()
	cmd := billing.CreateCheckoutCommand{
		EventID: f.eventID, HostID: f.hostID, PlanCode: catalog.PlanExperience, Currency: "USD",
	}

	cmd.IdempotencyKey = "click_1"
	first, err := f.svc.CreateCheckout(ctx, cmd)
	require.NoError(t, err)

	// Same key (a retried request) and a fresh key (a second click) both resolve
	// to the purchase that is already open.
	cmd.IdempotencyKey = "click_1"
	retry, err := f.svc.CreateCheckout(ctx, cmd)
	require.NoError(t, err)
	cmd.IdempotencyKey = "click_2"
	second, err := f.svc.CreateCheckout(ctx, cmd)
	require.NoError(t, err)

	assert.Equal(t, first.PurchaseID, retry.PurchaseID)
	assert.Equal(t, first.PurchaseID, second.PurchaseID)
	assert.Len(t, f.gw.calls, 1, "one checkout reaches the provider")
}

// --- Webhook ordering and duplication ---

type webhookFixture struct {
	*testEnv
	purchaseID uuid.UUID
	eventID    uuid.UUID
}

// newWebhookFixture seeds a purchase that has already been paid for.
func newWebhookFixture(t *testing.T, status billing.PurchaseStatus) *webhookFixture {
	t.Helper()
	f := &webhookFixture{testEnv: newTestEnv(), purchaseID: uuid.New(), eventID: uuid.New()}
	total := int64(3900)
	settledAt := time.Now().UTC()
	p := billing.Purchase{
		ID: f.purchaseID, EventID: f.eventID, HostID: uuid.New(),
		PlanCode: catalog.PlanExperience, PlanVersionID: uuid.New(),
		QuotedAmountMinor: total, BaseAmountMinor: total, QuotedCurrency: "USD",
		Status: status, Provider: billing.ProviderPaddle, ProviderPriceID: "pri_x",
	}
	if status.PostSettlement() {
		p.SettledTotalMinor = &total
		p.SettledAt = &settledAt
		applied := settledAt
		p.EntitlementAppliedAt = &applied
	}
	f.repo.purchases[f.purchaseID] = p
	return f
}

func (f *webhookFixture) deliver(t *testing.T, ev billing.ProviderEvent) error {
	t.Helper()
	f.dec.event = ev
	return f.svc.HandleProviderEvent(context.Background(), []byte(`{"event_id":"`+ev.ExternalEventID+`"}`), "sig")
}

func (f *webhookFixture) purchase(t *testing.T) billing.Purchase {
	t.Helper()
	p, err := f.repo.GetPurchase(context.Background(), f.purchaseID)
	require.NoError(t, err)
	return p
}

func settledEvent(id string, purchaseID uuid.UUID) billing.ProviderEvent {
	return billing.ProviderEvent{
		ExternalEventID: id, Type: billing.EventPaymentSettled, PurchaseID: purchaseID,
		ExternalTransactionID: "txn_1", PriceID: "pri_x",
		Subtotal: billing.Money{AmountMinor: 3900, Currency: "USD"},
		Total:    billing.Money{AmountMinor: 3900, Currency: "USD"},
	}
}

// A completed notification arriving after the money went back must not put the
// purchase back into a paid state.
func TestSettlementAfterRefundIsIgnored(t *testing.T) {
	f := newWebhookFixture(t, billing.StatusRefunded)

	require.NoError(t, f.deliver(t, settledEvent("evt_late_completed", f.purchaseID)))

	assert.Equal(t, billing.StatusRefunded, f.purchase(t).Status)
}

// The reverse order is legitimate: a failed attempt followed by a successful
// retry of the same transaction.
func TestSettlementAfterFailureStillSettles(t *testing.T) {
	f := newWebhookFixture(t, billing.StatusCheckoutCreated)
	require.NoError(t, f.deliver(t, billing.ProviderEvent{
		ExternalEventID: "evt_failed", Type: billing.EventPaymentFailed, PurchaseID: f.purchaseID,
	}))
	require.Equal(t, billing.StatusFailed, f.purchase(t).Status)

	require.NoError(t, f.deliver(t, settledEvent("evt_completed", f.purchaseID)))

	assert.Equal(t, billing.StatusSettled, f.purchase(t).Status)
	assert.Len(t, f.grants.calls, 1)
}

// A failure notification that overtakes the completion must not undo it.
func TestFailureAfterSettlementIsIgnored(t *testing.T) {
	f := newWebhookFixture(t, billing.StatusSettled)

	require.NoError(t, f.deliver(t, billing.ProviderEvent{
		ExternalEventID: "evt_late_failed", Type: billing.EventPaymentCanceled, PurchaseID: f.purchaseID,
	}))

	assert.Equal(t, billing.StatusSettled, f.purchase(t).Status)
}

// A redelivered completion is the fastest repair path for a settlement whose
// entitlement never applied, and it must still grant exactly once.
func TestRepeatedSettlementWebhookGrantsExactlyOnce(t *testing.T) {
	f := newWebhookFixture(t, billing.StatusCheckoutCreated)

	for i := range 5 {
		require.NoError(t, f.deliver(t, settledEvent(fmt.Sprintf("evt_%d", i), f.purchaseID)))
	}

	assert.Equal(t, billing.StatusSettled, f.purchase(t).Status)
	assert.Len(t, f.grants.calls, 1)
	assert.NotNil(t, f.purchase(t).EntitlementAppliedAt)
}

func TestWebhookResolvesPurchaseByTransactionWhenCustomDataIsMissing(t *testing.T) {
	f := newWebhookFixture(t, billing.StatusSettled)
	checkoutID := "txn_known"
	p := f.repo.purchases[f.purchaseID]
	p.ProviderCheckoutID = &checkoutID
	f.repo.purchases[f.purchaseID] = p

	// Paddle's adjustments carry no custom data, only the transaction.
	require.NoError(t, f.deliver(t, billing.ProviderEvent{
		ExternalEventID: "evt_adj", Type: billing.EventPaymentRefunded,
		ExternalTransactionID: checkoutID,
		Adjustment:            billing.Money{AmountMinor: 3900, Currency: "USD"},
	}))

	assert.Equal(t, billing.StatusRefunded, f.purchase(t).Status)
}

func TestWebhookForAnUnknownTransactionIsRetried(t *testing.T) {
	f := newWebhookFixture(t, billing.StatusSettled)

	err := f.deliver(t, billing.ProviderEvent{
		ExternalEventID: "evt_orphan", Type: billing.EventPaymentRefunded,
		ExternalTransactionID: "txn_not_ours",
	})

	require.Error(t, err, "an unmatched notification must not be acknowledged as processed")
}

// --- Refund policy ---

func TestRefundBeforeConsumeRevokesTheLicense(t *testing.T) {
	f := newWebhookFixture(t, billing.StatusSettled)

	require.NoError(t, f.deliver(t, billing.ProviderEvent{
		ExternalEventID: "evt_refund", Type: billing.EventPaymentRefunded, PurchaseID: f.purchaseID,
		Adjustment: billing.Money{AmountMinor: 3900, Currency: "USD"},
	}))

	assert.Equal(t, billing.StatusRefunded, f.purchase(t).Status)
	assert.Equal(t, []uuid.UUID{f.purchaseID}, f.licenses.revoked)
	assert.Empty(t, f.licenses.eventStatuses, "nothing was activated, so no event needs review")
}

func TestRefundAfterConsumeKeepsTheGrantAndFlagsTheEvent(t *testing.T) {
	f := newWebhookFixture(t, billing.StatusSettled)
	f.licenses.consumed[f.purchaseID] = true

	require.NoError(t, f.deliver(t, billing.ProviderEvent{
		ExternalEventID: "evt_refund", Type: billing.EventPaymentRefunded, PurchaseID: f.purchaseID,
		Adjustment: billing.Money{AmountMinor: 3900, Currency: "USD"},
	}))

	p := f.purchase(t)
	assert.Equal(t, billing.StatusRefunded, p.Status)
	assert.Equal(t, int64(3900), p.RefundedAmountMinor)
	assert.Empty(t, f.licenses.revoked, "a consumed license is never withdrawn automatically")
	assert.Equal(t, billing.EventBillingRefunded, f.licenses.eventStatuses[f.eventID])
}

func TestPartialRefundDoesNotFlagTheEvent(t *testing.T) {
	f := newWebhookFixture(t, billing.StatusSettled)
	f.licenses.consumed[f.purchaseID] = true

	require.NoError(t, f.deliver(t, billing.ProviderEvent{
		ExternalEventID: "evt_partial", Type: billing.EventPaymentRefunded, PurchaseID: f.purchaseID,
		Adjustment: billing.Money{AmountMinor: 1000, Currency: "USD"},
	}))

	p := f.purchase(t)
	assert.Equal(t, billing.StatusPartiallyRefunded, p.Status)
	assert.Equal(t, int64(1000), p.RefundedAmountMinor)
	assert.Empty(t, f.licenses.eventStatuses)
}

func TestPartialRefundsAccumulateIntoAFullRefund(t *testing.T) {
	f := newWebhookFixture(t, billing.StatusSettled)
	f.licenses.consumed[f.purchaseID] = true

	for i, amount := range []int64{2000, 1900} {
		require.NoError(t, f.deliver(t, billing.ProviderEvent{
			ExternalEventID: fmt.Sprintf("evt_partial_%d", i), Type: billing.EventPaymentRefunded,
			PurchaseID: f.purchaseID,
			Adjustment: billing.Money{AmountMinor: amount, Currency: "USD"},
		}))
	}

	p := f.purchase(t)
	assert.Equal(t, billing.StatusRefunded, p.Status)
	assert.Equal(t, int64(3900), p.RefundedAmountMinor)
}

func TestRefundBeforeSettlementCancelsTheOpenCheckout(t *testing.T) {
	f := newWebhookFixture(t, billing.StatusCheckoutCreated)

	require.NoError(t, f.deliver(t, billing.ProviderEvent{
		ExternalEventID: "evt_early_refund", Type: billing.EventPaymentRefunded, PurchaseID: f.purchaseID,
		Adjustment: billing.Money{AmountMinor: 3900, Currency: "USD"},
	}))

	assert.Equal(t, billing.StatusCanceled, f.purchase(t).Status)
	assert.Empty(t, f.grants.calls)
}

// --- Dispute policy ---

func TestDisputeAfterConsumeFlagsForReviewAndKeepsEverything(t *testing.T) {
	f := newWebhookFixture(t, billing.StatusSettled)
	f.licenses.consumed[f.purchaseID] = true

	require.NoError(t, f.deliver(t, billing.ProviderEvent{
		ExternalEventID: "evt_chargeback", Type: billing.EventPaymentDisputed, PurchaseID: f.purchaseID,
		Adjustment: billing.Money{AmountMinor: 3900, Currency: "USD"},
	}))

	assert.Equal(t, billing.StatusDisputed, f.purchase(t).Status)
	assert.Empty(t, f.licenses.revoked)
	assert.Equal(t, billing.EventBillingPaymentReview, f.licenses.eventStatuses[f.eventID])
}

func TestDisputeReversalRestoresTheSettledPurchase(t *testing.T) {
	f := newWebhookFixture(t, billing.StatusSettled)
	f.licenses.consumed[f.purchaseID] = true
	require.NoError(t, f.deliver(t, billing.ProviderEvent{
		ExternalEventID: "evt_chargeback", Type: billing.EventPaymentDisputed, PurchaseID: f.purchaseID,
		Adjustment: billing.Money{AmountMinor: 3900, Currency: "USD"},
	}))

	require.NoError(t, f.deliver(t, billing.ProviderEvent{
		ExternalEventID: "evt_chargeback_reverse", Type: billing.EventPaymentDisputeReversed,
		PurchaseID: f.purchaseID,
		Adjustment: billing.Money{AmountMinor: 3900, Currency: "USD"},
	}))

	p := f.purchase(t)
	assert.Equal(t, billing.StatusSettled, p.Status)
	assert.Zero(t, p.RefundedAmountMinor)
	assert.Equal(t, billing.EventBillingOK, f.licenses.eventStatuses[f.eventID])
}

// --- Reconciliation ---

// The scenario the reconciler exists for: the provider took the money and the
// purchase settled, but issuing the license failed.
func TestReconcileIsIdempotentAcrossRepeatedRuns(t *testing.T) {
	f := newTestEnv()
	ctx := context.Background()
	settledAt := time.Now().UTC()
	total := int64(3900)
	purchaseID := uuid.New()
	f.repo.purchases[purchaseID] = billing.Purchase{
		ID: purchaseID, EventID: uuid.New(), HostID: uuid.New(),
		PlanCode: catalog.PlanExperience, PlanVersionID: uuid.New(),
		Status: billing.StatusSettled, SettledTotalMinor: &total, SettledAt: &settledAt,
	}

	applied := 0
	for range 10 {
		n, errs := f.svc.ReconcileUnapplied(ctx, 50)
		require.Empty(t, errs)
		applied += n
	}

	assert.Equal(t, 1, applied, "exactly one run does the work")
	assert.Len(t, f.grants.calls, 1, "exactly one license and grant")
	p, err := f.repo.GetPurchase(ctx, purchaseID)
	require.NoError(t, err)
	assert.NotNil(t, p.EntitlementAppliedAt)
}

// A grant that keeps failing must stay queued rather than be marked applied.
func TestReconcileLeavesAFailingGrantUnapplied(t *testing.T) {
	f := newTestEnv()
	ctx := context.Background()
	f.grants.err = assert.AnError
	settledAt := time.Now().UTC()
	purchaseID := uuid.New()
	f.repo.purchases[purchaseID] = billing.Purchase{
		ID: purchaseID, EventID: uuid.New(), HostID: uuid.New(),
		PlanCode: catalog.PlanExperience, Status: billing.StatusSettled, SettledAt: &settledAt,
	}

	applied, errs := f.svc.ReconcileUnapplied(ctx, 50)

	assert.Zero(t, applied)
	assert.Len(t, errs, 1)
	p, err := f.repo.GetPurchase(ctx, purchaseID)
	require.NoError(t, err)
	assert.Nil(t, p.EntitlementAppliedAt)
}

// A duplicate source reference means another attempt already granted it; the
// purchase is finished, not failed.
func TestReconcileTreatsAnExistingGrantAsApplied(t *testing.T) {
	f := newTestEnv()
	ctx := context.Background()
	f.grants.err = entitlement.ErrDuplicateSourceReference
	settledAt := time.Now().UTC()
	purchaseID := uuid.New()
	f.repo.purchases[purchaseID] = billing.Purchase{
		ID: purchaseID, EventID: uuid.New(), HostID: uuid.New(),
		PlanCode: catalog.PlanExperience, Status: billing.StatusSettled, SettledAt: &settledAt,
	}

	applied, errs := f.svc.ReconcileUnapplied(ctx, 50)

	assert.Empty(t, errs)
	assert.Equal(t, 1, applied)
}

// Paddle announces one refund as adjustment.created and again as
// adjustment.updated, and this account subscribes to both. Counting the money
// twice would turn a $10 goodwill refund into a full one.
func TestOneRefundAnnouncedTwiceIsCountedOnce(t *testing.T) {
	f := newWebhookFixture(t, billing.StatusSettled)
	f.licenses.consumed[f.purchaseID] = true

	for _, eventID := range []string{"evt_adj_created", "evt_adj_updated"} {
		require.NoError(t, f.deliver(t, billing.ProviderEvent{
			ExternalEventID: eventID,
			DedupeKey:       "adjustment:adj_same",
			Type:            billing.EventPaymentRefunded,
			PurchaseID:      f.purchaseID,
			Adjustment:      billing.Money{AmountMinor: 1000, Currency: "USD"},
		}))
	}

	p := f.purchase(t)
	assert.Equal(t, int64(1000), p.RefundedAmountMinor)
	assert.Equal(t, billing.StatusPartiallyRefunded, p.Status)
}

// Two genuinely different partial refunds must still accumulate.
func TestTwoDistinctAdjustmentsBothCount(t *testing.T) {
	f := newWebhookFixture(t, billing.StatusSettled)
	f.licenses.consumed[f.purchaseID] = true

	for i, key := range []string{"adjustment:adj_one", "adjustment:adj_two"} {
		require.NoError(t, f.deliver(t, billing.ProviderEvent{
			ExternalEventID: fmt.Sprintf("evt_%d", i),
			DedupeKey:       key,
			Type:            billing.EventPaymentRefunded,
			PurchaseID:      f.purchaseID,
			Adjustment:      billing.Money{AmountMinor: 1000, Currency: "USD"},
		}))
	}

	assert.Equal(t, int64(2000), f.purchase(t).RefundedAmountMinor)
}
