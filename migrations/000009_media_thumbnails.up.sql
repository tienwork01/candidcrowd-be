ALTER TABLE media
  ADD COLUMN IF NOT EXISTS thumbnail_ready boolean NOT NULL DEFAULT false;
