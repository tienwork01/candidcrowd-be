-- Marks media whose R2 original has been removed after being archived to
-- long-term storage.
--
-- The gallery endpoints presign object keys directly. For an archived record
-- that key no longer resolves, so the listing has to know which items must
-- keep going through the application's own /content route instead. Reading it
-- from storage_replicas on every list would mean a correlated subquery per
-- page, hence the denormalised column.
ALTER TABLE media ADD COLUMN source_deleted_at timestamptz;

UPDATE media m
SET source_deleted_at = r.deleted_at
FROM storage_replicas r
WHERE r.media_id = m.id
  AND r.provider = 'r2'
  AND r.state = 'deleted'
  AND r.location_reference = m.object_key;

CREATE INDEX media_archived_idx ON media (event_id) WHERE source_deleted_at IS NOT NULL;
