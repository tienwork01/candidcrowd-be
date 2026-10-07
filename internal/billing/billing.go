package billing

import (
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/google/uuid"
)

// Provider identifies a payment provider. Billing service code uses this
// string for storage and routing but never inspects its value.
type Provider string

const ProviderPaddle Provider = "paddle"

type PurchaseStatus string

const (
	StatusPending           PurchaseStatus = "pending"
	StatusCheckoutCreated   PurchaseStatus = "checkout_created"
	StatusSettled           PurchaseStatus = "settled"
	StatusFailed            PurchaseStatus = "failed"
	StatusCanceled          PurchaseStatus = "canceled"
	StatusPartiallyRefunded PurchaseStatus = "partially_refunded"
	StatusRefunded          PurchaseStatus = "refunded"
	StatusDisputed          PurchaseStatus = "disputed"
)

// Open reports whether a checkout is still in flight for this purchase.
func (s PurchaseStatus) Open() bool {
	return s == StatusPending || s == StatusCheckoutCreated
}

// Paid reports whether money was taken and not fully returned. A partially
// refunded purchase stays paid: the tier it bought was still sold.
func (s PurchaseStatus) Paid() bool {
	return s == StatusSettled || s == StatusPartiallyRefunded
}

// PostSettlement reports whether the status is one a payment can only reach
// after it settled.
func (s PurchaseStatus) PostSettlement() bool {
	return s == StatusSettled || s == StatusPartiallyRefunded ||
		s == StatusRefunded || s == StatusDisputed
}

// Money is a minor-unit amount in an ISO 4217 currency.
type Money struct {
	AmountMinor int64
	Currency    string
}

// Purchase is the billing aggregate: one payment attempt for one event plan.
type Purchase struct {
	ID                    uuid.UUID        `gorm:"type:uuid;primaryKey"`
	EventID               uuid.UUID        `gorm:"type:uuid;column:event_id"`
	HostID                uuid.UUID        `gorm:"type:uuid;column:host_id"`
	PlanCode              catalog.PlanCode `gorm:"column:plan_code"`
	PlanVersionID         uuid.UUID        `gorm:"type:uuid;column:plan_version_id"`
	QuotedAmountMinor     int64            `gorm:"column:quoted_amount_minor"`
	BaseAmountMinor       int64            `gorm:"column:base_amount_minor"`
	DiscountAmountMinor   int64            `gorm:"column:discount_amount_minor"`
	UpgradeCreditMinor    int64            `gorm:"column:upgrade_credit_minor"`
	PricingReason         PricingReason    `gorm:"column:pricing_reason"`
	PricingContext        PricingContext   `gorm:"column:pricing_context"`
	PricingVersion        string           `gorm:"column:pricing_version"`
	QuotedCurrency        string           `gorm:"column:quoted_currency"`
	RefundedAmountMinor   int64            `gorm:"column:refunded_amount_minor"`
	SettledSubtotalMinor  *int64           `gorm:"column:settled_subtotal_minor"`
	SettledTaxMinor       *int64           `gorm:"column:settled_tax_minor"`
	SettledTotalMinor     *int64           `gorm:"column:settled_total_minor"`
	SettledCurrency       *string          `gorm:"column:settled_currency"`
	Status                PurchaseStatus   `gorm:"column:status"`
	Provider              Provider         `gorm:"column:provider"`
	ProviderPriceID       string           `gorm:"column:provider_price_id"`
	ProviderCheckoutID    *string          `gorm:"column:provider_checkout_id"`
	ProviderTransactionID *string          `gorm:"column:provider_transaction_id"`
	IdempotencyKey        string           `gorm:"column:idempotency_key"`
	CheckoutURL           *string          `gorm:"column:checkout_url"`
	FailureCode           *string          `gorm:"column:failure_code"`
	CreatedAt             time.Time
	UpdatedAt             time.Time
	SettledAt             *time.Time `gorm:"column:settled_at"`
	EntitlementAppliedAt  *time.Time `gorm:"column:entitlement_applied_at"`
}

func (Purchase) TableName() string { return "billing_purchases" }

// ProviderProduct anchors a sellable plan to a payment provider's product. It
// carries no price: the pricing engine decides the amount and the adapter
// sends it as a non-catalog price under this product.
type ProviderProduct struct {
	ID                uuid.UUID `gorm:"type:uuid;primaryKey"`
	Provider          Provider
	PlanCode          catalog.PlanCode `gorm:"column:plan_code"`
	ProviderProductID string           `gorm:"column:provider_product_id"`
	Active            bool
	CreatedAt         time.Time
	RetiredAt         *time.Time
}

func (ProviderProduct) TableName() string { return "billing_provider_products" }

// ProviderPrice maps a catalog plan version + currency to a payment provider's
// price ID. It is optional: it pins a standard amount to a price a provider
// dashboard also shows, and is never consulted for an amount it does not
// already carry.
type ProviderPrice struct {
	ID              uuid.UUID `gorm:"type:uuid;primaryKey"`
	Provider        Provider
	PlanVersionID   uuid.UUID `gorm:"type:uuid"`
	Currency        string
	ProviderPriceID string `gorm:"column:provider_price_id"`
	AmountMinor     int64  `gorm:"column:amount_minor"`
	Active          bool
	CreatedAt       time.Time
	RetiredAt       *time.Time
}

func (ProviderPrice) TableName() string { return "billing_provider_prices" }

// ProviderEventRecord is a deduplicated webhook event.
type ProviderEventRecord struct {
	ID              uuid.UUID `gorm:"type:uuid;primaryKey"`
	Provider        Provider
	ExternalEventID string     `gorm:"column:external_event_id"`
	EventType       string     `gorm:"column:event_type"`
	PurchaseID      *uuid.UUID `gorm:"type:uuid;column:purchase_id"`
	PayloadHash     string     `gorm:"column:payload_hash"`
	Status          string
	AttemptCount    int        `gorm:"column:attempt_count"`
	LastError       *string    `gorm:"column:last_error"`
	ReceivedAt      time.Time  `gorm:"column:received_at"`
	ProcessedAt     *time.Time `gorm:"column:processed_at"`
}

func (ProviderEventRecord) TableName() string { return "billing_provider_events" }
