-- Item quotas next to the existing byte quota. A 0 limit means "no limit".
--
-- uploaded_* count every upload that ever completed and are never decremented:
-- deleting media frees storage, not upload allowance. reserved_* hold uploads
-- in flight, exactly like reserved_media_bytes.
ALTER TABLE events
    ADD COLUMN max_media_items      bigint NOT NULL DEFAULT 0 CHECK (max_media_items >= 0),
    ADD COLUMN uploaded_media_items bigint NOT NULL DEFAULT 0 CHECK (uploaded_media_items >= 0),
    ADD COLUMN reserved_media_items bigint NOT NULL DEFAULT 0 CHECK (reserved_media_items >= 0),
    ADD COLUMN max_photo_items      bigint NOT NULL DEFAULT 0 CHECK (max_photo_items >= 0),
    ADD COLUMN uploaded_photo_items bigint NOT NULL DEFAULT 0 CHECK (uploaded_photo_items >= 0),
    ADD COLUMN reserved_photo_items bigint NOT NULL DEFAULT 0 CHECK (reserved_photo_items >= 0),
    ADD COLUMN max_video_items      bigint NOT NULL DEFAULT 0 CHECK (max_video_items >= 0),
    ADD COLUMN uploaded_video_items bigint NOT NULL DEFAULT 0 CHECK (uploaded_video_items >= 0),
    ADD COLUMN reserved_video_items bigint NOT NULL DEFAULT 0 CHECK (reserved_video_items >= 0);

-- Completed uploads end in ready, featured, hidden or deleted (moderation keeps
-- the row); abandoned uploads are removed outright.
UPDATE events e
SET uploaded_media_items = m.media_items,
    uploaded_photo_items = m.photo_items,
    uploaded_video_items = m.video_items
FROM (
    SELECT event_id,
           COUNT(*) AS media_items,
           COUNT(*) FILTER (WHERE mime_type LIKE 'image/%') AS photo_items,
           COUNT(*) FILTER (WHERE mime_type LIKE 'video/%') AS video_items
    FROM media
    WHERE status IN ('ready', 'featured', 'hidden', 'deleted')
    GROUP BY event_id
) m
WHERE m.event_id = e.id;

UPDATE events e
SET reserved_media_items = m.media_items,
    reserved_photo_items = m.photo_items,
    reserved_video_items = m.video_items
FROM (
    SELECT event_id,
           COUNT(*) AS media_items,
           COUNT(*) FILTER (WHERE mime_type LIKE 'image/%') AS photo_items,
           COUNT(*) FILTER (WHERE mime_type LIKE 'video/%') AS video_items
    FROM media
    WHERE status IN ('pending', 'uploading', 'uploaded')
    GROUP BY event_id
) m
WHERE m.event_id = e.id;

-- Limits come from each event's active grant snapshot.
UPDATE events e
SET max_media_items = COALESCE((g.entitlement_snapshot->'limits'->>'max_media_items')::bigint, 0),
    max_photo_items = COALESCE((g.entitlement_snapshot->'limits'->>'max_photo_items')::bigint, 0),
    max_video_items = COALESCE((g.entitlement_snapshot->'limits'->>'max_video_items')::bigint, 0)
FROM event_plan_grants g
WHERE g.event_id = e.id AND g.status = 'active';
