-- Reservations are counted on the event row instead of being summed from the
-- media table on every upload.
--
-- ReserveUpload used to take SELECT ... FOR UPDATE on the event and then run a
-- SUM() aggregate while holding that lock, so every guest of one event queued
-- behind a single row for the duration of an aggregate scan. With a counter
-- the same quota rule is one conditional UPDATE.
ALTER TABLE events ADD COLUMN reserved_media_bytes bigint NOT NULL DEFAULT 0;

UPDATE events e
SET reserved_media_bytes = COALESCE((
  SELECT SUM(m.expected_size)
  FROM media m
  WHERE m.event_id = e.id
    AND m.status IN ('pending', 'uploading', 'uploaded')
), 0);

ALTER TABLE events ADD CONSTRAINT events_reserved_media_bytes_non_negative
  CHECK (reserved_media_bytes >= 0);
