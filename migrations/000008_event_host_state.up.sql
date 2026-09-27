ALTER TABLE events
  ADD COLUMN event_mode text NOT NULL DEFAULT 'social',
  ADD COLUMN lifecycle_phase text,
  ADD COLUMN setup_checklist jsonb NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN candid_camera_enabled boolean NOT NULL DEFAULT true;
