DROP INDEX IF EXISTS billing_purchases_one_open_per_event_idx;
ALTER TABLE billing_purchases DROP COLUMN IF EXISTS provider_price_id;
