CREATE TABLE event_exports (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  event_id uuid NOT NULL REFERENCES events(id),
  status text NOT NULL CHECK (status IN ('queued', 'processing', 'ready', 'failed')),
  object_key text,
  error_message text,
  created_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz
);

CREATE INDEX event_exports_event_created_idx ON event_exports (event_id, created_at DESC);
CREATE INDEX event_exports_queued_idx ON event_exports (created_at ASC) WHERE status = 'queued';
