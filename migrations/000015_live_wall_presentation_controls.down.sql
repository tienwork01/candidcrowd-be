ALTER TABLE live_wall_sessions
  DROP COLUMN IF EXISTS revision,
  DROP COLUMN IF EXISTS is_blackout,
  DROP COLUMN IF EXISTS show_cta,
  DROP COLUMN IF EXISTS is_playing;
