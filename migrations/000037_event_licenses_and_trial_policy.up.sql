-- Account-level, single-event licenses. These are internal billing assets;
-- consumer UI continues to sell an Experience or Signature event package.
CREATE TABLE event_licenses (
    id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id               uuid NOT NULL REFERENCES users (id),
    plan_version_id          uuid NOT NULL REFERENCES plan_versions (id),
    status                   text NOT NULL CHECK (status IN ('available', 'reserved', 'consumed', 'revoked', 'expired')),
    source                   text NOT NULL CHECK (source IN ('purchase', 'promotion', 'referral', 'admin', 'business')),
    purchase_id              uuid REFERENCES billing_purchases (id),
    payment_provider         text,
    provider_transaction_id  text,
    purchase_amount_minor    bigint CHECK (purchase_amount_minor IS NULL OR purchase_amount_minor >= 0),
    purchase_currency        char(3),
    reserved_event_id        uuid REFERENCES events (id),
    consumed_event_id        uuid REFERENCES events (id),
    created_at               timestamptz NOT NULL DEFAULT now(),
    reserved_at              timestamptz,
    consumed_at              timestamptz,
    revoked_at               timestamptz,
    expires_at               timestamptz,
    UNIQUE (purchase_id),
    CHECK (purchase_currency IS NULL OR purchase_currency = upper(purchase_currency)),
    CHECK ((status <> 'reserved') OR (reserved_event_id IS NOT NULL AND reserved_at IS NOT NULL)),
    CHECK ((status <> 'consumed') OR (consumed_event_id IS NOT NULL AND consumed_at IS NOT NULL))
);

CREATE INDEX event_licenses_available_idx
    ON event_licenses (account_id, plan_version_id, created_at)
    WHERE status = 'available';

CREATE UNIQUE INDEX event_licenses_consumed_event_idx
    ON event_licenses (consumed_event_id)
    WHERE consumed_event_id IS NOT NULL;

ALTER TABLE event_plan_grants
    ADD COLUMN event_license_id uuid REFERENCES event_licenses (id);

ALTER TABLE event_plan_grants DROP CONSTRAINT event_plan_grants_source_check;
ALTER TABLE event_plan_grants ADD CONSTRAINT event_plan_grants_source_check
    CHECK (source IN ('system', 'manual', 'migration', 'promotion', 'external', 'purchase', 'referral', 'admin', 'business'));

CREATE UNIQUE INDEX event_plan_grants_license_idx
    ON event_plan_grants (event_license_id)
    WHERE event_license_id IS NOT NULL;

ALTER TABLE events
    ADD COLUMN trial_started_at timestamptz,
    ADD COLUMN trial_ended_at timestamptz;

-- Existing events started life on the Free grant. Paid/manual historical
-- grants end the trial; deletion does not erase trial history.
UPDATE events SET trial_started_at = created_at WHERE trial_started_at IS NULL;
UPDATE events e
SET trial_ended_at = g.activated_at
FROM event_plan_grants g
JOIN plan_versions pv ON pv.id = g.plan_version_id
WHERE g.event_id = e.id
  AND pv.code <> 'free'
  AND e.trial_ended_at IS NULL;

-- The one-trial-per-host rule starts here, so accounts that already have
-- several open free events predate it. Grandfather them deterministically:
-- the newest active event keeps the open trial and the older ones are marked
-- as having used theirs. Closing or deleting an event is never an option — a
-- migration must not take an event away from anyone — and without this the
-- index below simply fails to build on any account with two free events.
UPDATE events e
SET trial_ended_at = e.created_at
WHERE e.status = 'active'
  AND e.trial_started_at IS NOT NULL
  AND e.trial_ended_at IS NULL
  AND e.id <> (
      SELECT newest.id FROM events newest
      WHERE newest.host_id = e.host_id
        AND newest.status = 'active'
        AND newest.trial_started_at IS NOT NULL
        AND newest.trial_ended_at IS NULL
      ORDER BY newest.created_at DESC, newest.id DESC
      LIMIT 1
  );

CREATE UNIQUE INDEX events_one_active_trial_per_host_idx
    ON events (host_id)
    WHERE status = 'active' AND trial_started_at IS NOT NULL AND trial_ended_at IS NULL;

CREATE INDEX events_trial_window_idx
    ON events (host_id, trial_started_at DESC)
    WHERE trial_started_at IS NOT NULL;

ALTER TABLE billing_purchases
    ADD COLUMN base_amount_minor bigint,
    ADD COLUMN discount_amount_minor bigint NOT NULL DEFAULT 0,
    ADD COLUMN pricing_reason text NOT NULL DEFAULT 'first_purchase',
    ADD COLUMN pricing_version text NOT NULL DEFAULT 'consumer-usd-2026-01';

UPDATE billing_purchases
SET base_amount_minor = quoted_amount_minor
WHERE base_amount_minor IS NULL;

ALTER TABLE billing_purchases
    ALTER COLUMN base_amount_minor SET NOT NULL,
    ADD CHECK (base_amount_minor >= 0),
    ADD CHECK (discount_amount_minor >= 0),
    ADD CHECK (quoted_amount_minor = base_amount_minor - discount_amount_minor),
    ADD CHECK (pricing_reason IN ('first_purchase', 'returning_customer', 'upgrade', 'promotion'));

ALTER TABLE billing_provider_prices
    ADD COLUMN amount_minor bigint;

UPDATE billing_provider_prices bp
SET amount_minor = (
    SELECT pp.amount_minor
    FROM plan_prices pp
    WHERE pp.plan_version_id = bp.plan_version_id
      AND pp.currency = bp.currency
    ORDER BY pp.valid_from DESC
    LIMIT 1
)
WHERE bp.amount_minor IS NULL;

ALTER TABLE billing_provider_prices
    ALTER COLUMN amount_minor SET NOT NULL,
    ADD CHECK (amount_minor >= 0);

DROP INDEX billing_provider_prices_one_active_idx;
CREATE UNIQUE INDEX billing_provider_prices_one_active_idx
    ON billing_provider_prices (provider, plan_version_id, currency, amount_minor)
    WHERE active = true;
