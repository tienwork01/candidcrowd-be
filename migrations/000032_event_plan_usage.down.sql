ALTER TABLE events
    DROP COLUMN IF EXISTS max_media_items,
    DROP COLUMN IF EXISTS uploaded_media_items,
    DROP COLUMN IF EXISTS reserved_media_items,
    DROP COLUMN IF EXISTS max_photo_items,
    DROP COLUMN IF EXISTS uploaded_photo_items,
    DROP COLUMN IF EXISTS reserved_photo_items,
    DROP COLUMN IF EXISTS max_video_items,
    DROP COLUMN IF EXISTS uploaded_video_items,
    DROP COLUMN IF EXISTS reserved_video_items;
