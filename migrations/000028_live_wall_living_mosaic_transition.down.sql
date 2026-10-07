ALTER TABLE live_wall_sessions
  DROP CONSTRAINT IF EXISTS live_wall_sessions_transition_mode_check;

ALTER TABLE live_wall_sessions
  ADD CONSTRAINT live_wall_sessions_transition_mode_check
    CHECK (transition_mode IN ('classic', 'cinematic', 'float_3d', 'flash'));
