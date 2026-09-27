ALTER TABLE events DROP CONSTRAINT IF EXISTS events_reserved_media_bytes_non_negative;
ALTER TABLE events DROP COLUMN IF EXISTS reserved_media_bytes;
