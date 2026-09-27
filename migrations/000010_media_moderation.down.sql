DROP INDEX IF EXISTS media_event_status_created_at_idx;

-- PostgreSQL enum values cannot be removed safely. The status values remain
-- available if this migration is rolled back.
