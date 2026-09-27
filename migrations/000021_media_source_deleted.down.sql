DROP INDEX IF EXISTS media_archived_idx;
ALTER TABLE media DROP COLUMN IF EXISTS source_deleted_at;
