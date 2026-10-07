-- Destroys every grant and its audit trail. Do not run once real grants exist
-- unless they have been exported first.
DROP TABLE IF EXISTS event_plan_audits;
DROP FUNCTION IF EXISTS event_plan_audits_immutable();
DROP TABLE IF EXISTS event_plan_grants;
DROP TABLE IF EXISTS plan_prices;
DROP TABLE IF EXISTS plan_versions;
