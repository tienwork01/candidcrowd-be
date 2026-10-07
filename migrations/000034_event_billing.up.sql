-- billing_purchases
CREATE TABLE billing_purchases (
    id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id                 uuid NOT NULL REFERENCES events (id),
    host_id                  uuid NOT NULL REFERENCES users (id),
    plan_version_id          uuid NOT NULL REFERENCES plan_versions (id),
    plan_code                text NOT NULL,
    quoted_amount_minor      bigint NOT NULL CHECK (quoted_amount_minor >= 0),
    quoted_currency          char(3) NOT NULL CHECK (quoted_currency = upper(quoted_currency)),
    settled_subtotal_minor   bigint CHECK (settled_subtotal_minor >= 0),
    settled_tax_minor        bigint CHECK (settled_tax_minor >= 0),
    settled_total_minor      bigint CHECK (settled_total_minor >= 0),
    settled_currency         char(3),
    status                   text NOT NULL,
    provider                 text NOT NULL,
    provider_checkout_id     text,
    provider_transaction_id  text,
    idempotency_key          text NOT NULL,
    checkout_url             text,
    failure_code             text,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    settled_at               timestamptz,
    entitlement_applied_at   timestamptz,
    UNIQUE (host_id, idempotency_key),
    UNIQUE (provider, provider_transaction_id)
);

CREATE INDEX billing_purchases_event_idx
    ON billing_purchases (event_id, created_at DESC);

CREATE INDEX billing_purchases_reconcile_idx
    ON billing_purchases (status, entitlement_applied_at)
    WHERE status = 'settled' AND entitlement_applied_at IS NULL;

-- billing_provider_prices
CREATE TABLE billing_provider_prices (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider           text NOT NULL,
    plan_version_id    uuid NOT NULL REFERENCES plan_versions (id),
    currency           char(3) NOT NULL CHECK (currency = upper(currency)),
    provider_price_id  text NOT NULL,
    active             boolean NOT NULL DEFAULT true,
    created_at         timestamptz NOT NULL DEFAULT now(),
    retired_at         timestamptz,
    UNIQUE (provider, provider_price_id)
);

CREATE UNIQUE INDEX billing_provider_prices_one_active_idx
    ON billing_provider_prices (provider, plan_version_id, currency)
    WHERE active = true;

-- billing_provider_events
CREATE TABLE billing_provider_events (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider           text NOT NULL,
    external_event_id  text NOT NULL,
    event_type         text NOT NULL,
    purchase_id        uuid REFERENCES billing_purchases (id),
    payload_hash       text NOT NULL,
    status             text NOT NULL,
    attempt_count      integer NOT NULL DEFAULT 0,
    last_error         text,
    received_at        timestamptz NOT NULL DEFAULT now(),
    processed_at       timestamptz,
    UNIQUE (provider, external_event_id)
);
