DROP INDEX IF EXISTS media_event_ready_checksum_sha256_uq;
ALTER TABLE media DROP CONSTRAINT IF EXISTS media_checksum_sha256_format_chk;
ALTER TABLE media DROP COLUMN IF EXISTS checksum_sha256;
