//go:build integration

package billing_test

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/billing"
	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/database"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// These tests exercise the invariants that only the database can enforce:
// exactly one license and one grant per purchase, however many times the
// reconciler or a replayed webhook runs.

func billingTestDB(t *testing.T) *gorm.DB {
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

type integrationHarness struct {
	db        *gorm.DB
	svc       *billing.Service
	repo      billing.Repository
	licenses  *entitlement.PurchaseLicenseService
	decoder   *cannedDecoder
	hostID    uuid.UUID
	eventID   uuid.UUID
	versionID uuid.UUID
}

// cannedDecoder stands in for a provider: the signature was already verified by
// the adapter, and what matters here is the canonical event.
type cannedDecoder struct{ event billing.ProviderEvent }

func (d *cannedDecoder) DecodeWebhook([]byte, string) (billing.ProviderEvent, error) {
	return d.event, nil
}

type grantAdapter struct {
	licenses *entitlement.PurchaseLicenseService
}

func (a *grantAdapter) ActivateForPurchase(ctx context.Context, in billing.GrantInput) error {
	return a.licenses.ActivatePurchase(ctx, entitlement.PurchaseLicenseInput{
		PurchaseID: in.PurchaseID, AccountID: in.AccountID, EventID: in.EventID,
		PlanVersionID: in.PlanVersionID, PaymentProvider: in.PaymentProvider,
		ProviderTransactionID: in.ProviderTransactionID, PurchaseAmountMinor: in.PurchaseAmountMinor,
		PurchaseCurrency: in.PurchaseCurrency, ActivatedAt: in.SettledAt,
	})
}

type reversalAdapter struct {
	licenses *entitlement.PurchaseLicenseService
	db       *gorm.DB
}

func (a *reversalAdapter) RevokeUnconsumedLicense(ctx context.Context, purchaseID uuid.UUID) (bool, error) {
	return a.licenses.RevokeUnconsumed(ctx, purchaseID)
}

func (a *reversalAdapter) SetEventBillingStatus(ctx context.Context, eventID uuid.UUID, status billing.EventBillingStatus) error {
	return a.db.WithContext(ctx).Exec(`UPDATE events SET billing_status = ? WHERE id = ?`, string(status), eventID).Error
}

func newIntegrationHarness(t *testing.T, plan catalog.PlanCode) *integrationHarness {
	t.Helper()
	db := billingTestDB(t)
	ctx := context.Background()

	hostID := uuid.New()
	require.NoError(t, db.Exec(`INSERT INTO users (id, better_auth_user_id, email) VALUES (?, ?, ?)`,
		hostID, "billing-"+hostID.String(), hostID.String()+"@example.test").Error)

	plans := catalog.NewService(catalog.NewGormRepository(db))
	events := event.NewService(event.NewGormRepository(db, entitlement.NewProvisioner(plans)), 5<<30)
	evt, err := events.Create(ctx, hostID, event.CreateInput{Name: "billing integration"})
	require.NoError(t, err)

	version, err := plans.ActiveVersion(ctx, plan)
	require.NoError(t, err)

	licenses := entitlement.NewPurchaseLicenseService(db)
	decoder := &cannedDecoder{}
	repo := billing.NewGormRepository(db)
	svc := billing.NewService(billing.Config{
		Repo:     repo,
		Decoder:  decoder,
		Grants:   &grantAdapter{licenses: licenses},
		Licenses: &reversalAdapter{licenses: licenses, db: db},
		Provider: billing.ProviderPaddle,
		Currency: "USD",
		Enabled:  true,
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})

	h := &integrationHarness{db: db, svc: svc, repo: repo, licenses: licenses,
		decoder: decoder, hostID: hostID, eventID: evt.ID, versionID: version.ID}
	t.Cleanup(h.cleanup)
	return h
}

func (h *integrationHarness) settledPurchase(t *testing.T, amount int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, h.db.Exec(`INSERT INTO billing_purchases
		(id,event_id,host_id,plan_version_id,plan_code,quoted_amount_minor,base_amount_minor,
		 discount_amount_minor,quoted_currency,status,provider,provider_price_id,provider_transaction_id,
		 idempotency_key,pricing_reason,pricing_context,pricing_version,settled_total_minor,settled_currency,settled_at)
		VALUES (?,?,?,?,(SELECT code FROM plan_versions WHERE id=?),?,?,0,'USD','settled','paddle',?,?,?,
		        'first_purchase','first','test-v1',?, 'USD', now())`,
		id, h.eventID, h.hostID, h.versionID, h.versionID, amount, amount,
		"pri-"+id.String(), "txn-"+id.String(), "idem-"+id.String(), amount).Error)
	return id
}

func (h *integrationHarness) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, h.db.Raw(query, args...).Scan(&n).Error)
	return n
}

func (h *integrationHarness) cleanup() {
	h.db.Exec(`DELETE FROM event_plan_audits WHERE event_id = ?`, h.eventID)
	h.db.Exec(`DELETE FROM event_plan_grants WHERE event_id = ?`, h.eventID)
	h.db.Exec(`DELETE FROM event_licenses WHERE account_id = ?`, h.hostID)
	h.db.Exec(`DELETE FROM billing_provider_events WHERE purchase_id IN (SELECT id FROM billing_purchases WHERE host_id = ?)`, h.hostID)
	h.db.Exec(`DELETE FROM billing_purchases WHERE host_id = ?`, h.hostID)
	h.db.Exec(`DELETE FROM events WHERE host_id = ?`, h.hostID)
	h.db.Exec(`DELETE FROM users WHERE id = ?`, h.hostID)
}

// The reconciler's whole reason to exist: the money arrived, the purchase
// settled, and the license was never issued. Running it again must converge,
// not multiply.
func TestIntegrationReconcileRepeatedlyYieldsExactlyOneLicenseAndGrant(t *testing.T) {
	h := newIntegrationHarness(t, catalog.PlanExperience)
	ctx := context.Background()
	purchaseID := h.settledPurchase(t, 3900)

	applied := 0
	for range 10 {
		n, errs := h.svc.ReconcileUnapplied(ctx, 50)
		require.Empty(t, errs)
		applied += n
	}

	require.Equal(t, 1, applied)
	require.Equal(t, int64(1), h.count(t, `SELECT COUNT(*) FROM event_licenses WHERE purchase_id = ?`, purchaseID))
	require.Equal(t, int64(1), h.count(t, `SELECT COUNT(*) FROM event_licenses WHERE purchase_id = ? AND status = 'consumed'`, purchaseID))
	require.Equal(t, int64(1), h.count(t,
		`SELECT COUNT(*) FROM event_plan_grants g JOIN event_licenses l ON l.id = g.event_license_id WHERE l.purchase_id = ?`, purchaseID))
	require.Equal(t, int64(1), h.count(t,
		`SELECT COUNT(*) FROM event_plan_grants WHERE event_id = ? AND status = 'active'`, h.eventID))
}

// A provider that redelivers the same completion, and a reconciler running
// beside it, must still produce one license and one grant.
func TestIntegrationReplayedSettlementAndReconcileStayConsistent(t *testing.T) {
	h := newIntegrationHarness(t, catalog.PlanExperience)
	ctx := context.Background()
	purchaseID := h.settledPurchase(t, 3900)

	for i := range 3 {
		h.decoder.event = billing.ProviderEvent{
			ExternalEventID: uuid.New().String(),
			Type:            billing.EventPaymentSettled,
			PurchaseID:      purchaseID,
			PriceID:         "pri-" + purchaseID.String(),
			Subtotal:        billing.Money{AmountMinor: 3900, Currency: "USD"},
			Total:           billing.Money{AmountMinor: 3900, Currency: "USD"},
			OccurredAt:      time.Now().UTC(),
		}
		require.NoError(t, h.svc.HandleProviderEvent(ctx, []byte(`{"n":`+string(rune('0'+i))+`}`), "sig"))
		_, errs := h.svc.ReconcileUnapplied(ctx, 50)
		require.Empty(t, errs)
	}

	require.Equal(t, int64(1), h.count(t, `SELECT COUNT(*) FROM event_licenses WHERE purchase_id = ?`, purchaseID))
	require.Equal(t, int64(1), h.count(t,
		`SELECT COUNT(*) FROM event_plan_grants WHERE event_id = ? AND status = 'active'`, h.eventID))
}

// Refund of a license nobody used: the entitlement goes away and no event is
// affected, because none was ever activated with it.
func TestIntegrationRefundBeforeConsumeRevokesLicense(t *testing.T) {
	h := newIntegrationHarness(t, catalog.PlanExperience)
	ctx := context.Background()
	purchaseID := h.settledPurchase(t, 3900)
	require.NoError(t, h.db.Exec(`INSERT INTO event_licenses (id, account_id, plan_version_id, status, source, purchase_id, created_at)
		VALUES (?, ?, ?, 'available', 'purchase', ?, now())`, uuid.New(), h.hostID, h.versionID, purchaseID).Error)

	h.decoder.event = billing.ProviderEvent{
		ExternalEventID: uuid.New().String(), Type: billing.EventPaymentRefunded,
		PurchaseID: purchaseID, Adjustment: billing.Money{AmountMinor: 3900, Currency: "USD"},
	}
	require.NoError(t, h.svc.HandleProviderEvent(ctx, []byte(`{}`), "sig"))

	require.Equal(t, int64(1), h.count(t, `SELECT COUNT(*) FROM event_licenses WHERE purchase_id = ? AND status = 'revoked'`, purchaseID))
	require.Equal(t, int64(0), h.count(t, `SELECT COUNT(*) FROM event_plan_grants WHERE event_id = ? AND source = 'purchase'`, h.eventID))
	require.Equal(t, "ok", h.billingStatus(t))
}

// Refund of a license an event already used: the event keeps its plan, its
// grant and its media, and is flagged for a human instead.
func TestIntegrationRefundAfterConsumeKeepsGrantAndFlagsEvent(t *testing.T) {
	h := newIntegrationHarness(t, catalog.PlanExperience)
	ctx := context.Background()
	purchaseID := h.settledPurchase(t, 3900)
	_, errs := h.svc.ReconcileUnapplied(ctx, 50)
	require.Empty(t, errs)
	require.Equal(t, int64(1), h.count(t, `SELECT COUNT(*) FROM event_licenses WHERE purchase_id = ? AND status = 'consumed'`, purchaseID))

	h.decoder.event = billing.ProviderEvent{
		ExternalEventID: uuid.New().String(), Type: billing.EventPaymentRefunded,
		PurchaseID: purchaseID, Adjustment: billing.Money{AmountMinor: 3900, Currency: "USD"},
	}
	require.NoError(t, h.svc.HandleProviderEvent(ctx, []byte(`{}`), "sig"))

	require.Equal(t, int64(1), h.count(t, `SELECT COUNT(*) FROM event_licenses WHERE purchase_id = ? AND status = 'consumed'`, purchaseID))
	require.Equal(t, int64(1), h.count(t, `SELECT COUNT(*) FROM event_plan_grants WHERE event_id = ? AND status = 'active'`, h.eventID))
	require.Equal(t, "refunded", h.billingStatus(t))

	p, err := h.repo.GetPurchase(ctx, purchaseID)
	require.NoError(t, err)
	require.Equal(t, billing.StatusRefunded, p.Status)
	require.Equal(t, int64(3900), p.RefundedAmountMinor)
}

// A chargeback on a consumed license flags the event for review and changes
// nothing else.
func TestIntegrationDisputeAfterConsumeFlagsPaymentReview(t *testing.T) {
	h := newIntegrationHarness(t, catalog.PlanSignature)
	ctx := context.Background()
	purchaseID := h.settledPurchase(t, 6900)
	_, errs := h.svc.ReconcileUnapplied(ctx, 50)
	require.Empty(t, errs)

	h.decoder.event = billing.ProviderEvent{
		ExternalEventID: uuid.New().String(), Type: billing.EventPaymentDisputed,
		PurchaseID: purchaseID, Adjustment: billing.Money{AmountMinor: 6900, Currency: "USD"},
	}
	require.NoError(t, h.svc.HandleProviderEvent(ctx, []byte(`{}`), "sig"))

	require.Equal(t, "payment_review", h.billingStatus(t))
	require.Equal(t, int64(1), h.count(t, `SELECT COUNT(*) FROM event_plan_grants WHERE event_id = ? AND status = 'active'`, h.eventID))
	require.Equal(t, int64(0), h.count(t, `SELECT COUNT(*) FROM event_licenses WHERE purchase_id = ? AND status = 'revoked'`, purchaseID))
}

func (h *integrationHarness) billingStatus(t *testing.T) string {
	t.Helper()
	var status string
	require.NoError(t, h.db.Raw(`SELECT billing_status FROM events WHERE id = ?`, h.eventID).Scan(&status).Error)
	return status
}
