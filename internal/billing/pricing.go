package billing

import (
	"context"
	"fmt"

	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
	"github.com/google/uuid"
)

type PricingReason string

const (
	PricingFirstPurchase PricingReason = "first_purchase"
	PricingReturning     PricingReason = "returning_customer"
	PricingUpgrade       PricingReason = "upgrade"
	PricingPromotion     PricingReason = "promotion"
)

// PricingContext is the commercial price list a purchase is quoted from. It is
// decided once per event: an upgrade of the same event stays in the context its
// first paid purchase was sold in, so the tier credit and the target price are
// always two numbers from the same list.
type PricingContext string

const (
	ContextFirst     PricingContext = "first"
	ContextReturning PricingContext = "returning"
)

const consumerPricingVersion = "consumer-usd-2026-02"

// Quote is the immutable backend pricing decision displayed by clients and
// snapshotted on the purchase.
//
// The arithmetic the whole system relies on is:
//
//	final = base - discount, with discount >= upgrade_credit
//
// BaseAmountMinor is the target tier's commercial value in the quote's pricing
// context, never a cash figure a customer happened to pay.
type Quote struct {
	EventID             uuid.UUID        `json:"event_id"`
	PlanVersionID       uuid.UUID        `json:"plan_version_id"`
	CurrentPlanCode     catalog.PlanCode `json:"current_plan"`
	PlanCode            catalog.PlanCode `json:"plan_code"`
	BaseAmountMinor     int64            `json:"base_amount_minor"`
	UpgradeCreditMinor  int64            `json:"upgrade_credit_minor"`
	DiscountAmountMinor int64            `json:"discount_amount_minor"`
	FinalAmountMinor    int64            `json:"final_amount_minor"`
	Currency            string           `json:"currency"`
	Context             PricingContext   `json:"pricing_context"`
	Reason              PricingReason    `json:"reason"`
	PricingVersion      string           `json:"pricing_version"`
}

// tierValue is a tier's commercial value in one pricing context, read from the
// catalog. A tier with no published loyalty price simply keeps its list price
// in both contexts, so adding a plan never needs a change here.
func tierValue(pctx PricingContext, price CatalogPrice) int64 {
	if pctx == ContextReturning && price.ReturningAmountMinor != nil {
		return *price.ReturningAmountMinor
	}
	return price.AmountMinor
}

// ApplyPercentagePromotion changes the amount, never the tier or plan version.
// A coupon is a discount on this purchase: it lowers what is charged now and
// leaves BaseAmountMinor — the tier's commercial value, and therefore the
// credit a later upgrade is given — untouched.
func ApplyPercentagePromotion(q Quote, percent int) (Quote, error) {
	if percent < 0 || percent > 100 {
		return Quote{}, fmt.Errorf("billing: promotion percentage must be between 0 and 100")
	}
	discount := q.FinalAmountMinor * int64(percent) / 100
	q.DiscountAmountMinor += discount
	q.FinalAmountMinor -= discount
	q.Reason = PricingPromotion
	return q, nil
}

// QuoteEventPurchase is the only authority for consumer event pricing.
//
// Two cases, and they are deliberately not the same case:
//
//   - Buying a plan for an event that has none. Priced from the first-time list,
//     or from the loyalty list when the host already completed a paid purchase
//     on another event.
//   - Upgrading the event's existing paid tier. Priced inside the context that
//     event was sold in, crediting the commercial value of the tier it holds.
//     Loyalty pricing earned by this very purchase does not reprice it.
func (s *Service) QuoteEventPurchase(ctx context.Context, eventID, hostID uuid.UUID, target catalog.PlanCode, currency string) (Quote, error) {
	if currency != s.currency {
		return Quote{}, fmt.Errorf("%w: %s", ErrUnsupportedCurrency, currency)
	}
	if !target.Sellable() {
		return Quote{}, fmt.Errorf("%w: cannot purchase %s", ErrInvalidPlanTransition, target)
	}
	if err := s.ownership.OwnerOf(ctx, eventID, hostID); err != nil {
		return Quote{}, ErrPurchaseNotFound
	}
	current, err := s.entSvc.Resolve(ctx, eventID)
	if err != nil {
		return Quote{}, fmt.Errorf("billing: resolve entitlement: %w", err)
	}
	if err := entitlement.CheckTransition(&current.PlanCode, target); err != nil {
		return Quote{}, fmt.Errorf("%w: %s -> %s", ErrInvalidPlanTransition, current.PlanCode, target)
	}
	version, err := s.plans.ActiveVersion(ctx, string(target))
	if err != nil {
		return Quote{}, fmt.Errorf("billing: resolve plan version: %w", err)
	}
	price, err := s.plans.CurrentPrice(ctx, version.ID, currency)
	if err != nil {
		return Quote{}, fmt.Errorf("billing: resolve catalog price: %w", err)
	}

	q := Quote{
		EventID:         eventID,
		PlanVersionID:   version.ID,
		CurrentPlanCode: current.PlanCode,
		PlanCode:        target,
		Currency:        price.Currency,
		PricingVersion:  consumerPricingVersion,
	}

	// A tier nobody can buy — free today, a trial tomorrow — means this is a
	// first purchase for the event rather than an upgrade of something sold.
	if !current.PlanCode.Sellable() {
		if err := s.quoteNewPurchase(ctx, hostID, eventID, price, &q); err != nil {
			return Quote{}, err
		}
		return q, nil
	}
	if err := s.quoteUpgrade(ctx, hostID, eventID, current.PlanCode, currency, price, &q); err != nil {
		return Quote{}, err
	}
	return q, nil
}

// quoteNewPurchase prices the first paid plan on an event. Loyalty pricing is
// earned by an eligible paid purchase on a *different* event, which is what
// keeps it a reward for coming back rather than a discount on the purchase
// that just unlocked it.
func (s *Service) quoteNewPurchase(ctx context.Context, hostID, eventID uuid.UUID, price CatalogPrice, q *Quote) error {
	returning, err := s.repo.HasEligibleCommercialPurchase(ctx, hostID, eventID)
	if err != nil {
		return fmt.Errorf("billing: returning eligibility: %w", err)
	}
	q.Context, q.Reason = ContextFirst, PricingFirstPurchase
	if returning {
		q.Context, q.Reason = ContextReturning, PricingReturning
	}
	q.BaseAmountMinor = tierValue(q.Context, price)
	q.FinalAmountMinor = q.BaseAmountMinor
	return nil
}

// quoteUpgrade prices a move between paid tiers on one event.
//
// Credit is the commercial value of the tier the event already holds, read from
// the purchase that funded it — its base amount, before any coupon. A coupon
// discounts a purchase; it does not make the tier worth less when the host
// upgrades, and it cannot be farmed into a larger upgrade credit.
func (s *Service) quoteUpgrade(ctx context.Context, hostID, eventID uuid.UUID, current catalog.PlanCode, currency string, price CatalogPrice, q *Quote) error {
	funding, err := s.repo.LatestEventPurchase(ctx, eventID)
	if err != nil {
		return fmt.Errorf("billing: load funding purchase: %w", err)
	}

	var credit int64
	switch {
	case funding != nil && funding.Status.Paid():
		q.Context = funding.PricingContext
		credit = funding.BaseAmountMinor
	case funding != nil:
		// Refunded or disputed: the money went back, so the tier carries no
		// credit. The grant stays — billing never takes an event away.
		q.Context = funding.PricingContext
	default:
		// Comped, migrated or manually granted. Nothing was paid, but the event
		// does hold the tier, so credit its value rather than charging for it
		// twice.
		q.Context = ContextFirst
		returning, eligErr := s.repo.HasEligibleCommercialPurchase(ctx, hostID, eventID)
		if eligErr != nil {
			return fmt.Errorf("billing: returning eligibility: %w", eligErr)
		}
		if returning {
			q.Context = ContextReturning
		}
		held, listErr := s.tierPrice(ctx, current, currency)
		if listErr != nil {
			return listErr
		}
		credit = tierValue(q.Context, held)
	}

	q.BaseAmountMinor = tierValue(q.Context, price)
	if credit > q.BaseAmountMinor {
		credit = q.BaseAmountMinor
	}
	q.UpgradeCreditMinor = credit
	q.DiscountAmountMinor = credit
	q.FinalAmountMinor = q.BaseAmountMinor - credit
	q.Reason = PricingUpgrade
	return nil
}

// tierPrice is the catalog price of a tier the event already holds.
func (s *Service) tierPrice(ctx context.Context, plan catalog.PlanCode, currency string) (CatalogPrice, error) {
	version, err := s.plans.ActiveVersion(ctx, string(plan))
	if err != nil {
		return CatalogPrice{}, fmt.Errorf("billing: resolve held plan version: %w", err)
	}
	price, err := s.plans.CurrentPrice(ctx, version.ID, currency)
	if err != nil {
		return CatalogPrice{}, fmt.Errorf("billing: resolve held plan price: %w", err)
	}
	return price, nil
}
