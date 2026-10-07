package billing

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
	"github.com/google/uuid"
)

// Errors returned by billing service operations.
var (
	ErrBillingDisabled          = errors.New("billing: billing is disabled")
	ErrInvalidPlanTransition    = errors.New("billing: invalid plan transition")
	ErrPriceNotAvailable        = errors.New("billing: price not available for plan and currency")
	ErrProviderPriceNotFound    = errors.New("billing: provider price mapping not found")
	ErrProviderProductNotFound  = errors.New("billing: provider product mapping not found")
	ErrPurchaseConflict         = errors.New("billing: an open purchase already exists for this event")
	ErrIdempotencyConflict      = errors.New("billing: idempotency key was already used for a different checkout")
	ErrPurchaseNotFound         = errors.New("billing: purchase not found")
	ErrPurchaseAlreadySettled   = errors.New("billing: purchase already settled")
	ErrPaymentIntegrityMismatch = errors.New("billing: payment integrity check failed")
	ErrUnsupportedCurrency      = errors.New("billing: unsupported currency")
)

// EntitlementReader is used to resolve an event's current plan for transition validation.
type EntitlementReader interface {
	Resolve(ctx context.Context, eventID uuid.UUID) (entitlement.EventEntitlement, error)
}

// Service orchestrates billing use cases without knowing which payment
// provider is active.
type Service struct {
	repo       Repository
	checkout   CheckoutGateway
	decoder    WebhookDecoder
	grants     GrantActivator
	licenses   LicenseRevoker
	plans      CatalogReader
	ownership  EventAuthorizer
	entSvc     EntitlementReader
	provider   Provider
	currency   string
	enabled    bool
	successURL string
	cancelURL  string
	log        *slog.Logger
	now        func() time.Time
}

// Config wires the service at composition time.
type Config struct {
	Repo       Repository
	Checkout   CheckoutGateway
	Decoder    WebhookDecoder
	Grants     GrantActivator
	Licenses   LicenseRevoker
	Plans      CatalogReader
	Ownership  EventAuthorizer
	EntSvc     EntitlementReader
	Provider   Provider
	Currency   string
	Enabled    bool
	SuccessURL string
	CancelURL  string
	Logger     *slog.Logger
}

func NewService(cfg Config) *Service {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		repo:       cfg.Repo,
		checkout:   cfg.Checkout,
		decoder:    cfg.Decoder,
		grants:     cfg.Grants,
		licenses:   cfg.Licenses,
		plans:      cfg.Plans,
		ownership:  cfg.Ownership,
		entSvc:     cfg.EntSvc,
		provider:   cfg.Provider,
		currency:   cfg.Currency,
		enabled:    cfg.Enabled,
		successURL: cfg.SuccessURL,
		cancelURL:  cfg.CancelURL,
		log:        log,
		now:        time.Now,
	}
}

// Enabled reports whether billing is switched on for this deployment.
func (s *Service) Enabled() bool { return s.enabled }

// --- Create Checkout Use Case ---

// CreateCheckoutCommand is what the handler sends after parsing the request.
type CreateCheckoutCommand struct {
	EventID        uuid.UUID
	HostID         uuid.UUID
	PlanCode       catalog.PlanCode
	Currency       string
	IdempotencyKey string
}

// CreateCheckoutOutput is the response to the handler.
type CreateCheckoutOutput struct {
	PurchaseID        uuid.UUID
	Status            PurchaseStatus
	CheckoutSessionID string
}

// CreateCheckout implements the checkout creation flow per plan section 8.1.
func (s *Service) CreateCheckout(ctx context.Context, in CreateCheckoutCommand) (CreateCheckoutOutput, error) {
	if !s.enabled {
		return CreateCheckoutOutput{}, ErrBillingDisabled
	}
	if in.Currency != s.currency {
		return CreateCheckoutOutput{}, fmt.Errorf("%w: %s", ErrUnsupportedCurrency, in.Currency)
	}

	// 1. Idempotency: return existing purchase if same key was used.
	existing, err := s.repo.IdempotentPurchase(ctx, in.HostID, in.IdempotencyKey)
	if err != nil {
		return CreateCheckoutOutput{}, fmt.Errorf("billing: idempotency check: %w", err)
	}
	if existing != nil {
		if existing.EventID != in.EventID || existing.PlanCode != in.PlanCode || existing.QuotedCurrency != in.Currency {
			return CreateCheckoutOutput{}, ErrIdempotencyConflict
		}
		// Same key: return existing purchase without modification.
		sessionID := ""
		if existing.ProviderCheckoutID != nil {
			sessionID = *existing.ProviderCheckoutID
		}
		return CreateCheckoutOutput{
			PurchaseID:        existing.ID,
			Status:            existing.Status,
			CheckoutSessionID: sessionID,
		}, nil
	}

	// 2. Authorization, transition validation and pricing are one backend
	// decision. The browser never supplies an amount or provider price ID.
	quote, err := s.QuoteEventPurchase(ctx, in.EventID, in.HostID, in.PlanCode, in.Currency)
	if err != nil {
		return CreateCheckoutOutput{}, err
	}
	// 5. Resolve how the provider will represent this amount. A fixed price
	// mapping is used when one exists for exactly this total; otherwise the
	// quote is sent as a non-catalog price under the plan's product, so the
	// provider catalog never has to enumerate every discounted total.
	execution, err := s.resolveExecution(ctx, quote)
	if err != nil {
		return CreateCheckoutOutput{}, err
	}

	// 6. Check no open purchase exists.
	open, err := s.repo.OpenPurchaseForEvent(ctx, in.EventID)
	if err != nil {
		return CreateCheckoutOutput{}, fmt.Errorf("billing: check open purchase: %w", err)
	}
	if open != nil {
		// Return the existing open purchase.
		sessionID := ""
		if open.ProviderCheckoutID != nil {
			sessionID = *open.ProviderCheckoutID
		}
		return CreateCheckoutOutput{
			PurchaseID:        open.ID,
			Status:            open.Status,
			CheckoutSessionID: sessionID,
		}, nil
	}

	// 7. Create purchase record with pending status.
	nowTime := s.now()
	purchase := Purchase{
		ID:                  uuid.New(),
		EventID:             in.EventID,
		HostID:              in.HostID,
		PlanCode:            in.PlanCode,
		PlanVersionID:       quote.PlanVersionID,
		QuotedAmountMinor:   quote.FinalAmountMinor,
		BaseAmountMinor:     quote.BaseAmountMinor,
		DiscountAmountMinor: quote.DiscountAmountMinor,
		UpgradeCreditMinor:  quote.UpgradeCreditMinor,
		PricingReason:       quote.Reason,
		PricingContext:      quote.Context,
		PricingVersion:      quote.PricingVersion,
		QuotedCurrency:      quote.Currency,
		Status:              StatusPending,
		Provider:            s.provider,
		// Empty for a non-catalog price: the provider assigns the price ID when
		// it creates the transaction, and step 10 snapshots it.
		ProviderPriceID: execution.PriceID,
		IdempotencyKey:  in.IdempotencyKey,
		CreatedAt:       nowTime,
		UpdatedAt:       nowTime,
	}

	purchase, err = s.repo.CreatePurchase(ctx, purchase)
	if err != nil {
		// The database enforces one open purchase per event. A concurrent
		// request may win after our earlier read; return its checkout when it
		// is ready instead of ever creating a second provider transaction.
		open, lookupErr := s.repo.OpenPurchaseForEvent(ctx, in.EventID)
		if lookupErr == nil && open != nil && open.ProviderCheckoutID != nil {
			return CreateCheckoutOutput{
				PurchaseID:        open.ID,
				Status:            open.Status,
				CheckoutSessionID: *open.ProviderCheckoutID,
			}, nil
		}
		if lookupErr == nil && open != nil {
			return CreateCheckoutOutput{}, ErrPurchaseConflict
		}
		return CreateCheckoutOutput{}, fmt.Errorf("billing: create purchase: %w", err)
	}

	// 8. Get host email for checkout.
	email, err := s.ownership.HostEmail(ctx, in.HostID)
	if err != nil {
		s.log.WarnContext(ctx, "billing.checkout.email_lookup_failed", "host_id", in.HostID, "error", err)
		email = "" // Proceed without email; provider may still work.
	}

	// 9. Call provider outside DB transaction.
	target, err := s.checkout.CreateCheckout(ctx, CreateCheckoutInput{
		PurchaseID:        purchase.ID,
		EventID:           in.EventID,
		PlanCode:          string(in.PlanCode),
		Description:       execution.Description,
		AmountMinor:       quote.FinalAmountMinor,
		Currency:          quote.Currency,
		ProviderPriceID:   execution.PriceID,
		ProviderProductID: execution.ProductID,
		CustomerEmail:     email,
		SuccessURL:        s.successURL,
		CancelURL:         s.cancelURL,
	})
	if err != nil {
		s.log.ErrorContext(ctx, "billing.checkout.provider_failed",
			"purchase_id", purchase.ID, "provider", s.provider, "error", err)
		_ = s.repo.UpdateStatus(ctx, purchase.ID, StatusFailed, strPtr("provider_error"))
		return CreateCheckoutOutput{}, fmt.Errorf("billing: provider checkout: %w", err)
	}

	// 10. Persist checkout result, including the price the provider will charge.
	// Settlement is verified against this snapshot, never against a mapping that
	// may have been retired in the meantime.
	priceID := target.ProviderPriceID
	if priceID == "" {
		priceID = execution.PriceID
	}
	if err := s.repo.UpdateCheckout(ctx, purchase.ID, target.ExternalCheckoutID, priceID, ""); err != nil {
		s.log.ErrorContext(ctx, "billing.checkout.persist_failed",
			"purchase_id", purchase.ID, "error", err)
		// Purchase exists, checkout was created at provider. Reconciliation can
		// recover this state.
		return CreateCheckoutOutput{}, fmt.Errorf("billing: persist checkout: %w", err)
	}

	s.log.InfoContext(ctx, "billing.checkout.created",
		"purchase_id", purchase.ID, "event_id", in.EventID,
		"plan", in.PlanCode, "provider", s.provider)

	return CreateCheckoutOutput{
		PurchaseID:        purchase.ID,
		Status:            StatusCheckoutCreated,
		CheckoutSessionID: target.ExternalCheckoutID,
	}, nil
}

// checkoutExecution is how one verified quote is handed to the provider. It is
// the only place a provider representation is chosen, and it never changes an
// amount.
type checkoutExecution struct {
	PriceID     string
	ProductID   string
	Description string
}

func (s *Service) resolveExecution(ctx context.Context, quote Quote) (checkoutExecution, error) {
	exec := checkoutExecution{
		Description: fmt.Sprintf("%s event package", quote.PlanCode),
	}
	price, err := s.repo.FindProviderPrice(ctx, s.provider, quote.PlanVersionID, quote.Currency, quote.FinalAmountMinor)
	if err == nil {
		exec.PriceID = price.ProviderPriceID
		return exec, nil
	}
	if !errors.Is(err, ErrProviderPriceNotFound) {
		return checkoutExecution{}, fmt.Errorf("billing: resolve provider price: %w", err)
	}

	product, err := s.repo.FindProviderProduct(ctx, s.provider, string(quote.PlanCode))
	if err != nil {
		return checkoutExecution{}, fmt.Errorf("%w: no price mapping and no product for %s: %v",
			ErrProviderPriceNotFound, quote.PlanCode, err)
	}
	exec.ProductID = product.ProviderProductID
	return exec, nil
}

// ExecutionGaps lists the sellable plans that cannot be charged for, because
// no provider product anchors them. A fixed price mapping is not enough: it
// covers one amount, while a coupon, promotion or upgrade credit produces
// amounts no catalog price will ever match.
//
// It is checked at startup so a missing mapping is a boot-time log line rather
// than the first host discovering it at the checkout button.
func (s *Service) ExecutionGaps(ctx context.Context) []catalog.PlanCode {
	var gaps []catalog.PlanCode
	for _, plan := range catalog.SellablePlans() {
		if _, err := s.repo.FindProviderProduct(ctx, s.provider, string(plan)); err != nil {
			gaps = append(gaps, plan)
		}
	}
	return gaps
}

// --- Webhook Handler Use Case ---

// HandleProviderEvent processes a verified canonical event from any provider.
// It follows the settlement flow in plan section 8.2.
func (s *Service) HandleProviderEvent(ctx context.Context, rawBody []byte, signature string) error {
	// 1. Decode and verify.
	ev, err := s.decoder.DecodeWebhook(rawBody, signature)
	if err != nil {
		return fmt.Errorf("billing: decode webhook: %w", err)
	}

	// 2. Resolve which purchase this is about. A provider does not necessarily
	// echo the checkout's custom data on later notifications (Paddle's
	// adjustments do not), so fall back to the transaction it references.
	purchaseID := ev.PurchaseID
	if purchaseID == uuid.Nil && ev.ExternalTransactionID != "" {
		found, lookupErr := s.repo.PurchaseByProviderTransaction(ctx, s.provider, ev.ExternalTransactionID)
		if lookupErr != nil {
			return fmt.Errorf("billing: resolve purchase by transaction: %w", lookupErr)
		}
		if found != nil {
			purchaseID = found.ID
		}
	}

	// 3. Deduplicate on what happened rather than on the notification, so a
	// refund announced as both created and updated is applied once.
	dedupeKey := ev.DedupeKey
	if dedupeKey == "" {
		dedupeKey = ev.ExternalEventID
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(rawBody))
	rec := ProviderEventRecord{
		// A stable record ID lets a verified provider retry reclaim a failed
		// delivery without creating another event row.
		ID:              uuid.NewSHA1(uuid.NameSpaceURL, []byte(string(s.provider)+":"+dedupeKey)),
		Provider:        s.provider,
		ExternalEventID: dedupeKey,
		EventType:       string(ev.Type),
		PayloadHash:     hash,
		Status:          "received",
		ReceivedAt:      s.now(),
	}
	if purchaseID != uuid.Nil {
		rec.PurchaseID = &purchaseID
	}

	dup, err := s.repo.InsertProviderEvent(ctx, rec)
	if err != nil {
		return fmt.Errorf("billing: insert provider event: %w", err)
	}
	if dup {
		s.log.InfoContext(ctx, "billing.webhook.duplicate",
			"external_event_id", ev.ExternalEventID, "type", ev.Type)
		return nil // Already processed.
	}

	s.log.InfoContext(ctx, "billing.webhook.received",
		"external_event_id", ev.ExternalEventID, "type", ev.Type,
		"purchase_id", purchaseID)

	if ev.Type == EventIgnored {
		_ = s.repo.MarkProviderEventProcessed(ctx, rec.ID)
		return nil
	}
	if purchaseID == uuid.Nil {
		// Verified, but about a transaction this system did not create, or one
		// whose checkout row is not visible yet. Failing keeps the provider
		// retrying, and the record above is reclaimed on that retry.
		err = fmt.Errorf("billing: no purchase for provider transaction %q", ev.ExternalTransactionID)
		_ = s.repo.MarkProviderEventFailed(ctx, rec.ID, err.Error())
		s.log.ErrorContext(ctx, "billing.webhook.unmatched",
			"external_event_id", ev.ExternalEventID, "type", ev.Type,
			"transaction_id", ev.ExternalTransactionID)
		return err
	}

	// 4. Route by event type. Every handler validates the transition itself:
	// notifications arrive duplicated, delayed and out of order.
	switch ev.Type {
	case EventPaymentSettled:
		err = s.handleSettlement(ctx, purchaseID, ev)
	case EventPaymentFailed:
		err = s.handleFailure(ctx, purchaseID, ev, StatusFailed)
	case EventPaymentCanceled:
		err = s.handleFailure(ctx, purchaseID, ev, StatusCanceled)
	case EventPaymentRefunded:
		err = s.handleReversal(ctx, purchaseID, ev, false)
	case EventPaymentDisputed:
		err = s.handleReversal(ctx, purchaseID, ev, true)
	case EventPaymentDisputeReversed:
		err = s.handleDisputeReversed(ctx, purchaseID, ev)
	default:
		s.log.WarnContext(ctx, "billing.webhook.unknown_type",
			"type", ev.Type, "external_event_id", ev.ExternalEventID)
		_ = s.repo.MarkProviderEventProcessed(ctx, rec.ID)
		return nil
	}

	if err != nil {
		errMsg := err.Error()
		_ = s.repo.MarkProviderEventFailed(ctx, rec.ID, errMsg)
		return err
	}
	_ = s.repo.MarkProviderEventProcessed(ctx, rec.ID)
	return nil
}

func (s *Service) handleSettlement(ctx context.Context, purchaseID uuid.UUID, ev ProviderEvent) error {
	purchase, err := s.repo.GetPurchase(ctx, purchaseID)
	if err != nil {
		return fmt.Errorf("billing: load purchase for settlement: %w", err)
	}

	// A settled purchase re-notified is not an error. Re-running the grant is
	// how a delivery that settled but failed to apply its entitlement repairs
	// itself without waiting for reconciliation.
	if purchase.Status == StatusSettled {
		s.log.InfoContext(ctx, "billing.purchase.already_settled", "purchase_id", purchase.ID)
		return s.applyEntitlement(ctx, purchase, ev.ExternalTransactionID, ev.OccurredAt)
	}
	// Money already went back. A late or replayed completion must not resurrect
	// the payment.
	if purchase.Status.PostSettlement() {
		s.log.WarnContext(ctx, "billing.webhook.settlement_after_reversal",
			"purchase_id", purchase.ID, "status", purchase.Status)
		return nil
	}
	if purchase.Status == StatusFailed || purchase.Status == StatusCanceled {
		// Out of order: a failed attempt followed by a successful one. The
		// completion is the truth about money, so it wins.
		s.log.InfoContext(ctx, "billing.webhook.settlement_after_failure",
			"purchase_id", purchase.ID, "previous_status", purchase.Status)
	}

	// The mapping can be retired after checkout creation. Verify against the
	// price snapshot captured on the purchase, never the current mapping. The
	// snapshot is empty only when the provider never reported which price it
	// created, in which case amount and currency carry the check alone.
	if purchase.ProviderPriceID != "" && ev.PriceID != purchase.ProviderPriceID {
		s.log.ErrorContext(ctx, "billing.integrity_mismatch",
			"purchase_id", purchase.ID,
			"expected_price", purchase.ProviderPriceID,
			"received_price", ev.PriceID)
		return fmt.Errorf("%w: price ID mismatch", ErrPaymentIntegrityMismatch)
	}
	if ev.Subtotal.Currency != purchase.QuotedCurrency || ev.Total.Currency != purchase.QuotedCurrency || ev.Subtotal.AmountMinor != purchase.QuotedAmountMinor {
		return fmt.Errorf("%w: amount or currency mismatch", ErrPaymentIntegrityMismatch)
	}

	// Settle purchase.
	if err := s.repo.SettlePurchase(ctx, SettleInput{
		PurchaseID:            purchase.ID,
		ProviderTransactionID: ev.ExternalTransactionID,
		SettledSubtotalMinor:  ev.Subtotal.AmountMinor,
		SettledTaxMinor:       ev.Tax.AmountMinor,
		SettledTotalMinor:     ev.Total.AmountMinor,
		SettledCurrency:       ev.Total.Currency,
	}); err != nil && !errors.Is(err, ErrPurchaseAlreadySettled) {
		return fmt.Errorf("billing: settle purchase: %w", err)
	}

	s.log.InfoContext(ctx, "billing.purchase.settled",
		"purchase_id", purchase.ID, "event_id", purchase.EventID,
		"plan", purchase.PlanCode, "total", ev.Total.AmountMinor)

	purchase.SettledTotalMinor = &ev.Total.AmountMinor
	purchase.SettledCurrency = &ev.Total.Currency
	return s.applyEntitlement(ctx, purchase, ev.ExternalTransactionID, ev.OccurredAt)
}

// applyEntitlement issues the license and grant for a settled purchase. It is
// safe to call repeatedly: the entitlement side is keyed on the purchase, and a
// purchase that already applied is skipped.
func (s *Service) applyEntitlement(ctx context.Context, purchase Purchase, txnID string, occurredAt time.Time) error {
	if purchase.EntitlementAppliedAt != nil {
		return nil
	}
	settledAt := occurredAt
	if settledAt.IsZero() {
		settledAt = s.now()
	}
	if txnID == "" && purchase.ProviderTransactionID != nil {
		txnID = *purchase.ProviderTransactionID
	}

	grantErr := s.grants.ActivateForPurchase(ctx, GrantInput{
		EventID:               purchase.EventID,
		AccountID:             purchase.HostID,
		PlanVersionID:         purchase.PlanVersionID,
		PlanCode:              string(purchase.PlanCode),
		PurchaseID:            purchase.ID,
		PaymentProvider:       string(purchase.Provider),
		ProviderTransactionID: txnID,
		PurchaseAmountMinor:   valueOrZero(purchase.SettledTotalMinor),
		PurchaseCurrency:      valueOrEmpty(purchase.SettledCurrency),
		SettledAt:             settledAt,
	})
	if grantErr != nil {
		if errors.Is(grantErr, entitlement.ErrDuplicateSourceReference) {
			// Already granted by a previous attempt.
			s.log.InfoContext(ctx, "billing.entitlement.already_applied", "purchase_id", purchase.ID)
		} else {
			s.log.ErrorContext(ctx, "billing.entitlement.grant_failed",
				"purchase_id", purchase.ID, "error", grantErr)
			return fmt.Errorf("billing: grant entitlement: %w", grantErr)
		}
	}

	if err := s.repo.MarkEntitlementApplied(ctx, purchase.ID); err != nil {
		s.log.ErrorContext(ctx, "billing.entitlement.mark_applied_failed",
			"purchase_id", purchase.ID, "error", err)
		// Non-fatal: reconciliation will pick it up.
	}

	s.log.InfoContext(ctx, "billing.entitlement.applied",
		"purchase_id", purchase.ID, "event_id", purchase.EventID, "plan", purchase.PlanCode)
	return nil
}

func (s *Service) handleFailure(ctx context.Context, purchaseID uuid.UUID, ev ProviderEvent, status PurchaseStatus) error {
	purchase, err := s.repo.GetPurchase(ctx, purchaseID)
	if err != nil {
		return fmt.Errorf("billing: load purchase for failure: %w", err)
	}
	if purchase.Status.PostSettlement() {
		s.log.WarnContext(ctx, "billing.webhook.failure_after_settlement",
			"purchase_id", purchase.ID, "event_type", ev.Type, "status", purchase.Status)
		return nil
	}
	if purchase.Status == status {
		return nil
	}
	return s.repo.UpdateStatus(ctx, purchase.ID, status, nil)
}

// handleReversal applies a refund or a chargeback.
//
// The policy is deliberately asymmetric. Entitlement is unwound only where
// nothing was consumed; once an event is live on the plan it bought, the grant
// stays and a human decides. Billing never deletes an event or its media: a
// wedding gallery does not disappear because a card was charged back.
func (s *Service) handleReversal(ctx context.Context, purchaseID uuid.UUID, ev ProviderEvent, dispute bool) error {
	purchase, err := s.repo.GetPurchase(ctx, purchaseID)
	if err != nil {
		return fmt.Errorf("billing: load purchase for reversal: %w", err)
	}
	if !purchase.Status.PostSettlement() {
		// A reversal of something never recorded as settled. Nothing was
		// granted, so there is nothing to unwind; close the open checkout.
		s.log.WarnContext(ctx, "billing.webhook.reversal_before_settlement",
			"purchase_id", purchase.ID, "status", purchase.Status, "event_type", ev.Type)
		if purchase.Status.Open() {
			return s.repo.UpdateStatus(ctx, purchase.ID, StatusCanceled, strPtr("reversed_before_settlement"))
		}
		return nil
	}

	settledTotal := valueOrZero(purchase.SettledTotalMinor)
	returned := ev.Adjustment.AmountMinor
	if returned <= 0 {
		returned = settledTotal // A reversal without an amount is a full one.
	}
	cumulative := purchase.RefundedAmountMinor + returned
	if settledTotal > 0 && cumulative > settledTotal {
		cumulative = settledTotal
	}

	status := StatusPartiallyRefunded
	switch {
	case dispute:
		status = StatusDisputed
	case settledTotal <= 0 || cumulative >= settledTotal:
		status = StatusRefunded
	}

	if err := s.repo.RecordReversal(ctx, ReversalInput{
		PurchaseID:          purchase.ID,
		Status:              status,
		RefundedAmountMinor: cumulative,
	}); err != nil {
		return fmt.Errorf("billing: record reversal: %w", err)
	}

	s.log.WarnContext(ctx, "billing.purchase."+string(status),
		"purchase_id", purchase.ID, "event_id", purchase.EventID,
		"plan", purchase.PlanCode, "provider", s.provider,
		"returned_minor", returned, "refunded_total_minor", cumulative)

	if s.licenses == nil {
		return nil
	}
	revoked, err := s.licenses.RevokeUnconsumedLicense(ctx, purchase.ID)
	if err != nil {
		return fmt.Errorf("billing: revoke license: %w", err)
	}
	if revoked {
		// Nothing was activated with it, so nothing is taken away.
		s.log.InfoContext(ctx, "billing.license.revoked", "purchase_id", purchase.ID)
		return nil
	}

	// The license is consumed: the event is live on the plan it bought. Flag it
	// and leave every grant, limit and media file alone.
	eventStatus := EventBillingRefunded
	switch {
	case dispute:
		eventStatus = EventBillingPaymentReview
	case status == StatusPartiallyRefunded:
		// A goodwill partial refund is not a reason to flag an event.
		return nil
	}
	if err := s.licenses.SetEventBillingStatus(ctx, purchase.EventID, eventStatus); err != nil {
		return fmt.Errorf("billing: flag event for review: %w", err)
	}
	s.log.WarnContext(ctx, "billing.event.flagged",
		"event_id", purchase.EventID, "billing_status", eventStatus, "purchase_id", purchase.ID)
	return nil
}

// handleDisputeReversed restores a chargeback that was decided in our favour.
func (s *Service) handleDisputeReversed(ctx context.Context, purchaseID uuid.UUID, ev ProviderEvent) error {
	purchase, err := s.repo.GetPurchase(ctx, purchaseID)
	if err != nil {
		return fmt.Errorf("billing: load purchase for dispute reversal: %w", err)
	}
	if purchase.Status != StatusDisputed {
		s.log.WarnContext(ctx, "billing.webhook.dispute_reversal_without_dispute",
			"purchase_id", purchase.ID, "status", purchase.Status)
		return nil
	}
	remaining := purchase.RefundedAmountMinor - ev.Adjustment.AmountMinor
	if ev.Adjustment.AmountMinor <= 0 || remaining < 0 {
		remaining = 0
	}
	if err := s.repo.RecordReversal(ctx, ReversalInput{
		PurchaseID:          purchase.ID,
		Status:              StatusSettled,
		RefundedAmountMinor: remaining,
	}); err != nil {
		return fmt.Errorf("billing: record dispute reversal: %w", err)
	}
	s.log.InfoContext(ctx, "billing.purchase.dispute_reversed", "purchase_id", purchase.ID)
	if s.licenses == nil {
		return nil
	}
	return s.licenses.SetEventBillingStatus(ctx, purchase.EventID, EventBillingOK)
}

// GetPurchase returns a purchase if it belongs to the given host.
func (s *Service) GetPurchase(ctx context.Context, purchaseID, hostID uuid.UUID) (Purchase, error) {
	p, err := s.repo.GetPurchase(ctx, purchaseID)
	if err != nil {
		return Purchase{}, ErrPurchaseNotFound
	}
	if p.HostID != hostID {
		return Purchase{}, ErrPurchaseNotFound
	}
	return p, nil
}

// ReconcileUnapplied finds settled purchases that haven't had their grant
// applied yet and retries them. It shares applyEntitlement with the webhook
// path, so running it repeatedly converges on exactly one license and one
// grant per purchase no matter how many times it runs.
func (s *Service) ReconcileUnapplied(ctx context.Context, limit int) (applied int, errs []error) {
	purchases, err := s.repo.UnappliedSettled(ctx, limit)
	if err != nil {
		return 0, []error{err}
	}
	for _, p := range purchases {
		settledAt := s.now()
		if p.SettledAt != nil {
			settledAt = *p.SettledAt
		}
		if err := s.applyEntitlement(ctx, p, "", settledAt); err != nil {
			errs = append(errs, fmt.Errorf("purchase %s: %w", p.ID, err))
			continue
		}
		applied++
		s.log.InfoContext(ctx, "billing.reconciliation.applied",
			"purchase_id", p.ID, "event_id", p.EventID, "plan", p.PlanCode)
	}
	return applied, errs
}

func strPtr(s string) *string { return &s }

func valueOrZero(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func valueOrEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
