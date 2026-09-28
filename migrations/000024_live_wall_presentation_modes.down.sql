ALTER TABLE live_wall_sessions
  DROP COLUMN IF EXISTS arrival_behavior,
  DROP COLUMN IF EXISTS qr_strategy,
  DROP COLUMN IF EXISTS slide_duration_seconds,
  DROP COLUMN IF EXISTS layout_mode;
