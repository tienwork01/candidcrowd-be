ALTER TABLE live_wall_sessions
  ADD COLUMN transition_mode TEXT NOT NULL DEFAULT 'cinematic'
    CHECK (transition_mode IN ('classic', 'cinematic', 'float_3d', 'flash'));
