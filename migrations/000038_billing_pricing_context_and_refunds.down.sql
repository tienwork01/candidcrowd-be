DROP INDEX IF EXISTS events_billing_review_idx;
ALTER TABLE events DROP CONSTRAINT IF EXISTS events_billing_status_check;
ALTER TABLE events DROP COLUMN IF EXISTS billing_status;

DROP TABLE IF EXISTS billing_provider_products;

ALTER TABLE billing_purchases
    DROP CONSTRAINT IF EXISTS billing_purchases_refunded_amount_check,
    DROP CONSTRAINT IF EXISTS billing_purchases_upgrade_credit_check,
    DROP CONSTRAINT IF EXISTS billing_purchases_pricing_context_check,
    DROP COLUMN IF EXISTS refunded_amount_minor,
    DROP COLUMN IF EXISTS upgrade_credit_minor,
    DROP COLUMN IF EXISTS pricing_context;
