package billing

import (
	"context"

	"github.com/google/uuid"
)

// Repository persists purchases and deduplicates provider events.
type Repository interface {
	// CreatePurchase inserts a new purchase. Returns the purchase or an error
	// if the idempotency key is already used.
	CreatePurchase(ctx context.Context, p Purchase) (Purchase, error)

	// IdempotentPurchase returns the existing purchase for (hostID, idempotencyKey)
	// if one exists, or nil.
	IdempotentPurchase(ctx context.Context, hostID uuid.UUID, idempotencyKey string) (*Purchase, error)

	// GetPurchase loads a purchase by ID.
	GetPurchase(ctx context.Context, id uuid.UUID) (Purchase, error)

	// UpdateCheckout persists the checkout ID, the price the provider will
	// actually charge, and the URL after the provider responds.
	UpdateCheckout(ctx context.Context, id uuid.UUID, checkoutID, providerPriceID, checkoutURL string) error

	// SettlePurchase atomically moves a purchase to settled. Returns
	// ErrPurchaseAlreadySettled if it was already settled.
	SettlePurchase(ctx context.Context, in SettleInput) error

	// UpdateStatus sets the purchase status for non-settlement updates.
	UpdateStatus(ctx context.Context, id uuid.UUID, status PurchaseStatus, failureCode *string) error

	// MarkEntitlementApplied records when the grant was successfully created.
	MarkEntitlementApplied(ctx context.Context, id uuid.UUID) error

	// OpenPurchaseForEvent returns the most recent non-terminal purchase for
	// an event, or nil.
	OpenPurchaseForEvent(ctx context.Context, eventID uuid.UUID) (*Purchase, error)

	// LatestEventPurchase returns the most recent purchase for an event that
	// reached settlement, whatever happened to it afterwards, or nil. It is how
	// an upgrade reads the commercial context the event was sold in.
	LatestEventPurchase(ctx context.Context, eventID uuid.UUID) (*Purchase, error)

	// HasEligibleCommercialPurchase is true only for a positive-value purchase
	// of the host that settled, was not refunded or disputed, and belongs to an
	// event other than excludeEventID. Loyalty pricing is earned across events;
	// a purchase cannot discount itself.
	HasEligibleCommercialPurchase(ctx context.Context, hostID, excludeEventID uuid.UUID) (bool, error)

	// RecordReversal moves a settled purchase to a refunded, partially refunded
	// or disputed status and accumulates how much was returned.
	RecordReversal(ctx context.Context, in ReversalInput) error

	// UnappliedSettled lists purchases that settled but whose entitlement
	// has not been applied yet, for reconciliation.
	UnappliedSettled(ctx context.Context, limit int) ([]Purchase, error)

	// InsertProviderEvent records a webhook event for deduplication. Returns
	// true if the event was already seen (duplicate).
	InsertProviderEvent(ctx context.Context, rec ProviderEventRecord) (duplicate bool, err error)

	// MarkProviderEventProcessed updates a provider event as processed.
	MarkProviderEventProcessed(ctx context.Context, id uuid.UUID) error

	// MarkProviderEventFailed records a processing failure.
	MarkProviderEventFailed(ctx context.Context, id uuid.UUID, errMsg string) error

	// FindProviderPrice resolves an optional fixed provider price mapping for an
	// exact amount. ErrProviderPriceNotFound means "price this inline", not an
	// error for the caller.
	FindProviderPrice(ctx context.Context, provider Provider, planVersionID uuid.UUID, currency string, amountMinor int64) (ProviderPrice, error)

	// FindProviderProduct resolves the provider product a plan is sold under.
	FindProviderProduct(ctx context.Context, provider Provider, planCode string) (ProviderProduct, error)

	// PurchaseByProviderTransaction finds a purchase by its provider transaction ID.
	PurchaseByProviderTransaction(ctx context.Context, provider Provider, txnID string) (*Purchase, error)
}

// ReversalInput carries a refund or chargeback outcome. RefundedAmountMinor is
// the cumulative amount returned to the customer; the service adds each
// notification's amount to the purchase's running total, and webhook
// deduplication is what keeps one notification from being counted twice.
type ReversalInput struct {
	PurchaseID          uuid.UUID
	Status              PurchaseStatus
	RefundedAmountMinor int64
}

// SettleInput carries the settlement data.
type SettleInput struct {
	PurchaseID            uuid.UUID
	ProviderTransactionID string
	SettledSubtotalMinor  int64
	SettledTaxMinor       int64
	SettledTotalMinor     int64
	SettledCurrency       string
}
