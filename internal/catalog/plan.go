package catalog

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

var ErrNotFound = errors.New("catalog: plan not found")

type PlanCode string

// The codes below are the plans this code was written against. They are
// convenience constants, not the list of plans that exist: Valid, Rank and
// Sellable all read the registry in registry.go, which the database fills at
// startup. Adding a tier must not mean editing this file.
const (
	PlanFree       PlanCode = "free"
	PlanExperience PlanCode = "experience"
	PlanSignature  PlanCode = "signature"
)

// Feature is a capability an event either has or does not. Quantities are
// Limits, never features: upload video, for instance, is bounded by item and
// byte limits rather than switched on or off.
type Feature string

const (
	FeatureZIPExport         Feature = "media.zip_export"
	FeatureFeatureMedia      Feature = "media.feature"
	FeatureBulkModeration    Feature = "media.bulk_moderation"
	FeatureFullCustomization Feature = "customization.full"
	FeatureRemoveBranding    Feature = "branding.remove"
	FeatureFullLiveWall      Feature = "live_wall.full"
	FeatureThroughTheMoment  Feature = "live_wall.through_moment"
	FeatureQRSourceAnalytics Feature = "analytics.qr_sources"
)

// Limits are the numeric quotas of a plan. Item limits count successful
// uploads over the event's life; MaxMediaBytes counts bytes currently stored.
// A zero per-type or Live Wall limit means "no limit of that kind".
type Limits struct {
	MaxGuests        int64 `json:"max_guests"`
	MaxMediaItems    int64 `json:"max_media_items"`
	MaxPhotoItems    int64 `json:"max_photo_items"`
	MaxVideoItems    int64 `json:"max_video_items"`
	MaxMediaBytes    int64 `json:"max_media_bytes"`
	MaxLiveWallItems int64 `json:"max_live_wall_items"`
	RetentionDays    int   `json:"retention_days"`
}

// Entitlements is what a plan version grants. It is stored on the version and
// copied verbatim into every grant, so a grant keeps what it was sold with
// after the catalog changes.
type Entitlements struct {
	Features []Feature `json:"features"`
	// FairUse marks plans marketed as unlimited: their item limits are abuse
	// thresholds and are not disclosed in the public catalog.
	FairUse bool   `json:"fair_use"`
	Limits  Limits `json:"limits"`
}

func (e Entitlements) Has(f Feature) bool { return slices.Contains(e.Features, f) }

func (e Entitlements) Value() (driver.Value, error) {
	if e.Features == nil {
		e.Features = []Feature{}
	}
	b, err := json.Marshal(e)
	return string(b), err
}

func (e *Entitlements) Scan(src any) error {
	var raw []byte
	switch v := src.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("catalog: cannot scan %T into Entitlements", src)
	}
	return json.Unmarshal(raw, e)
}

type VersionStatus string

const (
	VersionDraft   VersionStatus = "draft"
	VersionActive  VersionStatus = "active"
	VersionRetired VersionStatus = "retired"
)

// Version is one immutable revision of a plan. Changing what a plan grants
// means publishing a new version, never editing an active one.
type Version struct {
	ID           uuid.UUID     `gorm:"type:uuid;primaryKey"`
	Code         PlanCode      `gorm:"column:code"`
	Version      int           `gorm:"column:version"`
	DisplayName  string        `gorm:"column:display_name"`
	Status       VersionStatus `gorm:"column:status"`
	Entitlements Entitlements  `gorm:"column:entitlements;type:jsonb"`
	CreatedAt    time.Time
	ActivatedAt  *time.Time
	RetiredAt    *time.Time
}

func (Version) TableName() string { return "plan_versions" }

// Price is a display price. It is not tied to any payment provider.
type Price struct {
	ID             uuid.UUID `gorm:"type:uuid;primaryKey"`
	PlanVersionID  uuid.UUID `gorm:"type:uuid"`
	Currency       string
	AmountMinor    int64
	CompareAtMinor *int64
	// ReturningAmountMinor is the loyalty price of this tier, when one is
	// published. It is not shown in the public catalog.
	ReturningAmountMinor *int64 `gorm:"column:returning_amount_minor"`
	ValidFrom            time.Time
	ValidUntil           *time.Time
	CreatedAt            time.Time
}

func (Price) TableName() string { return "plan_prices" }
