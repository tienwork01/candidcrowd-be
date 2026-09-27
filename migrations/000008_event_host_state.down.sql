ALTER TABLE events
  DROP COLUMN IF EXISTS candid_camera_enabled,
  DROP COLUMN IF EXISTS setup_checklist,
  DROP COLUMN IF EXISTS lifecycle_phase,
  DROP COLUMN IF EXISTS event_mode;
