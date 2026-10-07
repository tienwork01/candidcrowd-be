DROP INDEX IF EXISTS billing_provider_prices_one_active_idx;
ALTER TABLE billing_provider_prices DROP COLUMN IF EXISTS amount_minor;
CREATE UNIQUE INDEX billing_provider_prices_one_active_idx
    ON billing_provider_prices (provider, plan_version_id, currency)
    WHERE active = true;

ALTER TABLE billing_purchases
    DROP COLUMN IF EXISTS pricing_version,
    DROP COLUMN IF EXISTS pricing_reason,
    DROP COLUMN IF EXISTS discount_amount_minor,
    DROP COLUMN IF EXISTS base_amount_minor;

DROP INDEX IF EXISTS events_trial_window_idx;
DROP INDEX IF EXISTS events_one_active_trial_per_host_idx;
ALTER TABLE events DROP COLUMN IF EXISTS trial_ended_at, DROP COLUMN IF EXISTS trial_started_at;

DROP INDEX IF EXISTS event_plan_grants_license_idx;
UPDATE event_plan_grants SET source = 'external' WHERE source IN ('purchase', 'referral', 'admin', 'business');
ALTER TABLE event_plan_grants DROP CONSTRAINT event_plan_grants_source_check;
ALTER TABLE event_plan_grants ADD CONSTRAINT event_plan_grants_source_check
    CHECK (source IN ('system', 'manual', 'migration', 'promotion', 'external'));
ALTER TABLE event_plan_grants DROP COLUMN IF EXISTS event_license_id;
DROP TABLE IF EXISTS event_licenses;
