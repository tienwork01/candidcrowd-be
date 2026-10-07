-- The registry of plans. Everything that used to be a literal list in Go or a
-- CHECK constraint now points here, so adding a tier is a row in this table
-- plus its catalog version and prices — no code change, no new migration.
CREATE TABLE plans (
    code        text PRIMARY KEY,
    tier_rank   integer NOT NULL CHECK (tier_rank >= 0),
    sellable    boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    retired_at  timestamptz
);

-- Ranks order upgrades, so two live plans may not share one.
CREATE UNIQUE INDEX plans_tier_rank_idx ON plans (tier_rank) WHERE retired_at IS NULL;

INSERT INTO plans (code, tier_rank, sellable) VALUES
    ('free', 0, false),
    ('experience', 1, true),
    ('signature', 2, true);

-- A frozen list of codes becomes a reference to the registry.
ALTER TABLE plan_versions DROP CONSTRAINT plan_versions_code_check;
ALTER TABLE plan_versions
    ADD CONSTRAINT plan_versions_code_fkey FOREIGN KEY (code) REFERENCES plans (code);

ALTER TABLE billing_provider_products DROP CONSTRAINT billing_provider_products_plan_code_check;
ALTER TABLE billing_provider_products
    ADD CONSTRAINT billing_provider_products_plan_code_fkey FOREIGN KEY (plan_code) REFERENCES plans (code);

-- Loyalty pricing is data as well. A new plan ships its returning price beside
-- its list price instead of being added to a map in the pricing engine.
ALTER TABLE plan_prices
    ADD COLUMN returning_amount_minor integer
    CHECK (returning_amount_minor IS NULL OR returning_amount_minor >= 0);

UPDATE plan_prices p SET returning_amount_minor = 3200
  FROM plan_versions v
 WHERE v.id = p.plan_version_id AND v.code = 'experience' AND p.currency = 'USD';

UPDATE plan_prices p SET returning_amount_minor = 5900
  FROM plan_versions v
 WHERE v.id = p.plan_version_id AND v.code = 'signature' AND p.currency = 'USD';
