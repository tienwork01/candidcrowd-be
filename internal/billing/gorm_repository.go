package billing

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type gormRepository struct {
	db *gorm.DB
}

// NewGormRepository creates a GORM-backed billing repository.
func NewGormRepository(db *gorm.DB) Repository {
	return &gormRepository{db: db}
}

func (r *gormRepository) CreatePurchase(ctx context.Context, p Purchase) (Purchase, error) {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now

	err := r.db.WithContext(ctx).Create(&p).Error
	if err != nil {
		return Purchase{}, err
	}
	return p, nil
}

func (r *gormRepository) IdempotentPurchase(ctx context.Context, hostID uuid.UUID, idempotencyKey string) (*Purchase, error) {
	var p Purchase
	err := r.db.WithContext(ctx).
		Where("host_id = ? AND idempotency_key = ?", hostID, idempotencyKey).
		First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *gormRepository) GetPurchase(ctx context.Context, id uuid.UUID) (Purchase, error) {
	var p Purchase
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Purchase{}, ErrPurchaseNotFound
	}
	return p, err
}

func (r *gormRepository) UpdateCheckout(ctx context.Context, id uuid.UUID, checkoutID, providerPriceID, checkoutURL string) error {
	now := time.Now().UTC()
	res := r.db.WithContext(ctx).Model(&Purchase{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"provider_checkout_id": checkoutID,
			"provider_price_id":    providerPriceID,
			"checkout_url":         checkoutURL,
			"status":               StatusCheckoutCreated,
			"updated_at":           now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrPurchaseNotFound
	}
	return nil
}

func (r *gormRepository) SettlePurchase(ctx context.Context, in SettleInput) error {
	now := time.Now().UTC()
	res := r.db.WithContext(ctx).Model(&Purchase{}).
		Where("id = ? AND status <> ?", in.PurchaseID, StatusSettled).
		Updates(map[string]any{
			"status":                  StatusSettled,
			"provider_transaction_id": in.ProviderTransactionID,
			"settled_subtotal_minor":  in.SettledSubtotalMinor,
			"settled_tax_minor":       in.SettledTaxMinor,
			"settled_total_minor":     in.SettledTotalMinor,
			"settled_currency":        in.SettledCurrency,
			"settled_at":              now,
			"updated_at":              now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// Either not found or already settled.
		var p Purchase
		if err := r.db.WithContext(ctx).Where("id = ?", in.PurchaseID).First(&p).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrPurchaseNotFound
			}
			return err
		}
		if p.Status == StatusSettled {
			return ErrPurchaseAlreadySettled
		}
		return ErrPurchaseNotFound
	}
	return nil
}

func (r *gormRepository) UpdateStatus(ctx context.Context, id uuid.UUID, status PurchaseStatus, failureCode *string) error {
	now := time.Now().UTC()
	updates := map[string]any{
		"status":     status,
		"updated_at": now,
	}
	if failureCode != nil {
		updates["failure_code"] = *failureCode
	}
	res := r.db.WithContext(ctx).Model(&Purchase{}).
		Where("id = ?", id).
		Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrPurchaseNotFound
	}
	return nil
}

func (r *gormRepository) MarkEntitlementApplied(ctx context.Context, id uuid.UUID) error {
	now := time.Now().UTC()
	return r.db.WithContext(ctx).Model(&Purchase{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"entitlement_applied_at": now,
			"updated_at":             now,
		}).Error
}

func (r *gormRepository) OpenPurchaseForEvent(ctx context.Context, eventID uuid.UUID) (*Purchase, error) {
	var p Purchase
	err := r.db.WithContext(ctx).
		Where("event_id = ? AND status IN (?, ?)", eventID, StatusPending, StatusCheckoutCreated).
		Order("created_at DESC").
		First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *gormRepository) LatestEventPurchase(ctx context.Context, eventID uuid.UUID) (*Purchase, error) {
	var p Purchase
	err := r.db.WithContext(ctx).
		Where("event_id = ? AND status IN (?, ?, ?, ?)", eventID,
			StatusSettled, StatusPartiallyRefunded, StatusRefunded, StatusDisputed).
		Order("settled_at DESC NULLS LAST, created_at DESC").
		First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *gormRepository) HasEligibleCommercialPurchase(ctx context.Context, hostID, excludeEventID uuid.UUID) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&Purchase{}).
		Where("host_id = ? AND event_id <> ? AND status IN (?, ?) AND settled_total_minor > 0",
			hostID, excludeEventID, StatusSettled, StatusPartiallyRefunded).
		Limit(1).Count(&count).Error
	return count > 0, err
}

func (r *gormRepository) RecordReversal(ctx context.Context, in ReversalInput) error {
	now := time.Now().UTC()
	res := r.db.WithContext(ctx).Model(&Purchase{}).
		Where("id = ?", in.PurchaseID).
		Updates(map[string]any{
			"status":                in.Status,
			"refunded_amount_minor": in.RefundedAmountMinor,
			"updated_at":            now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrPurchaseNotFound
	}
	return nil
}

func (r *gormRepository) UnappliedSettled(ctx context.Context, limit int) ([]Purchase, error) {
	if limit <= 0 {
		limit = 50
	}
	var purchases []Purchase
	err := r.db.WithContext(ctx).
		Where("status = ? AND entitlement_applied_at IS NULL", StatusSettled).
		Order("settled_at ASC").
		Limit(limit).
		Find(&purchases).Error
	return purchases, err
}

func (r *gormRepository) InsertProviderEvent(ctx context.Context, rec ProviderEventRecord) (duplicate bool, err error) {
	if rec.ID == uuid.Nil {
		rec.ID = uuid.New()
	}
	if rec.ReceivedAt.IsZero() {
		rec.ReceivedAt = time.Now().UTC()
	}

	// Try inserting with OnConflict do nothing or check duplicated key.
	res := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "provider"}, {Name: "external_event_id"}},
		DoNothing: true,
	}).Create(&rec)

	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		// A delivery that failed processing must be reclaimable on the next
		// verified provider retry. Processed events remain immutable duplicates.
		retry := r.db.WithContext(ctx).Model(&ProviderEventRecord{}).
			Where("id = ? AND status = ?", rec.ID, "failed").
			Updates(map[string]any{
				"status":     "received",
				"last_error": nil,
			})
		if retry.Error != nil {
			return false, retry.Error
		}
		if retry.RowsAffected == 1 {
			return false, nil
		}
		return true, nil
	}
	return false, nil
}

func (r *gormRepository) MarkProviderEventProcessed(ctx context.Context, id uuid.UUID) error {
	now := time.Now().UTC()
	return r.db.WithContext(ctx).Model(&ProviderEventRecord{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":       "processed",
			"processed_at": now,
		}).Error
}

func (r *gormRepository) MarkProviderEventFailed(ctx context.Context, id uuid.UUID, errMsg string) error {
	return r.db.WithContext(ctx).Model(&ProviderEventRecord{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":        "failed",
			"last_error":    errMsg,
			"attempt_count": gorm.Expr("attempt_count + 1"),
		}).Error
}

func (r *gormRepository) FindProviderPrice(ctx context.Context, provider Provider, planVersionID uuid.UUID, currency string, amountMinor int64) (ProviderPrice, error) {
	var price ProviderPrice
	err := r.db.WithContext(ctx).
		Where("provider = ? AND plan_version_id = ? AND currency = ? AND amount_minor = ? AND active = true", provider, planVersionID, currency, amountMinor).
		First(&price).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ProviderPrice{}, ErrProviderPriceNotFound
	}
	return price, err
}

func (r *gormRepository) FindProviderProduct(ctx context.Context, provider Provider, planCode string) (ProviderProduct, error) {
	var product ProviderProduct
	err := r.db.WithContext(ctx).
		Where("provider = ? AND plan_code = ? AND active = true", provider, planCode).
		First(&product).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ProviderProduct{}, ErrProviderProductNotFound
	}
	return product, err
}

// PurchaseByProviderTransaction matches either column because the provider's
// transaction ID is known at checkout and again at settlement, and a refund can
// arrive before this process ever recorded the settlement.
func (r *gormRepository) PurchaseByProviderTransaction(ctx context.Context, provider Provider, txnID string) (*Purchase, error) {
	var p Purchase
	err := r.db.WithContext(ctx).
		Where("provider = ? AND (provider_transaction_id = ? OR provider_checkout_id = ?)", provider, txnID, txnID).
		Order("created_at DESC").
		First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}
