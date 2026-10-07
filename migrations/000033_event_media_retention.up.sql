-- Set once an event's media has been permanently removed at the end of its
-- plan's storage period plus the deletion grace period.
ALTER TABLE events ADD COLUMN media_purged_at timestamptz;

-- The retention job looks events up by when their active grant's storage
-- period ends.
CREATE INDEX event_plan_grants_active_retention_idx
    ON event_plan_grants (retention_expires_at)
    WHERE status = 'active';
