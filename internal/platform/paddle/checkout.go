package paddle

import (
	"context"
	"fmt"

	"github.com/candidcrowd/candidcrowd-backend/internal/billing"
)

// CheckoutAdapter implements billing.CheckoutGateway using the Paddle API. It
// executes an amount the pricing engine already decided and verified; it never
// looks up or adjusts a price of its own.
type CheckoutAdapter struct {
	client *Client
	// checkoutURL overrides the account's default payment link per transaction.
	// It is normally empty: Paddle accepts it only for a domain approved under
	// Website approval, and rejects everything else — including localhost — so
	// a wrong value here breaks every checkout. The account default is what
	// Paddle requires, and what an empty value falls back to.
	checkoutURL string
}

var _ billing.CheckoutGateway = (*CheckoutAdapter)(nil)

// NewCheckoutAdapter creates a new Paddle checkout adapter.
func NewCheckoutAdapter(client *Client, checkoutURL string) *CheckoutAdapter {
	return &CheckoutAdapter{client: client, checkoutURL: checkoutURL}
}

func (a *CheckoutAdapter) CreateCheckout(ctx context.Context, in billing.CreateCheckoutInput) (billing.CheckoutTarget, error) {
	customData := map[string]string{
		"purchase_id": in.PurchaseID.String(),
		"event_id":    in.EventID.String(),
	}

	out, err := a.client.CreateTransaction(ctx, CreateTransactionInput{
		IdempotencyKey: in.PurchaseID.String(),
		PriceID:        in.ProviderPriceID,
		ProductID:      in.ProviderProductID,
		Description:    in.Description,
		AmountMinor:    in.AmountMinor,
		Currency:       in.Currency,
		CustomData:     customData,
		CheckoutURL:    a.checkoutURL,
	})
	if err != nil {
		return billing.CheckoutTarget{}, fmt.Errorf("paddle checkout: %w", err)
	}

	return billing.CheckoutTarget{
		ExternalCheckoutID: out.TransactionID,
		ProviderPriceID:    out.PriceID,
	}, nil
}
