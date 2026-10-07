-- Upgrade pricing is context-aware. A purchase records the commercial context
-- it was priced in and how much tier credit it consumed, so an upgrade of the
-- same event can be priced from what was actually sold rather than from a
-- single hardcoded difference.
ALTER TABLE billing_purchases
    ADD COLUMN pricing_context text NOT NULL DEFAULT 'first',
    ADD COLUMN upgrade_credit_minor bigint NOT NULL DEFAULT 0,
    ADD COLUMN refunded_amount_minor bigint NOT NULL DEFAULT 0,
    ADD CONSTRAINT billing_purchases_pricing_context_check
        CHECK (pricing_context IN ('first', 'returning')),
    ADD CONSTRAINT billing_purchases_upgrade_credit_check
        CHECK (upgrade_credit_minor >= 0 AND upgrade_credit_minor <= discount_amount_minor),
    ADD CONSTRAINT billing_purchases_refunded_amount_check
        CHECK (refunded_amount_minor >= 0);

-- Existing rows: the only upgrade price ever charged came from the returning
-- price list, and its whole discount was tier credit.
UPDATE billing_purchases SET pricing_context = 'returning'
 WHERE pricing_reason IN ('returning_customer', 'upgrade');
UPDATE billing_purchases SET upgrade_credit_minor = discount_amount_minor
 WHERE pricing_reason = 'upgrade';

-- Product anchors. One provider product per sellable plan lets the adapter
-- send any amount the pricing engine verified as a non-catalog price, instead
-- of pre-creating a provider price for every possible final amount.
CREATE TABLE billing_provider_products (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider             text NOT NULL,
    plan_code            text NOT NULL CHECK (plan_code IN ('experience', 'signature')),
    provider_product_id  text NOT NULL,
    active               boolean NOT NULL DEFAULT true,
    created_at           timestamptz NOT NULL DEFAULT now(),
    retired_at           timestamptz
);

CREATE UNIQUE INDEX billing_provider_products_one_active_idx
    ON billing_provider_products (provider, plan_code)
    WHERE active = true;

-- Refunds and chargebacks never delete an event or its media. They move the
-- event into a state a human resolves.
ALTER TABLE events
    ADD COLUMN billing_status text NOT NULL DEFAULT 'ok';

ALTER TABLE events
    ADD CONSTRAINT events_billing_status_check
    CHECK (billing_status IN ('ok', 'payment_review', 'refunded'));

CREATE INDEX events_billing_review_idx
    ON events (billing_status)
    WHERE billing_status <> 'ok';
