-- Deferred work on a media record. Thumbnail rendering used to run in a
-- fire-and-forget goroutine, so a deploy or a crash lost it permanently and an
-- upload burst could start an unbounded number of image decodes at once.
CREATE TABLE media_jobs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  media_id uuid NOT NULL REFERENCES media(id) ON DELETE CASCADE,
  kind text NOT NULL CHECK (kind IN ('thumbnail')),
  status text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'processing', 'done', 'failed')),
  attempts integer NOT NULL DEFAULT 0,
  last_error text,
  run_after timestamptz NOT NULL DEFAULT now(),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  -- One outstanding job per media record and kind. Enqueueing is therefore
  -- idempotent, which is what lets MarkReady retry safely.
  UNIQUE (media_id, kind)
);

CREATE INDEX media_jobs_claimable_idx ON media_jobs (run_after ASC) WHERE status = 'queued';

-- Ready images that never received a thumbnail are queued once. These are the
-- records the previous in-process goroutine dropped on restart; without this
-- backfill they would keep falling back to the full-size original in grids.
INSERT INTO media_jobs (media_id, kind)
SELECT id, 'thumbnail'
FROM media
WHERE status IN ('ready', 'featured', 'hidden')
  AND thumbnail_ready = false
  AND mime_type LIKE 'image/%'
ON CONFLICT (media_id, kind) DO NOTHING;
