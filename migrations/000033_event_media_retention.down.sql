DROP INDEX IF EXISTS event_plan_grants_active_retention_idx;
ALTER TABLE events DROP COLUMN IF EXISTS media_purged_at;
