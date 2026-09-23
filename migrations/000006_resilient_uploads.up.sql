ALTER TABLE media ADD COLUMN client_upload_id uuid;
ALTER TABLE media ADD COLUMN last_activity_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE media ADD COLUMN failed_at timestamptz;
CREATE UNIQUE INDEX media_event_session_client_upload_uq
  ON media (event_id, guest_session_id, client_upload_id)
  WHERE client_upload_id IS NOT NULL;
CREATE INDEX media_stale_upload_idx
  ON media (last_activity_at)
  WHERE status IN ('pending', 'uploading', 'uploaded');
