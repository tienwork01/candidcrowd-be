CREATE TABLE qr_sources (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  event_id uuid NOT NULL REFERENCES events(id),
  code text NOT NULL,
  name text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (event_id, code)
);

CREATE INDEX qr_sources_event_id_idx ON qr_sources (event_id);

ALTER TABLE guest_sessions ADD COLUMN qr_source_code text;
CREATE INDEX guest_sessions_event_source_idx ON guest_sessions (event_id, qr_source_code);
