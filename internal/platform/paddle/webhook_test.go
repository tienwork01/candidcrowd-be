package paddle

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/billing"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeSignature(secret string, ts int64, body []byte) string {
	signed := fmt.Sprintf("%d:%s", ts, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signed))
	h1 := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("ts=%d;h1=%s", ts, h1)
}

func TestPaddleWebhook_ValidSignatureAndCompletedTransaction(t *testing.T) {
	secret := "pdl_ntfset_test_secret_123"
	verifier := NewWebhookVerifier(secret)

	now := time.Now().UTC()
	verifier.now = func() time.Time { return now }

	purchaseID := uuid.New()
	body := []byte(fmt.Sprintf(`{
		"event_id": "evt_01h8abcdef",
		"event_type": "transaction.completed",
		"occurred_at": "%s",
		"data": {
			"id": "txn_01h8xyz",
			"custom_data": {
				"purchase_id": "%s",
				"event_id": "%s"
			},
			"details": {
				"totals": {
					"subtotal": "4900",
					"tax": "500",
					"total": "5400",
					"currency_code": "USD"
				}
			},
			"items": [
				{
					"price": {
						"id": "pri_paddle_exp"
					}
				}
			]
		}
	}`, now.Format(time.RFC3339Nano), purchaseID.String(), uuid.New().String()))

	sig := makeSignature(secret, now.Unix(), body)

	ev, err := verifier.DecodeWebhook(body, sig)
	require.NoError(t, err)

	assert.Equal(t, "evt_01h8abcdef", ev.ExternalEventID)
	assert.Equal(t, billing.EventPaymentSettled, ev.Type)
	assert.Equal(t, "txn_01h8xyz", ev.ExternalTransactionID)
	assert.Equal(t, purchaseID, ev.PurchaseID)
	assert.Equal(t, "pri_paddle_exp", ev.PriceID)
	assert.Equal(t, int64(4900), ev.Subtotal.AmountMinor)
	assert.Equal(t, int64(500), ev.Tax.AmountMinor)
	assert.Equal(t, int64(5400), ev.Total.AmountMinor)
	assert.Equal(t, "USD", ev.Total.Currency)
}

func TestPaddleWebhook_TamperedBodyRejected(t *testing.T) {
	secret := "test_secret"
	verifier := NewWebhookVerifier(secret)
	now := time.Now().UTC()
	verifier.now = func() time.Time { return now }

	originalBody := []byte(`{"event_id":"evt_1","event_type":"transaction.completed"}`)
	sig := makeSignature(secret, now.Unix(), originalBody)

	tamperedBody := []byte(`{"event_id":"evt_1","event_type":"transaction.completed","extra":true}`)

	_, err := verifier.DecodeWebhook(tamperedBody, sig)
	assert.ErrorContains(t, err, "signature mismatch")
}

func TestPaddleWebhook_ExpiredTimestampRejected(t *testing.T) {
	secret := "test_secret"
	verifier := NewWebhookVerifier(secret)
	now := time.Now().UTC()
	verifier.now = func() time.Time { return now }

	oldTS := now.Add(-10 * time.Minute).Unix()
	body := []byte(`{"event_id":"evt_1"}`)
	sig := makeSignature(secret, oldTS, body)

	_, err := verifier.DecodeWebhook(body, sig)
	assert.ErrorContains(t, err, "timestamp too old")
}

// Paddle does not echo custom data on every notification about a transaction,
// so a missing purchase_id is resolved from the transaction ID instead of being
// rejected.
func TestPaddleWebhook_MissingPurchaseIDFallsBackToTransaction(t *testing.T) {
	secret := "test_secret"
	verifier := NewWebhookVerifier(secret)
	now := time.Now().UTC()
	verifier.now = func() time.Time { return now }

	body := []byte(`{
		"event_id": "evt_1",
		"event_type": "transaction.completed",
		"data": {
			"id": "txn_1",
			"custom_data": {}
		}
	}`)
	sig := makeSignature(secret, now.Unix(), body)

	ev, err := verifier.DecodeWebhook(body, sig)
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, ev.PurchaseID)
	assert.Equal(t, "txn_1", ev.ExternalTransactionID)
}

// A subscribed event billing has no state for is acknowledged, not rejected:
// returning an error would make Paddle retry it until the schedule gives up.
func TestPaddleWebhook_UnknownEventTypeIsIgnored(t *testing.T) {
	secret := "test_secret"
	verifier := NewWebhookVerifier(secret)
	now := time.Now().UTC()
	verifier.now = func() time.Time { return now }

	body := []byte(`{
		"event_id": "evt_1",
		"event_type": "subscription.created",
		"data": {}
	}`)
	sig := makeSignature(secret, now.Unix(), body)

	ev, err := verifier.DecodeWebhook(body, sig)
	require.NoError(t, err)
	assert.Equal(t, billing.EventIgnored, ev.Type)
}

func TestPaddleWebhook_RefundAdjustment(t *testing.T) {
	secret := "test_secret"
	verifier := NewWebhookVerifier(secret)
	now := time.Now().UTC()
	verifier.now = func() time.Time { return now }

	// An adjustment carries no custom data and totals live at the top level.
	body := []byte(`{
		"event_id": "evt_adj_1",
		"event_type": "adjustment.created",
		"data": {
			"id": "adj_1",
			"action": "refund",
			"status": "approved",
			"transaction_id": "txn_9",
			"totals": {"subtotal": "3900", "tax": "0", "total": "3900", "currency_code": "USD"}
		}
	}`)
	sig := makeSignature(secret, now.Unix(), body)

	ev, err := verifier.DecodeWebhook(body, sig)
	require.NoError(t, err)
	assert.Equal(t, billing.EventPaymentRefunded, ev.Type)
	assert.Equal(t, "txn_9", ev.ExternalTransactionID)
	assert.Equal(t, int64(3900), ev.Adjustment.AmountMinor)
	assert.Equal(t, "USD", ev.Adjustment.Currency)
}

func TestPaddleWebhook_AdjustmentActions(t *testing.T) {
	secret := "test_secret"
	verifier := NewWebhookVerifier(secret)
	now := time.Now().UTC()
	verifier.now = func() time.Time { return now }

	for action, want := range map[string]billing.ProviderEventType{
		"chargeback":         billing.EventPaymentDisputed,
		"chargeback_warning": billing.EventPaymentDisputed,
		"chargeback_reverse": billing.EventPaymentDisputeReversed,
		"credit":             billing.EventIgnored,
	} {
		body := []byte(`{
			"event_id": "evt_` + action + `",
			"event_type": "adjustment.created",
			"data": {"id": "adj_x", "action": "` + action + `", "transaction_id": "txn_9",
			         "totals": {"total": "3900", "currency_code": "USD"}}
		}`)
		ev, err := verifier.DecodeWebhook(body, makeSignature(secret, now.Unix(), body))
		require.NoError(t, err, action)
		assert.Equal(t, want, ev.Type, action)
	}
}

// A refund Paddle has not approved yet has moved no money.
func TestPaddleWebhook_PendingRefundIsIgnored(t *testing.T) {
	secret := "test_secret"
	verifier := NewWebhookVerifier(secret)
	now := time.Now().UTC()
	verifier.now = func() time.Time { return now }

	body := []byte(`{
		"event_id": "evt_pending",
		"event_type": "adjustment.created",
		"data": {"id": "adj_p", "action": "refund", "status": "pending_approval",
		         "transaction_id": "txn_9", "totals": {"total": "3900", "currency_code": "USD"}}
	}`)

	ev, err := verifier.DecodeWebhook(body, makeSignature(secret, now.Unix(), body))
	require.NoError(t, err)
	assert.Equal(t, billing.EventIgnored, ev.Type)
}

// The two notifications that describe one refund must carry one identity.
func TestPaddleWebhook_AdjustmentEventsShareADedupeKey(t *testing.T) {
	secret := "test_secret"
	verifier := NewWebhookVerifier(secret)
	now := time.Now().UTC()
	verifier.now = func() time.Time { return now }

	var keys []string
	for _, eventType := range []string{"adjustment.created", "adjustment.updated"} {
		body := []byte(`{
			"event_id": "evt_` + eventType + `",
			"event_type": "` + eventType + `",
			"data": {"id": "adj_7", "action": "refund", "status": "approved",
			         "transaction_id": "txn_9", "totals": {"total": "1000", "currency_code": "USD"}}
		}`)
		ev, err := verifier.DecodeWebhook(body, makeSignature(secret, now.Unix(), body))
		require.NoError(t, err)
		require.Equal(t, billing.EventPaymentRefunded, ev.Type)
		keys = append(keys, ev.DedupeKey)
	}

	require.Equal(t, keys[0], keys[1])
	require.Equal(t, "adjustment:adj_7", keys[0])
}
