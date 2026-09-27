DROP INDEX IF EXISTS guest_sessions_event_source_idx;
ALTER TABLE guest_sessions DROP COLUMN IF EXISTS qr_source_code;
DROP TABLE IF EXISTS qr_sources;
