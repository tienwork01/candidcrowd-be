CREATE EXTENSION IF NOT EXISTS "pgcrypto";
CREATE TYPE media_status AS ENUM ('pending', 'uploading', 'uploaded', 'ready', 'failed', 'deleted');
CREATE TYPE event_status AS ENUM ('draft', 'active', 'closed');
CREATE TABLE users (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), better_auth_user_id text NOT NULL UNIQUE, email text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE events (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), host_id uuid NOT NULL REFERENCES users(id), name text NOT NULL, slug text NOT NULL UNIQUE, event_date date, event_type text NOT NULL DEFAULT 'other', expected_guest_count integer NOT NULL DEFAULT 0 CHECK (expected_guest_count >= 0), status event_status NOT NULL DEFAULT 'active', gallery_enabled boolean NOT NULL DEFAULT true, max_media_bytes bigint NOT NULL DEFAULT 5368709120, used_media_bytes bigint NOT NULL DEFAULT 0, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX events_host_id_idx ON events(host_id);
CREATE TABLE guest_sessions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), event_id uuid NOT NULL REFERENCES events(id), token_hash bytea NOT NULL UNIQUE, expires_at timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX guest_sessions_event_id_idx ON guest_sessions(event_id);
CREATE TABLE media (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), event_id uuid NOT NULL REFERENCES events(id), guest_session_id uuid NOT NULL REFERENCES guest_sessions(id), object_key text NOT NULL UNIQUE, original_filename text NOT NULL, mime_type text NOT NULL, expected_size bigint NOT NULL, actual_size bigint, status media_status NOT NULL DEFAULT 'pending', created_at timestamptz NOT NULL DEFAULT now(), uploaded_at timestamptz
);
CREATE INDEX media_event_id_created_at_idx ON media(event_id, created_at DESC);
