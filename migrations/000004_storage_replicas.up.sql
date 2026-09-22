CREATE TYPE storage_replica_state AS ENUM ('copying', 'verified', 'deleted', 'failed');
CREATE TABLE storage_replicas (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(), media_id uuid NOT NULL REFERENCES media(id) ON DELETE CASCADE,
  provider text NOT NULL, location_reference text NOT NULL, state storage_replica_state NOT NULL,
  size bigint, checksum_sha256 text, created_at timestamptz NOT NULL DEFAULT now(),
  verified_at timestamptz, expires_at timestamptz, deleted_at timestamptz, UNIQUE (media_id, provider, location_reference)
);
CREATE INDEX storage_replicas_media_provider_idx ON storage_replicas(media_id, provider, state);
