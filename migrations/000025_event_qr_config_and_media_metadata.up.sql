ALTER TABLE events ADD COLUMN qr_config jsonb;

ALTER TABLE media
  ADD COLUMN guest_name text,
  ADD COLUMN caption text;
