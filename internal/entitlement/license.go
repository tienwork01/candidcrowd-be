package entitlement

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrLicenseUnavailable = errors.New("entitlement: event license is not available")
	ErrLicenseOwnership   = errors.New("entitlement: license and event must belong to the same account")
)

type LicenseStatus string

const (
	LicenseAvailable LicenseStatus = "available"
	LicenseReserved  LicenseStatus = "reserved"
	LicenseConsumed  LicenseStatus = "consumed"
	LicenseRevoked   LicenseStatus = "revoked"
	LicenseExpired   LicenseStatus = "expired"
)

type LicenseSource string

const (
	LicensePurchase  LicenseSource = "purchase"
	LicensePromotion LicenseSource = "promotion"
	LicenseReferral  LicenseSource = "referral"
	LicenseAdmin     LicenseSource = "admin"
	LicenseBusiness  LicenseSource = "business"
)

// EventLicense is an internal right to activate one event at one plan version.
// It is intentionally never presented as consumer currency or a wallet balance.
type EventLicense struct {
	ID                    uuid.UUID `gorm:"type:uuid;primaryKey"`
	AccountID             uuid.UUID `gorm:"type:uuid;column:account_id"`
	PlanVersionID         uuid.UUID `gorm:"type:uuid;column:plan_version_id"`
	Status                LicenseStatus
	Source                LicenseSource
	PurchaseID            *uuid.UUID `gorm:"type:uuid;column:purchase_id"`
	PaymentProvider       *string
	ProviderTransactionID *string
	PurchaseAmountMinor   *int64
	PurchaseCurrency      *string
	ReservedEventID       *uuid.UUID `gorm:"type:uuid"`
	ConsumedEventID       *uuid.UUID `gorm:"type:uuid"`
	CreatedAt             time.Time
	ReservedAt            *time.Time
	ConsumedAt            *time.Time
	RevokedAt             *time.Time
	ExpiresAt             *time.Time
}

func (EventLicense) TableName() string { return "event_licenses" }

type PurchaseLicenseInput struct {
	PurchaseID            uuid.UUID
	AccountID             uuid.UUID
	EventID               uuid.UUID
	PlanVersionID         uuid.UUID
	PaymentProvider       string
	ProviderTransactionID string
	PurchaseAmountMinor   int64
	PurchaseCurrency      string
	ActivatedAt           time.Time
}

// PurchaseLicenseService atomically issues, reserves, consumes and activates
// the license created by one settled purchase.
type PurchaseLicenseService struct {
	db       *gorm.DB
	now      func() time.Time
	notifier Notifier
}

func NewPurchaseLicenseService(db *gorm.DB) *PurchaseLicenseService {
	return &PurchaseLicenseService{db: db, now: time.Now}
}

func (s *PurchaseLicenseService) UseNotifier(n Notifier) *PurchaseLicenseService {
	s.notifier = n
	return s
}

// RevokeUnconsumed withdraws the license issued for a purchase when no event
// has consumed it yet. It reports whether anything was revoked.
//
// A consumed license is never touched: the event is live on the plan it bought,
// and taking the grant away would remove capabilities from a running event.
// Reversing that case is a human decision, not an automatic one.
func (s *PurchaseLicenseService) RevokeUnconsumed(ctx context.Context, purchaseID uuid.UUID) (bool, error) {
	res := s.db.WithContext(ctx).Model(&EventLicense{}).
		Where("purchase_id = ? AND status IN (?, ?)", purchaseID, LicenseAvailable, LicenseReserved).
		Updates(map[string]any{"status": LicenseRevoked, "revoked_at": s.now().UTC()})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func (s *PurchaseLicenseService) ActivatePurchase(ctx context.Context, in PurchaseLicenseInput) error {
	at := in.ActivatedAt.UTC()
	if at.IsZero() {
		at = s.now().UTC()
	}
	var activatedPlan catalog.PlanCode
	var grantID uuid.UUID
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var version catalog.Version
		if err := tx.Where("id = ?", in.PlanVersionID).First(&version).Error; err != nil {
			return fmt.Errorf("load purchased plan version: %w", err)
		}
		purchaseID := in.PurchaseID
		provider, txn, currency := in.PaymentProvider, in.ProviderTransactionID, in.PurchaseCurrency
		amount := in.PurchaseAmountMinor
		candidate := EventLicense{ID: uuid.New(), AccountID: in.AccountID, PlanVersionID: in.PlanVersionID,
			Status: LicenseAvailable, Source: LicensePurchase, PurchaseID: &purchaseID,
			PaymentProvider: &provider, ProviderTransactionID: &txn,
			PurchaseAmountMinor: &amount, PurchaseCurrency: &currency, CreatedAt: at}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "purchase_id"}}, DoNothing: true}).Create(&candidate).Error; err != nil {
			return err
		}

		var license EventLicense
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("purchase_id = ?", in.PurchaseID).First(&license).Error; err != nil {
			return err
		}
		if license.AccountID != in.AccountID || license.PlanVersionID != in.PlanVersionID {
			return ErrLicenseOwnership
		}
		if license.Status == LicenseConsumed {
			if license.ConsumedEventID != nil && *license.ConsumedEventID == in.EventID {
				return nil
			}
			return ErrLicenseUnavailable
		}
		if license.Status != LicenseAvailable && license.Status != LicenseReserved {
			return ErrLicenseUnavailable
		}

		var facts []struct {
			HostID    uuid.UUID
			EventDate *time.Time
		}
		if err := tx.Raw(`SELECT host_id, event_date FROM events WHERE id = ? AND status <> 'deleted' FOR UPDATE`, in.EventID).Scan(&facts).Error; err != nil {
			return err
		}
		if len(facts) == 0 {
			return ErrEventNotFound
		}
		if facts[0].HostID != in.AccountID {
			return ErrLicenseOwnership
		}
		current, err := activeGrant(tx, in.EventID)
		if err != nil {
			return err
		}
		var from *catalog.PlanCode
		if current != nil {
			from = &current.PlanCode
		}
		if err := CheckTransition(from, version.Code); err != nil {
			return err
		}

		if err := tx.Model(&EventLicense{}).Where("id = ?", license.ID).Updates(map[string]any{
			"status": LicenseReserved, "reserved_event_id": in.EventID, "reserved_at": at,
		}).Error; err != nil {
			return err
		}
		upload, retention := Expiry(facts[0].EventDate, at, version.Entitlements.Limits.RetentionDays)
		if current != nil {
			upload = later(upload, current.UploadExpiresAt)
			retention = later(retention, current.RetentionExpiresAt)
		}
		ref := "billing-purchase:" + in.PurchaseID.String()
		grant := Grant{ID: uuid.New(), EventID: in.EventID, PlanVersionID: version.ID, EventLicenseID: &license.ID,
			Status: StatusActive, Source: SourcePurchase, SourceReference: &ref,
			EntitlementSnapshot: version.Entitlements, ActivatedAt: at, UploadExpiresAt: upload,
			RetentionExpiresAt: retention, CreatedAt: at}
		if err := writeGrant(tx, grant, current, version.Code, "paid event package purchase"); err != nil {
			return err
		}
		if err := tx.Model(&EventLicense{}).Where("id = ? AND status = ?", license.ID, LicenseReserved).Updates(map[string]any{
			"status": LicenseConsumed, "consumed_event_id": in.EventID, "consumed_at": at,
		}).Error; err != nil {
			return err
		}
		if err := tx.Exec(`UPDATE events SET trial_ended_at = COALESCE(trial_ended_at, ?) WHERE id = ?`, at, in.EventID).Error; err != nil {
			return err
		}
		activatedPlan, grantID = version.Code, grant.ID
		return nil
	})
	if err == nil && grantID != uuid.Nil && s.notifier != nil {
		s.notifier.PlanChanged(ctx, PlanChange{EventID: in.EventID, PlanCode: activatedPlan, GrantID: grantID})
	}
	return err
}
