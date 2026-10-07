//go:build integration

package entitlement

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/database"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestIntegrationPurchasedLicenseConcurrentConsumeExactlyOnce(t *testing.T) {
	db := licenseTestDB(t)
	ctx := context.Background()
	hostID := insertLicenseTestUser(t, db)
	plans := catalog.NewService(catalog.NewGormRepository(db))
	events := event.NewService(event.NewGormRepository(db, NewProvisioner(plans)), 5<<30)
	first, err := events.Create(ctx, hostID, event.CreateInput{Name: "first"})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`UPDATE events SET trial_ended_at = now() WHERE id = ?`, first.ID).Error)
	second, err := events.Create(ctx, hostID, event.CreateInput{Name: "second"})
	require.NoError(t, err)

	version, err := plans.ActiveVersion(ctx, catalog.PlanSignature)
	require.NoError(t, err)
	purchaseID := insertLicenseTestPurchase(t, db, hostID, first.ID, version.ID, 0)
	svc := NewPurchaseLicenseService(db)
	base := PurchaseLicenseInput{PurchaseID: purchaseID, AccountID: hostID, PlanVersionID: version.ID,
		PaymentProvider: "test", ProviderTransactionID: "txn-" + purchaseID.String(),
		PurchaseAmountMinor: 0, PurchaseCurrency: "USD", ActivatedAt: time.Now().UTC()}

	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, eventID := range []uuid.UUID{first.ID, second.ID} {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			in := base
			in.EventID = id
			results <- svc.ActivatePurchase(ctx, in)
		}(eventID)
	}
	wg.Wait()
	close(results)
	success, rejected := 0, 0
	for activateErr := range results {
		if activateErr == nil {
			success++
		} else if errors.Is(activateErr, ErrLicenseUnavailable) {
			rejected++
		} else {
			t.Fatalf("unexpected activation error: %v", activateErr)
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, rejected)

	var consumed, signatureGrants int64
	require.NoError(t, db.Raw(`SELECT COUNT(*) FROM event_licenses WHERE purchase_id = ? AND status = 'consumed'`, purchaseID).Scan(&consumed).Error)
	require.NoError(t, db.Raw(`SELECT COUNT(*) FROM event_plan_grants g JOIN plan_versions p ON p.id=g.plan_version_id JOIN event_licenses l ON l.id=g.event_license_id WHERE l.purchase_id=? AND p.code='signature'`, purchaseID).Scan(&signatureGrants).Error)
	require.Equal(t, int64(1), consumed)
	require.Equal(t, int64(1), signatureGrants)

	cleanupLicenseTest(t, db, hostID, purchaseID, first.ID, second.ID)
}

func TestIntegrationPurchasedLicenseRejectsAnotherAccountsEvent(t *testing.T) {
	db := licenseTestDB(t)
	ctx := context.Background()
	ownerID, otherID := insertLicenseTestUser(t, db), insertLicenseTestUser(t, db)
	plans := catalog.NewService(catalog.NewGormRepository(db))
	events := event.NewService(event.NewGormRepository(db, NewProvisioner(plans)), 5<<30)
	evt, err := events.Create(ctx, otherID, event.CreateInput{Name: "other account"})
	require.NoError(t, err)
	version, err := plans.ActiveVersion(ctx, catalog.PlanExperience)
	require.NoError(t, err)
	purchaseID := insertLicenseTestPurchase(t, db, ownerID, evt.ID, version.ID, 3900)
	err = NewPurchaseLicenseService(db).ActivatePurchase(ctx, PurchaseLicenseInput{
		PurchaseID: purchaseID, AccountID: ownerID, EventID: evt.ID, PlanVersionID: version.ID,
		PaymentProvider: "test", ProviderTransactionID: "txn-" + purchaseID.String(),
		PurchaseAmountMinor: 3900, PurchaseCurrency: "USD", ActivatedAt: time.Now().UTC(),
	})
	require.ErrorIs(t, err, ErrLicenseOwnership)

	cleanupLicenseTest(t, db, ownerID, purchaseID)
	cleanupLicenseTest(t, db, otherID, uuid.Nil, evt.ID)
}

func licenseTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	db, sqlDB, err := database.Open(url, 10, 2, time.Minute)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func insertLicenseTestUser(t *testing.T, db *gorm.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO users (id, better_auth_user_id, email) VALUES (?, ?, ?)`, id, "license-"+id.String(), id.String()+"@example.test").Error)
	return id
}

func insertLicenseTestPurchase(t *testing.T, db *gorm.DB, hostID, eventID, versionID uuid.UUID, amount int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO billing_purchases
		(id,event_id,host_id,plan_version_id,plan_code,quoted_amount_minor,base_amount_minor,discount_amount_minor,
		 quoted_currency,status,provider,provider_price_id,idempotency_key,pricing_reason,pricing_version)
		VALUES (?,?,?,?, (SELECT code FROM plan_versions WHERE id=?), ?, ?, 0, 'USD','settled','test',? ,?,'first_purchase','test-v1')`,
		id, eventID, hostID, versionID, versionID, amount, amount, "price-"+id.String(), "idem-"+id.String()).Error)
	return id
}

func cleanupLicenseTest(t *testing.T, db *gorm.DB, hostID, purchaseID uuid.UUID, eventIDs ...uuid.UUID) {
	t.Helper()
	for _, id := range eventIDs {
		if id != uuid.Nil {
			db.Exec(`DELETE FROM event_plan_grants WHERE event_id = ?`, id)
		}
	}
	if purchaseID != uuid.Nil {
		db.Exec(`DELETE FROM event_licenses WHERE purchase_id = ?`, purchaseID)
		db.Exec(`DELETE FROM billing_purchases WHERE id = ?`, purchaseID)
	}
	for _, id := range eventIDs {
		if id != uuid.Nil {
			db.Exec(`DELETE FROM events WHERE id = ?`, id)
		}
	}
	if err := db.Exec(`DELETE FROM users WHERE id = ?`, hostID).Error; err != nil {
		t.Logf("cleanup user %s: %v", hostID, err)
	}
}
