package paddle

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/billing"
	"github.com/google/uuid"
)

// envelope is the part of every Paddle notification that does not depend on
// what it is about.
type envelope struct {
	EventID    string          `json:"event_id"`
	EventType  string          `json:"event_type"`
	OccurredAt time.Time       `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}

type transactionData struct {
	ID         string            `json:"id"`
	CustomData map[string]string `json:"custom_data"`
	Details    struct {
		Totals totals `json:"totals"`
	} `json:"details"`
	Items []struct {
		Price struct {
			ID string `json:"id"`
		} `json:"price"`
	} `json:"items"`
}

// adjustmentData is a refund or chargeback. Paddle does not echo the
// transaction's custom data here, so the transaction ID is the only link back
// to a purchase.
type adjustmentData struct {
	ID            string `json:"id"`
	Action        string `json:"action"`
	TransactionID string `json:"transaction_id"`
	Status        string `json:"status"`
	Totals        totals `json:"totals"`
}

type totals struct {
	Subtotal     string `json:"subtotal"`
	Tax          string `json:"tax"`
	Total        string `json:"total"`
	CurrencyCode string `json:"currency_code"`
}

// mapToCanonical maps a verified Paddle notification to a canonical
// ProviderEvent. An event Paddle sends but billing has no state for is mapped
// to billing.EventIgnored rather than rejected: a webhook subscription is wider
// than the state machine, and failing those would make Paddle retry forever.
func mapToCanonical(raw []byte) (billing.ProviderEvent, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return billing.ProviderEvent{}, fmt.Errorf("paddle mapper: parse envelope: %w", err)
	}
	base := billing.ProviderEvent{ExternalEventID: env.EventID, OccurredAt: env.OccurredAt}
	if env.EventID == "" {
		return billing.ProviderEvent{}, fmt.Errorf("paddle mapper: missing event_id")
	}

	switch {
	case strings.HasPrefix(env.EventType, "adjustment."):
		return mapAdjustment(env, base)
	case strings.HasPrefix(env.EventType, "transaction."):
		return mapTransaction(env, base)
	default:
		base.Type = billing.EventIgnored
		return base, nil
	}
}

func mapTransaction(env envelope, ev billing.ProviderEvent) (billing.ProviderEvent, error) {
	switch env.EventType {
	case "transaction.completed":
		ev.Type = billing.EventPaymentSettled
	case "transaction.payment_failed":
		ev.Type = billing.EventPaymentFailed
	case "transaction.canceled":
		ev.Type = billing.EventPaymentCanceled
	default:
		ev.Type = billing.EventIgnored
	}

	var data transactionData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return billing.ProviderEvent{}, fmt.Errorf("paddle mapper: parse transaction: %w", err)
	}
	ev.ExternalTransactionID = data.ID

	// custom_data is what the checkout attached. It is absent on transactions
	// this system did not create, and the service then resolves the purchase
	// through the transaction ID instead.
	if rawID, ok := data.CustomData["purchase_id"]; ok {
		purchaseID, err := uuid.Parse(rawID)
		if err != nil {
			return billing.ProviderEvent{}, fmt.Errorf("paddle mapper: invalid purchase_id: %w", err)
		}
		ev.PurchaseID = purchaseID
	}
	if len(data.Items) > 0 {
		ev.PriceID = data.Items[0].Price.ID
	}

	currency := data.Details.Totals.CurrencyCode
	ev.Subtotal = billing.Money{AmountMinor: parseMinorUnits(data.Details.Totals.Subtotal), Currency: currency}
	ev.Tax = billing.Money{AmountMinor: parseMinorUnits(data.Details.Totals.Tax), Currency: currency}
	ev.Total = billing.Money{AmountMinor: parseMinorUnits(data.Details.Totals.Total), Currency: currency}
	return ev, nil
}

func mapAdjustment(env envelope, ev billing.ProviderEvent) (billing.ProviderEvent, error) {
	var data adjustmentData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return billing.ProviderEvent{}, fmt.Errorf("paddle mapper: parse adjustment: %w", err)
	}
	ev.ExternalTransactionID = data.TransactionID

	switch data.Action {
	case "refund":
		ev.Type = billing.EventPaymentRefunded
	case "chargeback", "chargeback_warning":
		ev.Type = billing.EventPaymentDisputed
	case "chargeback_reverse":
		ev.Type = billing.EventPaymentDisputeReversed
	default:
		// credit and credit_reverse only exist for subscriptions.
		ev.Type = billing.EventIgnored
		return ev, nil
	}

	// A refund Paddle has not approved yet moves no money.
	if data.Action == "refund" && data.Status == "pending_approval" {
		ev.Type = billing.EventIgnored
		return ev, nil
	}

	// adjustment.created and adjustment.updated describe the same money. Paddle
	// sends both when a refund is approved after review, and this account
	// subscribes to both, so the adjustment itself is the identity.
	ev.DedupeKey = "adjustment:" + data.ID

	amount := parseMinorUnits(data.Totals.Total)
	ev.Adjustment = billing.Money{AmountMinor: amount, Currency: data.Totals.CurrencyCode}
	ev.Total = ev.Adjustment
	return ev, nil
}

// parseMinorUnits converts a string amount (e.g. "1999") to int64 minor units.
// Paddle writes adjustment amounts as positive numbers with the direction in
// the action, so a sign is never expected and never silently dropped.
func parseMinorUnits(s string) int64 {
	s = strings.TrimSpace(s)
	negative := strings.HasPrefix(s, "-")
	var n int64
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + int64(c-'0')
		}
	}
	if negative {
		return -n
	}
	return n
}
