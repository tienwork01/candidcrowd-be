package fakepay

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/billing"
	"github.com/google/uuid"
)

// Gateway implements billing.CheckoutGateway for testing portability.
type Gateway struct {
	BaseURL string
}

var _ billing.CheckoutGateway = (*Gateway)(nil)

func NewGateway(baseURL string) *Gateway {
	return &Gateway{BaseURL: baseURL}
}

func (g *Gateway) CreateCheckout(_ context.Context, in billing.CreateCheckoutInput) (billing.CheckoutTarget, error) {
	checkoutID := fmt.Sprintf("fake_chk_%s", in.PurchaseID)

	// Mirror a real provider: a fixed price is used as given, and any other
	// amount becomes a price created for this transaction alone.
	priceID := in.ProviderPriceID
	if priceID == "" {
		if in.ProviderProductID == "" {
			return billing.CheckoutTarget{}, fmt.Errorf("fakepay: neither a price ID nor a product ID was provided")
		}
		priceID = fmt.Sprintf("fake_pri_%s_%d", in.ProviderProductID, in.AmountMinor)
	}

	url := fmt.Sprintf("%s/pay?id=%s&price=%s&amount=%d&currency=%s",
		g.BaseURL, checkoutID, priceID, in.AmountMinor, in.Currency)
	return billing.CheckoutTarget{
		ExternalCheckoutID: checkoutID,
		ProviderPriceID:    priceID,
		URL:                url,
	}, nil
}

// Decoder implements billing.WebhookDecoder for testing portability.
type Decoder struct{}

var _ billing.WebhookDecoder = (*Decoder)(nil)

func NewDecoder() *Decoder {
	return &Decoder{}
}

type fakeWebhookPayload struct {
	EventID       string `json:"event_id"`
	Type          string `json:"type"`
	TransactionID string `json:"transaction_id"`
	PurchaseID    string `json:"purchase_id"`
	PriceID       string `json:"price_id"`
	AmountMinor   int64  `json:"amount_minor"`
	Currency      string `json:"currency"`
}

func (d *Decoder) DecodeWebhook(rawBody []byte, signature string) (billing.ProviderEvent, error) {
	if signature != "valid_fake_sig" {
		return billing.ProviderEvent{}, fmt.Errorf("fakepay: invalid signature")
	}

	var p fakeWebhookPayload
	if err := json.Unmarshal(rawBody, &p); err != nil {
		return billing.ProviderEvent{}, fmt.Errorf("fakepay: parse: %w", err)
	}

	purchaseUUID, err := uuid.Parse(p.PurchaseID)
	if err != nil {
		return billing.ProviderEvent{}, fmt.Errorf("fakepay: invalid purchase id: %w", err)
	}

	var evType billing.ProviderEventType
	switch p.Type {
	case "payment.completed":
		evType = billing.EventPaymentSettled
	case "payment.failed":
		evType = billing.EventPaymentFailed
	default:
		return billing.ProviderEvent{}, fmt.Errorf("fakepay: unknown type: %s", p.Type)
	}

	return billing.ProviderEvent{
		ExternalEventID:       p.EventID,
		Type:                  evType,
		ExternalTransactionID: p.TransactionID,
		PurchaseID:            purchaseUUID,
		PriceID:               p.PriceID,
		Subtotal:              billing.Money{AmountMinor: p.AmountMinor, Currency: p.Currency},
		Tax:                   billing.Money{AmountMinor: 0, Currency: p.Currency},
		Total:                 billing.Money{AmountMinor: p.AmountMinor, Currency: p.Currency},
		OccurredAt:            time.Now().UTC(),
	}, nil
}
