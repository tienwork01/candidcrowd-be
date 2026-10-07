-- Event plans: a versioned catalog, display prices, and the grant that ties
-- exactly one plan version to each event. Nothing here knows about payment.

CREATE TABLE plan_versions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code          text NOT NULL CHECK (code IN ('free', 'experience', 'signature')),
    version       integer NOT NULL CHECK (version > 0),
    display_name  text NOT NULL,
    status        text NOT NULL CHECK (status IN ('draft', 'active', 'retired')),
    entitlements  jsonb NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    activated_at  timestamptz,
    retired_at    timestamptz,
    UNIQUE (code, version)
);

-- New grants always resolve "the" active version of a code.
CREATE UNIQUE INDEX plan_versions_one_active_per_code_idx ON plan_versions (code) WHERE status = 'active';

CREATE TABLE plan_prices (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    plan_version_id   uuid NOT NULL REFERENCES plan_versions (id),
    currency          char(3) NOT NULL CHECK (currency = upper(currency)),
    amount_minor      integer NOT NULL CHECK (amount_minor >= 0),
    compare_at_minor  integer CHECK (compare_at_minor IS NULL OR compare_at_minor >= amount_minor),
    valid_from        timestamptz NOT NULL,
    valid_until       timestamptz CHECK (valid_until IS NULL OR valid_until > valid_from),
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX plan_prices_lookup_idx ON plan_prices (plan_version_id, currency, valid_from DESC);

CREATE TABLE event_plan_grants (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id              uuid NOT NULL REFERENCES events (id),
    plan_version_id       uuid NOT NULL REFERENCES plan_versions (id),
    status                text NOT NULL CHECK (status IN ('active', 'revoked', 'superseded')),
    source                text NOT NULL CHECK (source IN ('system', 'manual', 'migration', 'promotion', 'external')),
    source_reference      text,
    actor_id              uuid REFERENCES users (id),
    entitlement_snapshot  jsonb NOT NULL,
    activated_at          timestamptz NOT NULL,
    upload_expires_at     timestamptz NOT NULL,
    retention_expires_at  timestamptz NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    CHECK (upload_expires_at <= retention_expires_at)
);

-- The core invariant: an event has at most one active grant at a time.
CREATE UNIQUE INDEX event_plan_grants_one_active_idx ON event_plan_grants (event_id) WHERE status = 'active';
-- The same external or manual operation cannot grant a plan twice.
CREATE UNIQUE INDEX event_plan_grants_source_reference_idx ON event_plan_grants (source, source_reference) WHERE source_reference IS NOT NULL;
CREATE INDEX event_plan_grants_event_idx ON event_plan_grants (event_id, created_at DESC);

-- Audit rows deliberately carry no foreign keys: they must outlive whatever
-- they describe, and nothing may rewrite them.
CREATE TABLE event_plan_audits (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id          uuid NOT NULL,
    grant_id          uuid NOT NULL,
    previous_plan     text,
    target_plan       text NOT NULL,
    source            text NOT NULL,
    source_reference  text,
    actor_id          uuid,
    reason            text,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX event_plan_audits_event_idx ON event_plan_audits (event_id, created_at DESC);

CREATE FUNCTION event_plan_audits_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'event_plan_audits rows are immutable';
END;
$$;

CREATE TRIGGER event_plan_audits_no_update_delete
    BEFORE UPDATE OR DELETE ON event_plan_audits
    FOR EACH ROW EXECUTE FUNCTION event_plan_audits_immutable();

-- Catalog v1. Byte limits are MiB/GiB; 0 in a per-type item limit means the
-- plan has no type-specific cap, only the overall media cap.
INSERT INTO plan_versions (code, version, display_name, status, entitlements, activated_at) VALUES
('free', 1, 'Free', 'active', '{
  "features": [],
  "fair_use": false,
  "limits": {
    "max_guests": 20,
    "max_media_items": 50,
    "max_photo_items": 45,
    "max_video_items": 5,
    "max_media_bytes": 524288000,
    "max_live_wall_items": 20,
    "retention_days": 7
  }
}', now()),
('experience', 1, 'Experience', 'active', '{
  "features": ["media.zip_export", "media.feature", "media.bulk_moderation", "customization.full", "branding.remove", "live_wall.full"],
  "fair_use": true,
  "limits": {
    "max_guests": 0,
    "max_media_items": 2000,
    "max_photo_items": 2000,
    "max_video_items": 500,
    "max_media_bytes": 53687091200,
    "max_live_wall_items": 0,
    "retention_days": 365
  }
}', now()),
('signature', 1, 'Signature', 'active', '{
  "features": ["media.zip_export", "media.feature", "media.bulk_moderation", "customization.full", "branding.remove", "live_wall.full", "live_wall.through_moment", "analytics.qr_sources"],
  "fair_use": true,
  "limits": {
    "max_guests": 0,
    "max_media_items": 5000,
    "max_photo_items": 5000,
    "max_video_items": 1500,
    "max_media_bytes": 161061273600,
    "max_live_wall_items": 0,
    "retention_days": 730
  }
}', now());

-- Launch prices; compare_at is the standard price shown alongside.
INSERT INTO plan_prices (plan_version_id, currency, amount_minor, compare_at_minor, valid_from)
SELECT id, 'USD', p.amount, p.compare_at, now()
FROM plan_versions v
JOIN (VALUES ('free', 0, NULL::integer), ('experience', 3900, 4900), ('signature', 6900, 8900)) AS p(code, amount, compare_at)
  ON p.code = v.code
WHERE v.version = 1;
