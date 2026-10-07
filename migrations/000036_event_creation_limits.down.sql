-- 000036_event_creation_limits.down.sql

DROP INDEX IF EXISTS events_one_active_per_host_idx;
DROP INDEX IF EXISTS events_host_created_at_idx;
DROP INDEX IF EXISTS events_host_status_idx;
