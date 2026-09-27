CREATE TABLE live_wall_sessions (
  id uuid PRIMARY KEY,
  event_id uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
  event_name text NOT NULL,
  event_slug text NOT NULL,
  token_hash text NOT NULL UNIQUE,
  status text NOT NULL CHECK (status IN ('live', 'ended', 'revoked')) DEFAULT 'live',
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  ended_at timestamptz
);

CREATE INDEX live_wall_sessions_event_idx ON live_wall_sessions (event_id, created_at DESC);
CREATE INDEX live_wall_sessions_active_idx ON live_wall_sessions (token_hash, expires_at) WHERE status = 'live';
