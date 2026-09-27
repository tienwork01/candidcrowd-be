ALTER TABLE live_wall_sessions
  ADD COLUMN cta_every_media INTEGER NOT NULL DEFAULT 8
    CHECK (cta_every_media BETWEEN 3 AND 30);
