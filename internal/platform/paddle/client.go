package paddle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const (
	sandboxBaseURL    = "https://sandbox-api.paddle.com"
	productionBaseURL = "https://api.paddle.com"
)

// Client communicates with the Paddle Billing API.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a Paddle API client.
func NewClient(environment, apiKey string) *Client {
	base := sandboxBaseURL
	if environment == "production" {
		base = productionBaseURL
	}
	return &Client{
		baseURL: base,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

type createTransactionRequest struct {
	Items      []transactionItem `json:"items"`
	CustomData map[string]string `json:"custom_data,omitempty"`
	// Checkout names the page that can complete this transaction. Paddle
	// refuses to create a transaction when neither this nor a default payment
	// link on the account is set.
	Checkout *checkoutSettings `json:"checkout,omitempty"`
}

type checkoutSettings struct {
	URL string `json:"url"`
}

// transactionItem carries either a catalog price ID or a non-catalog price
// Paddle creates for this transaction alone. Paddle rejects both at once.
type transactionItem struct {
	PriceID  string           `json:"price_id,omitempty"`
	Price    *nonCatalogPrice `json:"price,omitempty"`
	Quantity int              `json:"quantity"`
}

// nonCatalogPrice is how a verified internal quote is executed without adding a
// provider price for every discounted total. The product is the catalog anchor;
// the amount comes from the pricing engine.
type nonCatalogPrice struct {
	Description string        `json:"description"`
	ProductID   string        `json:"product_id"`
	UnitPrice   paddleMoney   `json:"unit_price"`
	Quantity    priceQuantity `json:"quantity"`
	TaxMode     string        `json:"tax_mode"`
}

type paddleMoney struct {
	Amount       string `json:"amount"`
	CurrencyCode string `json:"currency_code"`
}

type priceQuantity struct {
	Minimum int `json:"minimum"`
	Maximum int `json:"maximum"`
}

type createTransactionResponse struct {
	Data struct {
		ID    string `json:"id"`
		Items []struct {
			Price struct {
				ID string `json:"id"`
			} `json:"price"`
		} `json:"items"`
	} `json:"data"`
	Error *paddleError `json:"error,omitempty"`
}

type paddleError struct {
	Type   string `json:"type"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// CreateTransactionInput is one checkout, already priced.
type CreateTransactionInput struct {
	// IdempotencyKey is the purchase ID. Paddle does not honour a
	// client-supplied Idempotency-Key on this endpoint — a sandbox drill proved
	// it creates a second transaction — so this is sent for traceability only.
	// What actually prevents a duplicate charge is the billing service: it calls
	// a provider at most once per purchase row, and the database allows one open
	// purchase per event.
	IdempotencyKey string
	// PriceID selects an existing Paddle price. When empty, ProductID and
	// AmountMinor are used to create a non-catalog price instead.
	PriceID     string
	ProductID   string
	Description string
	AmountMinor int64
	Currency    string
	CustomData  map[string]string
	// CheckoutURL overrides the account's default payment link.
	CheckoutURL string
}

// CreateTransactionOutput is the transaction the browser will open, plus the
// price Paddle will actually charge, which is only knowable here when the price
// is non-catalog.
type CreateTransactionOutput struct {
	TransactionID string
	PriceID       string
}

// CreateTransaction creates a Paddle transaction.
//
// It is NOT idempotent: calling it twice creates two transactions, whatever the
// Idempotency-Key header says. Callers must not retry it on their own; the
// billing service is what guarantees one call per purchase.
func (c *Client) CreateTransaction(ctx context.Context, in CreateTransactionInput) (CreateTransactionOutput, error) {
	item := transactionItem{Quantity: 1}
	switch {
	case in.PriceID != "":
		item.PriceID = in.PriceID
	case in.ProductID != "":
		item.Price = &nonCatalogPrice{
			Description: in.Description,
			ProductID:   in.ProductID,
			UnitPrice: paddleMoney{
				Amount:       strconv.FormatInt(in.AmountMinor, 10),
				CurrencyCode: in.Currency,
			},
			Quantity: priceQuantity{Minimum: 1, Maximum: 1},
			TaxMode:  "account_setting",
		}
	default:
		return CreateTransactionOutput{}, fmt.Errorf("paddle: neither a price ID nor a product ID was provided")
	}

	reqBody := createTransactionRequest{Items: []transactionItem{item}, CustomData: in.CustomData}
	if in.CheckoutURL != "" {
		reqBody.Checkout = &checkoutSettings{URL: in.CheckoutURL}
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return CreateTransactionOutput{}, fmt.Errorf("paddle: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/transactions", bytes.NewReader(payload))
	if err != nil {
		return CreateTransactionOutput{}, fmt.Errorf("paddle: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Idempotency-Key", in.IdempotencyKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return CreateTransactionOutput{}, fmt.Errorf("paddle: send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return CreateTransactionOutput{}, fmt.Errorf("paddle: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return CreateTransactionOutput{}, fmt.Errorf("paddle: API error status %d: %s", resp.StatusCode, string(body))
	}

	var result createTransactionResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return CreateTransactionOutput{}, fmt.Errorf("paddle: parse response: %w", err)
	}

	out := CreateTransactionOutput{TransactionID: result.Data.ID, PriceID: in.PriceID}
	if out.PriceID == "" && len(result.Data.Items) > 0 {
		out.PriceID = result.Data.Items[0].Price.ID
	}
	return out, nil
}
