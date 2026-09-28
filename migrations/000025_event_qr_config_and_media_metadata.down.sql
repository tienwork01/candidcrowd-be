ALTER TABLE media
  DROP COLUMN caption,
  DROP COLUMN guest_name;

ALTER TABLE events DROP COLUMN qr_config;
