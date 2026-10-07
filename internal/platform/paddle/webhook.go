package paddle

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/billing"
)

const (
	// maxTimestampAge is the maximum age of a webhook timestamp before it
	// is rejected as a replay.
	maxTimestampAge = 5 * time.Minute
)

// WebhookVerifier implements billing.WebhookDecoder for Paddle.
type WebhookVerifier struct {
	secret string
	maxAge time.Duration
	now    func() time.Time
}

var _ billing.WebhookDecoder = (*WebhookVerifier)(nil)

// NewWebhookVerifier creates a Paddle webhook verifier.
func NewWebhookVerifier(secret string) *WebhookVerifier {
	return &WebhookVerifier{
		secret: secret,
		maxAge: maxTimestampAge,
		now:    time.Now,
	}
}

// DecodeWebhook verifies the Paddle signature and maps the payload to a
// canonical billing.ProviderEvent.
func (v *WebhookVerifier) DecodeWebhook(rawBody []byte, signature string) (billing.ProviderEvent, error) {
	// Parse signature: ts=...; h1=...
	ts, sigs, err := parseSignature(signature)
	if err != nil {
		return billing.ProviderEvent{}, fmt.Errorf("paddle webhook: %w", err)
	}

	// Check timestamp freshness.
	tsTime := time.Unix(ts, 0)
	if age := v.now().Sub(tsTime); age > v.maxAge || age < -v.maxAge {
		return billing.ProviderEvent{}, fmt.Errorf("paddle webhook: timestamp too old or too far in future: %s", tsTime)
	}

	// Verify HMAC.
	signed := fmt.Sprintf("%d:%s", ts, string(rawBody))
	mac := hmac.New(sha256.New, []byte(v.secret))
	mac.Write([]byte(signed))
	expected := hex.EncodeToString(mac.Sum(nil))

	valid := false
	for _, sig := range sigs {
		if hmac.Equal([]byte(expected), []byte(sig)) {
			valid = true
			break
		}
	}
	if !valid {
		return billing.ProviderEvent{}, fmt.Errorf("paddle webhook: signature mismatch")
	}

	return mapToCanonical(rawBody)
}

// parseSignature extracts ts and h1 values from the Paddle-Signature header.
// Format: ts=1234567890;h1=abc123;h1=def456
func parseSignature(sig string) (ts int64, h1s []string, err error) {
	parts := strings.Split(sig, ";")
	for _, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key, value := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
		switch key {
		case "ts":
			ts, err = strconv.ParseInt(value, 10, 64)
			if err != nil {
				return 0, nil, fmt.Errorf("invalid timestamp: %w", err)
			}
		case "h1":
			h1s = append(h1s, value)
		}
	}
	if ts == 0 || len(h1s) == 0 {
		return 0, nil, fmt.Errorf("missing ts or h1 in signature")
	}
	return ts, h1s, nil
}
