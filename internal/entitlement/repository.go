package entitlement

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ActivationRequest identifies an activation for the repository: which event,
// and the provenance recorded on the grant and its audit row.
type ActivationRequest struct {
	EventID   uuid.UUID
	Source    Source
	SourceRef string
	ActorID   *uuid.UUID
	Reason    string
}

// EventFacts is what grant building needs to know about the event, read
// under the same lock as the current grant.
type EventFacts struct {
	EventDate *time.Time
}

// BuildFunc decides the new grant from the event and its current grant (nil
// when it has none). Returning an error aborts the activation.
type BuildFunc func(facts EventFacts, current *ActiveGrant) (Grant, catalog.PlanCode, error)

// Usage counts what an event has consumed. Item counts are successful uploads
// over the event's life — deleting media does not give them back — while
// MediaBytes is what is currently stored.
type Usage struct {
	MediaItems int64 `json:"media_items"`
	PhotoItems int64 `json:"photo_items"`
	VideoItems int64 `json:"video_items"`
	MediaBytes int64 `json:"media_bytes"`
}

// HostEventPlan is one row of a host's plan overview. Plan fields are nil for
// an event that has not been given a grant yet.
type HostEventPlan struct {
	EventID            uuid.UUID
	EventName          string
	EventDate          *time.Time
	PlanCode           *catalog.PlanCode
	PlanName           *string
	GrantStatus        *Status
	UploadExpiresAt    *time.Time
	RetentionExpiresAt *time.Time
	MediaItemsLimit    *int64
	MediaBytesLimit    *int64
}

type Repository interface {
	ActiveGrant(ctx context.Context, eventID uuid.UUID) (ActiveGrant, error)
	Activate(ctx context.Context, req ActivationRequest, build BuildFunc) (Grant, error)
	Usage(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]Usage, error)
	HostEventPlans(ctx context.Context, hostID uuid.UUID, page, perPage int) ([]HostEventPlan, int64, error)
	// BackfillCandidates pages, by event id, through events created before
	// cutoff that have no grant or only a Free one.
	BackfillCandidates(ctx context.Context, cutoff time.Time, after uuid.UUID, limit int) ([]uuid.UUID, error)
	// AuditTrail lists an event's plan changes, newest first.
	AuditTrail(ctx context.Context, eventID uuid.UUID, limit int) ([]AuditEntry, error)
	// Revoke retires the active grant and reinstates the one it superseded.
	Revoke(ctx context.Context, eventID uuid.UUID, reason string, at time.Time) (revoked, restored ActiveGrant, err error)
	Reconcile(ctx context.Context, opts ReconcileOptions) (ReconcileReport, error)
}

// ReconcileOptions tunes the reconciliation report.
type ReconcileOptions struct {
	Now          time.Time
	Grace        time.Duration
	ExpiringSoon time.Duration
	Sample       int
}

// ReconcileIssue is one invariant check with the events that break it.
type ReconcileIssue struct {
	Count  int64
	Sample []uuid.UUID
}

// ReconcileReport checks the invariants the plan system relies on. A healthy
// system reports zero for everything except the retention figures.
type ReconcileReport struct {
	// EventsWithoutGrant must be zero: every event has exactly one plan.
	EventsWithoutGrant ReconcileIssue
	// LimitMismatch: the event's hot limits differ from its grant snapshot.
	LimitMismatch ReconcileIssue
	// CounterDrift: upload or byte counters differ from the media rows.
	CounterDrift ReconcileIssue
	// RetentionDue: storage and grace period over, media not yet purged.
	RetentionDue ReconcileIssue
	// ExpiringSoon: storage period ends within ExpiringSoon.
	ExpiringSoon ReconcileIssue
	// BillingReview: a refund or chargeback parked the event for a human.
	// Billing never downgrades or deletes on its own, so these wait here until
	// someone decides.
	BillingReview ReconcileIssue
}

// AuditEntry is one recorded plan change.
type AuditEntry struct {
	GrantID         uuid.UUID
	PreviousPlan    *string
	TargetPlan      string
	Source          Source
	SourceReference *string
	ActorID         *uuid.UUID
	Reason          *string
	CreatedAt       time.Time
}

type gormRepository struct{ db *gorm.DB }

func NewGormRepository(db *gorm.DB) Repository { return &gormRepository{db: db} }

const activeGrantSelect = `
	SELECT g.*, pv.code AS plan_code, pv.display_name AS plan_name, pv.version AS plan_version
	FROM event_plan_grants g
	JOIN plan_versions pv ON pv.id = g.plan_version_id
	WHERE g.event_id = ? AND g.status = 'active'`

func activeGrant(tx *gorm.DB, eventID uuid.UUID) (*ActiveGrant, error) {
	var rows []ActiveGrant
	if err := tx.Raw(activeGrantSelect, eventID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func (r *gormRepository) ActiveGrant(ctx context.Context, eventID uuid.UUID) (ActiveGrant, error) {
	g, err := activeGrant(r.db.WithContext(ctx), eventID)
	if err != nil {
		return ActiveGrant{}, err
	}
	if g == nil {
		return ActiveGrant{}, ErrNoActiveGrant
	}
	return *g, nil
}

// Activate runs a grant change as one transaction. The event row is locked
// first, so concurrent activations for one event queue behind each other and
// each sees the grant the previous one left active.
func (r *gormRepository) Activate(ctx context.Context, req ActivationRequest, build BuildFunc) (Grant, error) {
	var created Grant
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var facts []EventFacts
		if err := tx.Raw(`SELECT event_date FROM events WHERE id = ? FOR UPDATE`, req.EventID).Scan(&facts).Error; err != nil {
			return err
		}
		if len(facts) == 0 {
			return ErrEventNotFound
		}
		if req.SourceRef != "" {
			var existing int64
			if err := tx.Model(&Grant{}).Where("source = ? AND source_reference = ?", req.Source, req.SourceRef).Count(&existing).Error; err != nil {
				return err
			}
			if existing > 0 {
				return ErrDuplicateSourceReference
			}
		}
		current, err := activeGrant(tx, req.EventID)
		if err != nil {
			return err
		}
		grant, target, err := build(facts[0], current)
		if err != nil {
			return err
		}
		if err := writeGrant(tx, grant, current, target, req.Reason); err != nil {
			return err
		}
		created = grant
		return nil
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		// Two different events activated with the same reference at once;
		// the unique index lets only one of them through.
		return Grant{}, ErrDuplicateSourceReference
	}
	return created, err
}

// writeGrant supersedes previous (if any), inserts grant, copies its hot quota
// onto the event row and appends the audit record, all on tx.
func writeGrant(tx *gorm.DB, grant Grant, previous *ActiveGrant, target catalog.PlanCode, reason string) error {
	var previousPlan *string
	if previous != nil {
		code := string(previous.PlanCode)
		previousPlan = &code
		if err := tx.Model(&Grant{}).Where("id = ? AND status = ?", previous.ID, StatusActive).
			Update("status", StatusSuperseded).Error; err != nil {
			return err
		}
	}
	if err := tx.Create(&grant).Error; err != nil {
		return err
	}
	if err := applyHotLimits(tx, grant.EventID, grant.EntitlementSnapshot.Limits); err != nil {
		return err
	}
	return writeAudit(tx, grant, previousPlan, target, reason)
}

// applyHotLimits copies a grant's limits onto the event row, so a reservation
// is one conditional UPDATE with no join to the grant.
func applyHotLimits(tx *gorm.DB, eventID uuid.UUID, limits catalog.Limits) error {
	return tx.Exec(`
		UPDATE events
		SET max_media_bytes = CASE WHEN CAST(@bytes AS bigint) > 0 THEN CAST(@bytes AS bigint) ELSE max_media_bytes END,
		    max_media_items = CAST(@items AS bigint),
		    max_photo_items = CAST(@photos AS bigint),
		    max_video_items = CAST(@videos AS bigint)
		WHERE id = @event`,
		sql.Named("bytes", limits.MaxMediaBytes), sql.Named("items", limits.MaxMediaItems),
		sql.Named("photos", limits.MaxPhotoItems), sql.Named("videos", limits.MaxVideoItems),
		sql.Named("event", eventID)).Error
}

func writeAudit(tx *gorm.DB, grant Grant, previousPlan *string, target catalog.PlanCode, reason string) error {
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}
	return tx.Create(&audit{
		ID:              uuid.New(),
		EventID:         grant.EventID,
		GrantID:         grant.ID,
		PreviousPlan:    previousPlan,
		TargetPlan:      string(target),
		Source:          grant.Source,
		SourceReference: grant.SourceReference,
		ActorID:         grant.ActorID,
		Reason:          reasonPtr,
		CreatedAt:       grant.ActivatedAt,
	}).Error
}

// Usage reads consumption for several events in one round trip, from the
// counters reservations maintain on the event row.
func (r *gormRepository) Usage(ctx context.Context, eventIDs []uuid.UUID) (map[uuid.UUID]Usage, error) {
	out := make(map[uuid.UUID]Usage, len(eventIDs))
	if len(eventIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		EventID uuid.UUID
		Usage   `gorm:"embedded"`
	}
	err := r.db.WithContext(ctx).Raw(`
		SELECT id AS event_id,
		       used_media_bytes AS media_bytes,
		       uploaded_media_items AS media_items,
		       uploaded_photo_items AS photo_items,
		       uploaded_video_items AS video_items
		FROM events
		WHERE id IN ?`, eventIDs).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.EventID] = row.Usage
	}
	return out, nil
}

func (r *gormRepository) HostEventPlans(ctx context.Context, hostID uuid.UUID, page, perPage int) ([]HostEventPlan, int64, error) {
	db := r.db.WithContext(ctx)
	var total int64
	if err := db.Raw(`SELECT COUNT(*) FROM events WHERE host_id = ? AND status <> 'deleted'`, hostID).Scan(&total).Error; err != nil {
		return nil, 0, err
	}
	rows := make([]HostEventPlan, 0)
	err := db.Raw(`
		SELECT e.id AS event_id,
		       e.name AS event_name,
		       e.event_date,
		       pv.code AS plan_code,
		       pv.display_name AS plan_name,
		       g.status AS grant_status,
		       g.upload_expires_at,
		       g.retention_expires_at,
		       (g.entitlement_snapshot->'limits'->>'max_media_items')::bigint AS media_items_limit,
		       (g.entitlement_snapshot->'limits'->>'max_media_bytes')::bigint AS media_bytes_limit
		FROM events e
		LEFT JOIN event_plan_grants g ON g.event_id = e.id AND g.status = 'active'
		LEFT JOIN plan_versions pv ON pv.id = g.plan_version_id
		WHERE e.host_id = ? AND e.status <> 'deleted'
		ORDER BY e.created_at DESC, e.id
		LIMIT ? OFFSET ?`, hostID, perPage, (page-1)*perPage).Scan(&rows).Error
	return rows, total, err
}

func (r *gormRepository) BackfillCandidates(ctx context.Context, cutoff time.Time, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := r.db.WithContext(ctx).Raw(`
		SELECT e.id
		FROM events e
		LEFT JOIN event_plan_grants g ON g.event_id = e.id AND g.status = 'active'
		LEFT JOIN plan_versions pv ON pv.id = g.plan_version_id
		WHERE e.created_at < ?
		  AND e.id > ?
		  AND (g.id IS NULL OR pv.code = 'free')
		ORDER BY e.id
		LIMIT ?`, cutoff, after, limit).Scan(&ids).Error
	return ids, err
}

func (r *gormRepository) AuditTrail(ctx context.Context, eventID uuid.UUID, limit int) ([]AuditEntry, error) {
	entries := make([]AuditEntry, 0)
	err := r.db.WithContext(ctx).Raw(`
		SELECT grant_id, previous_plan, target_plan, source, source_reference, actor_id, reason, created_at
		FROM event_plan_audits
		WHERE event_id = ?
		ORDER BY created_at DESC
		LIMIT ?`, eventID, limit).Scan(&entries).Error
	return entries, err
}

// Revoke undoes the latest activation: the active grant becomes revoked and
// the grant it superseded becomes active again, with its own limits and
// expiry. It runs under the event lock, like Activate.
func (r *gormRepository) Revoke(ctx context.Context, eventID uuid.UUID, reason string, at time.Time) (ActiveGrant, ActiveGrant, error) {
	var revoked, restored ActiveGrant
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var locked []uuid.UUID
		if err := tx.Raw(`SELECT id FROM events WHERE id = ? FOR UPDATE`, eventID).Scan(&locked).Error; err != nil {
			return err
		}
		if len(locked) == 0 {
			return ErrEventNotFound
		}
		current, err := activeGrant(tx, eventID)
		if err != nil {
			return err
		}
		if current == nil {
			return ErrNoActiveGrant
		}
		var previous []ActiveGrant
		if err := tx.Raw(`
			SELECT g.*, pv.code AS plan_code, pv.display_name AS plan_name, pv.version AS plan_version
			FROM event_plan_grants g
			JOIN plan_versions pv ON pv.id = g.plan_version_id
			WHERE g.event_id = ? AND g.status = 'superseded'
			ORDER BY g.activated_at DESC, g.created_at DESC
			LIMIT 1`, eventID).Scan(&previous).Error; err != nil {
			return err
		}
		if len(previous) == 0 {
			return ErrNothingToRestore
		}
		if err := tx.Model(&Grant{}).Where("id = ?", current.ID).Update("status", StatusRevoked).Error; err != nil {
			return err
		}
		if err := tx.Model(&Grant{}).Where("id = ?", previous[0].ID).Update("status", StatusActive).Error; err != nil {
			return err
		}
		if err := applyHotLimits(tx, eventID, previous[0].EntitlementSnapshot.Limits); err != nil {
			return err
		}
		from := string(current.PlanCode)
		reinstated := previous[0].Grant
		reinstated.Status = StatusActive
		reinstated.Source = SourceManual
		reinstated.SourceReference = nil
		reinstated.ActivatedAt = at
		if err := writeAudit(tx, reinstated, &from, previous[0].PlanCode, "revoked: "+reason); err != nil {
			return err
		}
		revoked, restored = *current, previous[0]
		restored.Status = StatusActive
		return nil
	})
	return revoked, restored, err
}

func (r *gormRepository) Reconcile(ctx context.Context, opts ReconcileOptions) (ReconcileReport, error) {
	db := r.db.WithContext(ctx)
	check := func(query string, args ...any) (ReconcileIssue, error) {
		var issue ReconcileIssue
		var ids []uuid.UUID
		if err := db.Raw(`SELECT COUNT(*) FROM (`+query+`) issue`, args...).Scan(&issue.Count).Error; err != nil {
			return issue, err
		}
		if err := db.Raw(`SELECT event_id FROM (`+query+`) issue LIMIT ?`, append(args, opts.Sample)...).Scan(&ids).Error; err != nil {
			return issue, err
		}
		issue.Sample = ids
		return issue, nil
	}
	var report ReconcileReport
	var err error
	if report.EventsWithoutGrant, err = check(`
		SELECT e.id AS event_id FROM events e
		WHERE NOT EXISTS (SELECT 1 FROM event_plan_grants g WHERE g.event_id = e.id AND g.status = 'active')`); err != nil {
		return report, err
	}
	if report.LimitMismatch, err = check(`
		SELECT e.id AS event_id FROM events e
		JOIN event_plan_grants g ON g.event_id = e.id AND g.status = 'active'
		WHERE e.max_media_items <> COALESCE((g.entitlement_snapshot->'limits'->>'max_media_items')::bigint, 0)
		   OR e.max_photo_items <> COALESCE((g.entitlement_snapshot->'limits'->>'max_photo_items')::bigint, 0)
		   OR e.max_video_items <> COALESCE((g.entitlement_snapshot->'limits'->>'max_video_items')::bigint, 0)
		   OR (COALESCE((g.entitlement_snapshot->'limits'->>'max_media_bytes')::bigint, 0) > 0
		       AND e.max_media_bytes <> (g.entitlement_snapshot->'limits'->>'max_media_bytes')::bigint)`); err != nil {
		return report, err
	}
	// Purged events are excluded: their rows are all marked deleted on purpose.
	if report.CounterDrift, err = check(`
		SELECT e.id AS event_id FROM events e
		LEFT JOIN (
			SELECT event_id,
			       COUNT(*) FILTER (WHERE status IN ('ready', 'featured', 'hidden', 'deleted')) AS uploaded,
			       COUNT(*) FILTER (WHERE status IN ('ready', 'featured', 'hidden', 'deleted') AND mime_type LIKE 'image/%') AS photos,
			       COUNT(*) FILTER (WHERE status IN ('ready', 'featured', 'hidden', 'deleted') AND mime_type LIKE 'video/%') AS videos,
			       COUNT(*) FILTER (WHERE status IN ('pending', 'uploading', 'uploaded')) AS reserved,
			       COALESCE(SUM(actual_size) FILTER (WHERE status IN ('ready', 'featured', 'hidden')), 0) AS bytes
			FROM media GROUP BY event_id
		) m ON m.event_id = e.id
		WHERE e.media_purged_at IS NULL
		  AND (e.uploaded_media_items <> COALESCE(m.uploaded, 0)
		    OR e.uploaded_photo_items <> COALESCE(m.photos, 0)
		    OR e.uploaded_video_items <> COALESCE(m.videos, 0)
		    OR e.reserved_media_items <> COALESCE(m.reserved, 0)
		    OR e.used_media_bytes <> COALESCE(m.bytes, 0))`); err != nil {
		return report, err
	}
	if report.RetentionDue, err = check(`
		SELECT g.event_id FROM event_plan_grants g
		JOIN events e ON e.id = g.event_id
		WHERE g.status = 'active' AND e.media_purged_at IS NULL AND g.retention_expires_at <= ?`,
		opts.Now.Add(-opts.Grace)); err != nil {
		return report, err
	}
	if report.ExpiringSoon, err = check(`
		SELECT g.event_id FROM event_plan_grants g
		JOIN events e ON e.id = g.event_id
		WHERE g.status = 'active' AND e.media_purged_at IS NULL
		  AND g.retention_expires_at > ? AND g.retention_expires_at <= ?`,
		opts.Now, opts.Now.Add(opts.ExpiringSoon)); err != nil {
		return report, err
	}
	if report.BillingReview, err = check(
		`SELECT id AS event_id FROM events WHERE billing_status <> 'ok' AND status <> 'deleted'`); err != nil {
		return report, err
	}
	return report, nil
}
