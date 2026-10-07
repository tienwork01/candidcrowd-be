package billing

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ProviderEventType is a canonical event type that billing understands.
// Provider adapters map vendor-specific events onto these.
type ProviderEventType string

const (
	EventPaymentSettled         ProviderEventType = "payment.settled"
	EventPaymentFailed          ProviderEventType = "payment.failed"
	EventPaymentCanceled        ProviderEventType = "payment.canceled"
	EventPaymentRefunded        ProviderEventType = "payment.refunded"
	EventPaymentDisputed        ProviderEventType = "payment.disputed"
	EventPaymentDisputeReversed ProviderEventType = "payment.dispute_reversed"
	// EventIgnored is a notification the adapter verified and understood but
	// that billing has no state for. It is recorded and acknowledged.
	EventIgnored ProviderEventType = "payment.ignored"
)

// CheckoutGateway creates a checkout session at the configured payment
// provider. The implementation lives in internal/platform/<provider>.
type CheckoutGateway interface {
	CreateCheckout(ctx context.Context, in CreateCheckoutInput) (CheckoutTarget, error)
}

// CreateCheckoutInput carries what the provider needs to open a checkout. The
// amount is the verified quote total: an adapter executes it, it never prices.
//
// Exactly one of ProviderPriceID and ProviderProductID is used. A price ID is
// sent when the amount matches a price that already exists in the provider's
// catalog; otherwise the adapter sends a non-catalog price under the product.
type CreateCheckoutInput struct {
	PurchaseID        uuid.UUID
	EventID           uuid.UUID
	PlanCode          string
	Description       string
	AmountMinor       int64
	Currency          string
	ProviderPriceID   string
	ProviderProductID string
	CustomerEmail     string
	SuccessURL        string
	CancelURL         string
}

// CheckoutTarget is what the provider returned.
type CheckoutTarget struct {
	ExternalCheckoutID string
	// ProviderPriceID is the price the provider actually charges. For a
	// non-catalog price the provider assigns it, so it is only known here; it is
	// snapshotted on the purchase and later matched against the webhook.
	ProviderPriceID string
	// URL is retained for redirect-capable adapters (for example the fake
	// portability drill). Paddle's adapter deliberately leaves it empty: the
	// browser opens its transaction ID through Paddle.js.
	URL string
}

// WebhookDecoder verifies a raw webhook and maps it to a canonical event.
type WebhookDecoder interface {
	DecodeWebhook(rawBody []byte, signature string) (ProviderEvent, error)
}

// ProviderEvent is the canonical representation of a payment provider
// notification. All provider-specific mapping happens before this struct is
// built; billing service code never sees vendor types.
//
// PurchaseID may be uuid.Nil: a provider does not necessarily echo a
// transaction's custom data on later notifications about it (Paddle's
// adjustments do not), so the service falls back to resolving the purchase
// through ExternalTransactionID.
type ProviderEvent struct {
	ExternalEventID string
	// DedupeKey identifies the thing that happened, when that is narrower than
	// the notification carrying it. A provider may announce one refund twice —
	// created, then updated — and only the refund may be counted once. Empty
	// means the notification is its own identity.
	DedupeKey             string
	Type                  ProviderEventType
	ExternalTransactionID string
	PurchaseID            uuid.UUID
	PriceID               string
	Subtotal              Money
	Tax                   Money
	Total                 Money
	// Adjustment is the amount returned to the customer on a refund or
	// chargeback notification, as a positive number.
	Adjustment Money
	OccurredAt time.Time
}

// EventAuthorizer checks event ownership for billing operations.
type EventAuthorizer interface {
	OwnerOf(ctx context.Context, eventID, hostID uuid.UUID) error
	HostEmail(ctx context.Context, hostID uuid.UUID) (string, error)
}

// CatalogReader reads what billing needs from the plan catalog.
type CatalogReader interface {
	ActiveVersion(ctx context.Context, code string) (CatalogVersion, error)
	CurrentPrice(ctx context.Context, planVersionID uuid.UUID, currency string) (CatalogPrice, error)
}

// CatalogVersion is what billing sees of a plan version.
type CatalogVersion struct {
	ID   uuid.UUID
	Code string
}

// CatalogPrice is what billing sees of a catalog display price.
//
// ReturningAmountMinor is the loyalty price of the same tier, when the catalog
// publishes one. It lives beside the list price so a new plan ships both as
// data rather than being added to a table inside the pricing engine.
type CatalogPrice struct {
	AmountMinor          int64
	ReturningAmountMinor *int64
	Currency             string
}

// GrantActivator is the existing entitlement interface, aliased here for
// documentation. Billing calls this after settlement, never writes grants
// directly. The concrete type is entitlement.GrantActivator.
type GrantActivator interface {
	ActivateForPurchase(ctx context.Context, in GrantInput) error
}

// GrantInput carries what the entitlement system needs.
type GrantInput struct {
	EventID               uuid.UUID
	AccountID             uuid.UUID
	PlanVersionID         uuid.UUID
	PlanCode              string
	PurchaseID            uuid.UUID
	PaymentProvider       string
	ProviderTransactionID string
	PurchaseAmountMinor   int64
	PurchaseCurrency      string
	SettledAt             time.Time
}

// LicenseRevoker withdraws the entitlement side of a payment that was reversed.
// It is deliberately narrow: an unconsumed license can be withdrawn, a consumed
// one cannot. Nothing here deletes an event or its media.
type LicenseRevoker interface {
	// RevokeUnconsumedLicense withdraws the license issued for a purchase when
	// it has not been consumed by an event yet. It reports whether a license was
	// revoked, and is a no-op for a consumed or missing license.
	RevokeUnconsumedLicense(ctx context.Context, purchaseID uuid.UUID) (bool, error)
	// SetEventBillingStatus flags an event for human review after a reversal.
	SetEventBillingStatus(ctx context.Context, eventID uuid.UUID, status EventBillingStatus) error
}

// EventBillingStatus is what a reversal does to an event: it marks it, it never
// downgrades, closes or deletes it.
type EventBillingStatus string

const (
	EventBillingOK            EventBillingStatus = "ok"
	EventBillingPaymentReview EventBillingStatus = "payment_review"
	EventBillingRefunded      EventBillingStatus = "refunded"
)
