//go:build paddlesandbox

// This drill talks to the real Paddle sandbox. It is behind its own build tag
// because it creates objects in a Paddle account, so it never runs in CI or
// alongside the normal test suite.
//
//	PADDLE_API_KEY=pdl_sdbx_... \
//	PADDLE_SANDBOX_PRODUCT_ID=pro_... \
//	PADDLE_CHECKOUT_URL=http://localhost:3000/billing/checkout \
//	go test -tags paddlesandbox -count=1 -v ./internal/platform/paddle/
//
// It proves the two things unit tests cannot: that Paddle accepts the
// non-catalog price this adapter builds from an internal quote, and that the
// amount it will charge is exactly the amount the pricing engine decided.
// Completing the payment and receiving the webhook still needs a browser and a
// public URL.
package paddle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/candidcrowd/candidcrowd-backend/internal/billing"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upgradeAmountMinor is $30.00: an Experience to Signature upgrade in the
// first-time context. It deliberately has no price record in Paddle's catalog.
const upgradeAmountMinor = 3000

func sandboxConfig(t *testing.T) (apiKey, productID string) {
	t.Helper()
	apiKey = os.Getenv("PADDLE_API_KEY")
	productID = os.Getenv("PADDLE_SANDBOX_PRODUCT_ID")
	if apiKey == "" || productID == "" {
		t.Skip("PADDLE_API_KEY and PADDLE_SANDBOX_PRODUCT_ID are required")
	}
	return apiKey, productID
}

func checkoutInput(productID string, purchaseID, eventID uuid.UUID) billing.CreateCheckoutInput {
	return billing.CreateCheckoutInput{
		PurchaseID:        purchaseID,
		EventID:           eventID,
		PlanCode:          "signature",
		Description:       "Signature event package (upgrade)",
		AmountMinor:       upgradeAmountMinor,
		Currency:          "USD",
		ProviderProductID: productID,
	}
}

func TestSandboxCreatesTransactionFromAnInlineQuote(t *testing.T) {
	apiKey, productID := sandboxConfig(t)
	// Paddle refuses to create any transaction unless the account has a default
	// payment link or the request names a checkout page.
	adapter := NewCheckoutAdapter(NewClient("sandbox", apiKey), os.Getenv("PADDLE_CHECKOUT_URL"))
	purchaseID, eventID := uuid.New(), uuid.New()

	target, err := adapter.CreateCheckout(context.Background(), checkoutInput(productID, purchaseID, eventID))
	require.NoError(t, err)
	require.NotEmpty(t, target.ExternalCheckoutID, "transaction id for Paddle.js")
	require.NotEmpty(t, target.ProviderPriceID, "the price Paddle created, snapshotted for settlement")
	t.Logf("transaction=%s price=%s", target.ExternalCheckoutID, target.ProviderPriceID)

	// Read the transaction back. This is the assertion that matters: the amount
	// Paddle will charge has to be the amount the pricing engine decided, not a
	// catalog price that happened to be nearby.
	txn := fetchSandboxTransaction(t, apiKey, target.ExternalCheckoutID)

	assert.Equal(t, fmt.Sprint(upgradeAmountMinor), txn.Details.Totals.Subtotal,
		"Paddle must charge the quoted amount")
	assert.Equal(t, "USD", txn.Details.Totals.CurrencyCode)
	require.Len(t, txn.Items, 1)
	assert.Equal(t, target.ProviderPriceID, txn.Items[0].Price.ID,
		"the snapshotted price is the one settlement will be verified against")
	assert.Equal(t, purchaseID.String(), txn.CustomData["purchase_id"],
		"the webhook finds its purchase through this")
	assert.Equal(t, eventID.String(), txn.CustomData["event_id"])
}

// TestSandboxTransactionsAreNotIdempotent records a provider behaviour this
// system must not rely on.
//
// Paddle ignores a client-supplied Idempotency-Key here: the same key with the
// same body yields a second, independent transaction. Duplicate charges are
// prevented one layer up, where the billing service calls a provider at most
// once per purchase row and the database permits a single open purchase per
// event. If this test ever starts failing because the two IDs match, Paddle
// added idempotency — good news, but the guarantee still belongs to us.
func TestSandboxTransactionsAreNotIdempotent(t *testing.T) {
	apiKey, productID := sandboxConfig(t)
	adapter := NewCheckoutAdapter(NewClient("sandbox", apiKey), os.Getenv("PADDLE_CHECKOUT_URL"))
	purchaseID, eventID := uuid.New(), uuid.New()
	in := checkoutInput(productID, purchaseID, eventID)

	first, err := adapter.CreateCheckout(context.Background(), in)
	require.NoError(t, err)
	second, err := adapter.CreateCheckout(context.Background(), in)
	require.NoError(t, err)

	assert.NotEqual(t, first.ExternalCheckoutID, second.ExternalCheckoutID,
		"Paddle creates a new transaction per call; the duplicate guard is ours")
}

type sandboxTransaction struct {
	ID         string            `json:"id"`
	Status     string            `json:"status"`
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

func fetchSandboxTransaction(t *testing.T, apiKey, id string) sandboxTransaction {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, sandboxBaseURL+"/transactions/"+id, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	require.NoError(t, err)
	require.Less(t, resp.StatusCode, 400, "read transaction: %s", string(body))

	var payload struct {
		Data sandboxTransaction `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))
	return payload.Data
}
