package entitlement

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/google/uuid"
)

var (
	ErrNoActiveGrant            = errors.New("entitlement: event has no active plan")
	ErrEventNotFound            = errors.New("entitlement: event not found")
	ErrDuplicateSourceReference = errors.New("entitlement: source reference already granted")
	ErrSourceReferenceRequired  = errors.New("entitlement: source reference is required")
	ErrInvalidSource            = errors.New("entitlement: invalid grant source")
	ErrUnknownPlan              = errors.New("entitlement: unknown or inactive plan")
	ErrInvalidTransition        = errors.New("entitlement: plan transition not allowed")
	ErrNothingToRestore         = errors.New("entitlement: the event's first grant cannot be revoked")
)

type Status string

const (
	StatusActive     Status = "active"
	StatusRevoked    Status = "revoked"
	StatusSuperseded Status = "superseded"
)

// Source records who caused a grant. It deliberately says nothing about how
// money changed hands: a future payment integration is just "external".
type Source string

const (
	SourceSystem    Source = "system"
	SourceManual    Source = "manual"
	SourceMigration Source = "migration"
	SourcePromotion Source = "promotion"
	SourceExternal  Source = "external"
	SourcePurchase  Source = "purchase"
	SourceReferral  Source = "referral"
	SourceAdmin     Source = "admin"
	SourceBusiness  Source = "business"
)

func (s Source) Valid() bool {
	switch s {
	case SourceSystem, SourceManual, SourceMigration, SourcePromotion, SourceExternal, SourcePurchase, SourceReferral, SourceAdmin, SourceBusiness:
		return true
	}
	return false
}

// Grant ties one plan version to one event. There is no expired status:
// expiry is read from the timestamps, so an event always keeps exactly one
// active grant and nothing has to flip a status on a schedule.
type Grant struct {
	ID                  uuid.UUID            `gorm:"type:uuid;primaryKey" json:"id"`
	EventID             uuid.UUID            `gorm:"type:uuid" json:"event_id"`
	PlanVersionID       uuid.UUID            `gorm:"type:uuid" json:"plan_version_id"`
	EventLicenseID      *uuid.UUID           `gorm:"type:uuid;column:event_license_id" json:"event_license_id,omitempty"`
	Status              Status               `json:"status"`
	Source              Source               `json:"source"`
	SourceReference     *string              `json:"source_reference,omitempty"`
	ActorID             *uuid.UUID           `gorm:"type:uuid" json:"actor_id,omitempty"`
	EntitlementSnapshot catalog.Entitlements `gorm:"type:jsonb" json:"-"`
	ActivatedAt         time.Time            `json:"activated_at"`
	UploadExpiresAt     time.Time            `json:"upload_expires_at"`
	RetentionExpiresAt  time.Time            `json:"retention_expires_at"`
	CreatedAt           time.Time            `json:"created_at"`
}

func (Grant) TableName() string { return "event_plan_grants" }

// ActiveGrant is a grant together with the plan it was issued from.
type ActiveGrant struct {
	Grant       `gorm:"embedded"`
	PlanCode    catalog.PlanCode `gorm:"column:plan_code"`
	PlanName    string           `gorm:"column:plan_name"`
	PlanVersion int              `gorm:"column:plan_version"`
}

type audit struct {
	ID              uuid.UUID `gorm:"type:uuid;primaryKey"`
	EventID         uuid.UUID `gorm:"type:uuid"`
	GrantID         uuid.UUID `gorm:"type:uuid"`
	PreviousPlan    *string
	TargetPlan      string
	Source          Source
	SourceReference *string
	ActorID         *uuid.UUID `gorm:"type:uuid"`
	Reason          *string
	CreatedAt       time.Time
}

func (audit) TableName() string { return "event_plan_audits" }

// EventEntitlement is the effective plan of one event, resolved from its
// active grant's snapshot rather than the live catalog.
type EventEntitlement struct {
	EventID            uuid.UUID
	GrantID            uuid.UUID
	PlanCode           catalog.PlanCode
	PlanName           string
	PlanVersion        int
	Source             Source
	Entitlements       catalog.Entitlements
	ActivatedAt        time.Time
	UploadExpiresAt    time.Time
	RetentionExpiresAt time.Time
}

func fromActiveGrant(g ActiveGrant) EventEntitlement {
	return EventEntitlement{
		EventID:            g.EventID,
		GrantID:            g.ID,
		PlanCode:           g.PlanCode,
		PlanName:           g.PlanName,
		PlanVersion:        g.PlanVersion,
		Source:             g.Source,
		Entitlements:       g.EntitlementSnapshot,
		ActivatedAt:        g.ActivatedAt,
		UploadExpiresAt:    g.UploadExpiresAt,
		RetentionExpiresAt: g.RetentionExpiresAt,
	}
}

func (e EventEntitlement) Has(f catalog.Feature) bool { return e.Entitlements.Has(f) }

func (e EventEntitlement) Limits() catalog.Limits { return e.Entitlements.Limits }

func (e EventEntitlement) UploadClosed(now time.Time) bool { return !now.Before(e.UploadExpiresAt) }

func (e EventEntitlement) RetentionExpired(now time.Time) bool {
	return !now.Before(e.RetentionExpiresAt)
}

// UpgradeOptions lists the plans this event may still move to. Only upward
// moves exist; there is no downgrade.
func (e EventEntitlement) UpgradeOptions() []catalog.PlanCode {
	options := []catalog.PlanCode{}
	for _, plan := range catalog.Plans() {
		if plan.TierRank > e.PlanCode.Rank() {
			options = append(options, plan.Code)
		}
	}
	return options
}

// FeatureError is returned when an event's plan does not include a feature.
type FeatureError struct {
	Feature      catalog.Feature
	RequiredPlan catalog.PlanCode
}

func (e *FeatureError) Error() string {
	return fmt.Sprintf("feature %s is not in this event's plan", e.Feature)
}

func (e *FeatureError) APIError() *apierror.Error {
	message := "This feature is not included in this event's plan."
	if e.RequiredPlan != "" {
		message = fmt.Sprintf("This feature requires the %s plan.", e.RequiredPlan)
	}
	return apierror.New(http.StatusForbidden, "feature_not_in_plan", message).
		WithDetails(map[string]any{"feature": e.Feature, "required_plan": e.RequiredPlan})
}

// CheckTransition allows a first grant of any plan and otherwise only upward
// moves: free → experience → signature.
func CheckTransition(from *catalog.PlanCode, to catalog.PlanCode) error {
	if !to.Valid() {
		return ErrUnknownPlan
	}
	if from == nil {
		return nil
	}
	if to.Rank() <= from.Rank() {
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, *from, to)
	}
	return nil
}

// Expiry computes when an event stops accepting uploads and when its media
// becomes eligible for deletion.
//
// The anchor is the start of the day after the event date (UTC), but never
// earlier than activation: an event created or upgraded after it happened
// still gets the plan's full retention instead of starting already expired.
// Uploads stay open for the whole retention period, so guests can still share
// photos after the event.
func Expiry(eventDate *time.Time, activatedAt time.Time, retentionDays int) (uploadExpiresAt, retentionExpiresAt time.Time) {
	anchor := activatedAt.UTC()
	if eventDate != nil {
		y, m, d := eventDate.Date()
		if dayAfter := time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC); dayAfter.After(anchor) {
			anchor = dayAfter
		}
	}
	retentionExpiresAt = anchor.AddDate(0, 0, retentionDays)
	return retentionExpiresAt, retentionExpiresAt
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
