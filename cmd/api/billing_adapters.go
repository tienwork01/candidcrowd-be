package main

import (
	"context"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/billing"
	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type entitlementGrantAdapter struct {
	licenses *entitlement.PurchaseLicenseService
}

func (a *entitlementGrantAdapter) ActivateForPurchase(ctx context.Context, in billing.GrantInput) error {
	err := a.licenses.ActivatePurchase(ctx, entitlement.PurchaseLicenseInput{
		PurchaseID: in.PurchaseID, AccountID: in.AccountID, EventID: in.EventID,
		PlanVersionID: in.PlanVersionID, PaymentProvider: in.PaymentProvider,
		ProviderTransactionID: in.ProviderTransactionID, PurchaseAmountMinor: in.PurchaseAmountMinor,
		PurchaseCurrency: in.PurchaseCurrency, ActivatedAt: in.SettledAt,
	})
	return err
}

type catalogReaderAdapter struct {
	catalog *catalog.Service
	db      *gorm.DB
}

func (a *catalogReaderAdapter) ActiveVersion(ctx context.Context, code string) (billing.CatalogVersion, error) {
	v, err := a.catalog.ActiveVersion(ctx, catalog.PlanCode(code))
	if err != nil {
		return billing.CatalogVersion{}, err
	}
	return billing.CatalogVersion{ID: v.ID, Code: string(v.Code)}, nil
}

func (a *catalogReaderAdapter) CurrentPrice(ctx context.Context, planVersionID uuid.UUID, currency string) (billing.CatalogPrice, error) {
	var p catalog.Price
	now := time.Now().UTC()
	err := a.db.WithContext(ctx).
		Where("plan_version_id = ? AND currency = ? AND valid_from <= ? AND (valid_until IS NULL OR valid_until > ?)",
			planVersionID, currency, now, now).
		First(&p).Error
	if err != nil {
		return billing.CatalogPrice{}, err
	}
	return billing.CatalogPrice{
		AmountMinor:          p.AmountMinor,
		ReturningAmountMinor: p.ReturningAmountMinor,
		Currency:             p.Currency,
	}, nil
}

type eventOwnershipAdapter struct {
	events *event.Service
	db     *gorm.DB
}

func (a *eventOwnershipAdapter) OwnerOf(ctx context.Context, eventID, hostID uuid.UUID) error {
	_, err := a.events.GetOwned(ctx, eventID, hostID)
	return err
}

func (a *eventOwnershipAdapter) HostEmail(ctx context.Context, hostID uuid.UUID) (string, error) {
	var email string
	err := a.db.WithContext(ctx).Raw("SELECT email FROM users WHERE id = ?", hostID).Scan(&email).Error
	if err != nil {
		return "", err
	}
	return email, nil
}

// billingReversalAdapter is the entitlement side of a refund or chargeback.
// Both operations are deliberately conservative: an unconsumed license is
// withdrawn, and a consumed one only flags its event for review. Nothing here
// downgrades a plan, closes uploads or deletes media.
type billingReversalAdapter struct {
	licenses *entitlement.PurchaseLicenseService
	db       *gorm.DB
}

func (a *billingReversalAdapter) RevokeUnconsumedLicense(ctx context.Context, purchaseID uuid.UUID) (bool, error) {
	return a.licenses.RevokeUnconsumed(ctx, purchaseID)
}

func (a *billingReversalAdapter) SetEventBillingStatus(ctx context.Context, eventID uuid.UUID, status billing.EventBillingStatus) error {
	return a.db.WithContext(ctx).
		Exec(`UPDATE events SET billing_status = ? WHERE id = ?`, string(status), eventID).Error
}
