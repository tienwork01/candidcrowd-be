ALTER TABLE media ADD COLUMN checksum_sha256 text;
ALTER TABLE media ADD CONSTRAINT media_checksum_sha256_format_chk CHECK (checksum_sha256 IS NULL OR checksum_sha256 ~ '^[0-9a-f]{64}$');
CREATE UNIQUE INDEX media_event_ready_checksum_sha256_uq ON media (event_id, checksum_sha256) WHERE status = 'ready' AND checksum_sha256 IS NOT NULL;
