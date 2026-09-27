-- Serves the gallery listing exactly as it is written: filter by event,
-- restrict to the two visible statuses, order by (created_at DESC, id DESC),
-- and seek with a row-wise cursor comparison.
--
-- The existing media_event_status_created_at_idx leads with status, so a query
-- matching two statuses cannot walk it in created_at order and the planner
-- falls back to a bitmap scan plus a sort of the event's whole gallery on
-- every page. Moving status into a partial predicate leaves the ordering
-- columns contiguous.
--
-- One statement per file: see 000022 for why.
CREATE INDEX CONCURRENTLY IF NOT EXISTS media_event_visible_created_idx
  ON media (event_id, created_at DESC, id DESC)
  WHERE status IN ('ready', 'featured');
