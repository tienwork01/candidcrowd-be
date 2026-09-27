ALTER TYPE media_status ADD VALUE IF NOT EXISTS 'featured';
ALTER TYPE media_status ADD VALUE IF NOT EXISTS 'hidden';

CREATE INDEX IF NOT EXISTS media_event_status_created_at_idx
  ON media (event_id, status, created_at DESC, id DESC);
