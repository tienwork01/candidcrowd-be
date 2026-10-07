-- Persist the provider price selected at checkout. Price mappings may later be
-- retired, but historical transactions must still be verifiable.
ALTER TABLE billing_purchases
    ADD COLUMN provider_price_id text;

UPDATE billing_purchases p
SET provider_price_id = pp.provider_price_id
FROM billing_provider_prices pp
WHERE pp.provider = p.provider
  AND pp.plan_version_id = p.plan_version_id
  AND pp.currency = p.quoted_currency
  AND pp.active = true
  AND p.provider_price_id IS NULL;

ALTER TABLE billing_purchases
    ALTER COLUMN provider_price_id SET NOT NULL;

-- Only one checkout can be open for an event even when requests with
-- different idempotency keys race.
CREATE UNIQUE INDEX billing_purchases_one_open_per_event_idx
    ON billing_purchases (event_id)
    WHERE status IN ('pending', 'checkout_created');
