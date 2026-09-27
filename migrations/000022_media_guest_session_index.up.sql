-- Analytics joins media to guest_sessions on this column to count contributors
-- per QR source. Without an index that join reads the event's whole media
-- history.
--
-- CONCURRENTLY keeps the table writable while the index builds, which matters
-- because uploads must not stall during a deploy. It cannot run inside a
-- transaction block, and golang-migrate sends a whole file as one
-- multi-statement query, which Postgres wraps implicitly — so this file holds
-- exactly one statement, and must stay that way.
CREATE INDEX CONCURRENTLY IF NOT EXISTS media_guest_session_idx ON media (guest_session_id);
