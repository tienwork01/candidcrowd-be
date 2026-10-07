-- 000036_event_creation_limits.up.sql

CREATE INDEX IF NOT EXISTS events_host_status_idx
    ON events (host_id, status)
    WHERE status = 'active';

CREATE INDEX IF NOT EXISTS events_host_created_at_idx
    ON events (host_id, created_at DESC);

-- Trial-specific constraints are introduced in 000037. Do not enforce an
-- account-wide active event limit here: paid accounts may own several active
-- events, and a migration must never silently close customer events.
