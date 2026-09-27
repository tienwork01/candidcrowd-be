ALTER TABLE live_wall_sessions
  ADD COLUMN content_policy TEXT NOT NULL DEFAULT 'featured_only'
    CHECK (content_policy IN ('featured_only', 'auto_approved'));
