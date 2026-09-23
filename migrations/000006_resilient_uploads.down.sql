DROP INDEX IF EXISTS media_stale_upload_idx;
DROP INDEX IF EXISTS media_event_session_client_upload_uq;
ALTER TABLE media DROP COLUMN IF EXISTS failed_at;
ALTER TABLE media DROP COLUMN IF EXISTS last_activity_at;
ALTER TABLE media DROP COLUMN IF EXISTS client_upload_id;
