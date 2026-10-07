ALTER TABLE media
  ADD COLUMN width integer CHECK (width > 0),
  ADD COLUMN height integer CHECK (height > 0),
  ADD COLUMN duration_seconds double precision CHECK (duration_seconds >= 0),
  ADD COLUMN transition_cue_seconds double precision CHECK (transition_cue_seconds >= 0);

-- Reuse the durable media worker to backfill presentation metadata. Existing
-- thumbnails remain valid: the worker only probes the original when the
-- thumbnail is already present.
INSERT INTO media_jobs (media_id, kind)
SELECT id, 'thumbnail'
FROM media
WHERE status IN ('ready', 'featured', 'hidden')
  AND (width IS NULL OR height IS NULL OR (mime_type LIKE 'video/%' AND duration_seconds IS NULL))
ON CONFLICT (media_id, kind) DO UPDATE
SET status = 'queued',
    attempts = 0,
    last_error = NULL,
    run_after = now(),
    updated_at = now();
