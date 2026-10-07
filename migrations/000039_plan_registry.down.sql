ALTER TABLE plan_prices DROP COLUMN IF EXISTS returning_amount_minor;

ALTER TABLE billing_provider_products DROP CONSTRAINT IF EXISTS billing_provider_products_plan_code_fkey;
ALTER TABLE billing_provider_products
    ADD CONSTRAINT billing_provider_products_plan_code_check
    CHECK (plan_code IN ('experience', 'signature'));

ALTER TABLE plan_versions DROP CONSTRAINT IF EXISTS plan_versions_code_fkey;
ALTER TABLE plan_versions
    ADD CONSTRAINT plan_versions_code_check
    CHECK (code IN ('free', 'experience', 'signature'));

DROP TABLE IF EXISTS plans;
