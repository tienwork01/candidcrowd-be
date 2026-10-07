ALTER TABLE media
  DROP COLUMN IF EXISTS transition_cue_seconds,
  DROP COLUMN IF EXISTS duration_seconds,
  DROP COLUMN IF EXISTS height,
  DROP COLUMN IF EXISTS width;
