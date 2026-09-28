ALTER TABLE live_wall_sessions
  ADD COLUMN layout_mode TEXT NOT NULL DEFAULT 'spotlight'
    CHECK (layout_mode IN ('spotlight', 'mosaic', 'featured')),
  ADD COLUMN slide_duration_seconds INTEGER NOT NULL DEFAULT 5
    CHECK (slide_duration_seconds BETWEEN 5 AND 12),
  ADD COLUMN qr_strategy TEXT NOT NULL DEFAULT 'interval'
    CHECK (qr_strategy IN ('interval', 'always', 'empty_only', 'hidden')),
  ADD COLUMN arrival_behavior TEXT NOT NULL DEFAULT 'queue'
    CHECK (arrival_behavior IN ('queue', 'next'));
