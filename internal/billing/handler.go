package billing

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const maxWebhookBody = 64 * 1024 // 64 KiB

// HostResolver resolves the internal host UUID from the request context.
type HostResolver func(c *gin.Context) (uuid.UUID, error)

// Handler exposes billing HTTP endpoints.
type Handler struct {
	svc         *Service
	resolveHost HostResolver
}

func NewHandler(svc *Service, resolveHost HostResolver) *Handler {
	return &Handler{svc: svc, resolveHost: resolveHost}
}

func (h *Handler) getHostID(c *gin.Context) (uuid.UUID, error) {
	if h.resolveHost != nil {
		return h.resolveHost(c)
	}
	if uid := c.GetString("user_id"); uid != "" {
		return uuid.Parse(uid)
	}
	return uuid.Nil, errors.New("cannot resolve host")
}

// --- Request/Response types ---

type createCheckoutRequest struct {
	PlanCode string `json:"plan_code" binding:"required"`
	Currency string `json:"currency" binding:"required"`
}

type checkoutResponse struct {
	PurchaseID string         `json:"purchase_id"`
	Status     PurchaseStatus `json:"status"`
	Checkout   *checkoutURL   `json:"checkout,omitempty"`
}

type checkoutURL struct {
	SessionID string `json:"session_id"`
	Mode      string `json:"mode"`
}

type purchaseResponse struct {
	ID                 string         `json:"id"`
	EventID            string         `json:"event_id"`
	PlanCode           string         `json:"plan_code"`
	Status             PurchaseStatus `json:"status"`
	EntitlementApplied bool           `json:"entitlement_applied"`
}

// Offers handles GET /api/v1/events/:id/offers. Prices are account- and
// event-specific; the public plan catalog is not a checkout quote.
func (h *Handler) Offers(c *gin.Context) {
	eventID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_event_id", "invalid event ID"))
		return
	}
	hostID, err := h.getHostID(c)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusUnauthorized, "unauthorized", "invalid host identity"))
		return
	}
	currency := strings.ToUpper(strings.TrimSpace(c.DefaultQuery("currency", "USD")))
	sellable := catalog.SellablePlans()
	offers := make([]Quote, 0, len(sellable))
	for _, target := range sellable {
		quote, quoteErr := h.svc.QuoteEventPurchase(c.Request.Context(), eventID, hostID, target, currency)
		if errors.Is(quoteErr, ErrInvalidPlanTransition) {
			continue
		}
		if quoteErr != nil {
			h.mapError(c, quoteErr)
			return
		}
		offers = append(offers, quote)
	}
	c.JSON(http.StatusOK, gin.H{"offers": offers})
}

// --- Endpoints ---

// CreateCheckout handles POST /api/v1/events/:id/checkout
func (h *Handler) CreateCheckout(c *gin.Context) {
	eventID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_event_id", "invalid event ID"))
		return
	}

	var req createCheckoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", "plan_code and currency are required"))
		return
	}

	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idempotencyKey == "" {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required"))
		return
	}

	hostID, err := h.getHostID(c)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusUnauthorized, "unauthorized", "invalid host identity"))
		return
	}

	out, err := h.svc.CreateCheckout(c.Request.Context(), CreateCheckoutCommand{
		EventID:        eventID,
		HostID:         hostID,
		PlanCode:       catalog.PlanCode(req.PlanCode),
		Currency:       strings.ToUpper(req.Currency),
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		h.mapError(c, err)
		return
	}

	resp := checkoutResponse{
		PurchaseID: out.PurchaseID.String(),
		Status:     out.Status,
	}
	if out.CheckoutSessionID != "" {
		resp.Checkout = &checkoutURL{SessionID: out.CheckoutSessionID, Mode: "embedded"}
	}
	c.JSON(http.StatusOK, resp)
}

// GetPurchase handles GET /api/v1/billing/purchases/:purchaseId
func (h *Handler) GetPurchase(c *gin.Context) {
	purchaseID, err := uuid.Parse(c.Param("purchaseId"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_purchase_id", "invalid purchase ID"))
		return
	}

	hostID, err := h.getHostID(c)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusUnauthorized, "unauthorized", "invalid host identity"))
		return
	}

	p, err := h.svc.GetPurchase(c.Request.Context(), purchaseID, hostID)
	if err != nil {
		h.mapError(c, err)
		return
	}

	c.JSON(http.StatusOK, purchaseResponse{
		ID:                 p.ID.String(),
		EventID:            p.EventID.String(),
		PlanCode:           string(p.PlanCode),
		Status:             p.Status,
		EntitlementApplied: p.EntitlementAppliedAt != nil,
	})
}

// Webhook handles POST /api/v1/webhooks/paddle
// This handler name is provider-specific because the transport endpoint is
// provider-specific (different providers have different signature headers).
func (h *Handler) Webhook(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxWebhookBody))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", "cannot read request body"))
		return
	}

	signature := c.GetHeader("Paddle-Signature")
	if signature == "" {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_webhook_signature", "missing signature header"))
		return
	}

	if err := h.svc.HandleProviderEvent(c.Request.Context(), body, signature); err != nil {
		if errors.Is(err, ErrPaymentIntegrityMismatch) {
			c.JSON(http.StatusOK, gin.H{"status": "acknowledged"}) // Don't retry
			return
		}
		// Return 500 so provider retries.
		apierror.Respond(c, apierror.New(http.StatusInternalServerError, "webhook_processing_error", "failed to process webhook"))
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) mapError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrBillingDisabled):
		apierror.Respond(c, apierror.New(http.StatusNotFound, "billing_disabled", "billing is not available"))
	case errors.Is(err, ErrPurchaseNotFound):
		apierror.Respond(c, apierror.New(http.StatusNotFound, "purchase_not_found", "purchase not found"))
	case errors.Is(err, ErrInvalidPlanTransition):
		apierror.Respond(c, apierror.New(http.StatusConflict, "invalid_plan_transition", err.Error()))
	case errors.Is(err, ErrPurchaseConflict):
		apierror.Respond(c, apierror.New(http.StatusConflict, "purchase_conflict", "an open purchase already exists"))
	case errors.Is(err, ErrIdempotencyConflict):
		apierror.Respond(c, apierror.New(http.StatusConflict, "idempotency_conflict", "idempotency key was already used for another checkout"))
	case errors.Is(err, ErrPriceNotAvailable):
		apierror.Respond(c, apierror.New(http.StatusUnprocessableEntity, "price_not_available", "no price available for this plan and currency"))
	case errors.Is(err, ErrProviderPriceNotFound):
		apierror.Respond(c, apierror.New(http.StatusServiceUnavailable, "provider_price_not_configured", "payment provider price not configured"))
	case errors.Is(err, ErrUnsupportedCurrency):
		apierror.Respond(c, apierror.New(http.StatusUnprocessableEntity, "unsupported_currency", "currency is not supported"))
	default:
		apierror.Respond(c, err)
	}
}
