package event

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// ErrNotFound is a domain-facing repository result. Infrastructure adapters
// map their persistence-specific not-found errors to this value.
var ErrNotFound = errors.New("event: not found")

type Status string

const (
	StatusDraft   Status = "draft"
	StatusActive  Status = "active"
	StatusClosed  Status = "closed"
	StatusDeleted Status = "deleted"
)

type Event struct {
	ID                  uuid.UUID       `gorm:"type:uuid;default:gen_random_uuid();primaryKey" json:"id"`
	HostID              uuid.UUID       `gorm:"type:uuid;not null;index" json:"host_id"`
	ClientRequestID     *uuid.UUID      `gorm:"type:uuid" json:"-"`
	Name                string          `json:"name"`
	Slug                string          `gorm:"uniqueIndex" json:"slug"`
	EventDate           *time.Time      `json:"event_date"`
	EventType           string          `json:"event_type"`
	ExpectedGuestCount  int             `json:"expected_guest_count"`
	Status              Status          `gorm:"type:event_status" json:"status"`
	GalleryEnabled      bool            `json:"gallery_enabled"`
	LifecyclePhase      *string         `json:"lifecycle_phase,omitempty"`
	SetupChecklist      datatypes.JSON  `gorm:"type:jsonb" json:"setup_checklist"`
	CandidCameraEnabled bool            `json:"candid_camera_enabled"`
	GuestTheme          *datatypes.JSON `gorm:"type:jsonb" json:"guest_theme,omitempty"`
	QRConfig            *datatypes.JSON `gorm:"type:jsonb" json:"qr_config,omitempty"`
	MaxMediaBytes       int64           `json:"max_media_bytes"`
	UsedMediaBytes      int64           `json:"used_media_bytes"`
	// ReservedMediaBytes counts quota taken by uploads that have not completed.
	// It is released by MarkReady, ExpireStale and Delete.
	ReservedMediaBytes int64      `json:"reserved_media_bytes"`
	TrialStartedAt     *time.Time `json:"-"`
	TrialEndedAt       *time.Time `json:"-"`
	// BillingStatus parks an event after a refund or chargeback. It is ops
	// state, never shown to the host, and nothing in the product reads it to
	// restrict an event: a reversal must not quietly take a gallery away.
	// The default tag matters: without it GORM writes the empty Go string on
	// insert and the column's CHECK rejects every new event.
	BillingStatus string    `gorm:"column:billing_status;default:ok" json:"-"`
	Metrics       *Metrics  `gorm:"-" json:"metrics,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Metrics struct {
	ScansCount        int64 `json:"scans_count"`
	VisitorsCount     int64 `json:"visitors_count"`
	ContributorsCount int64 `json:"contributors_count"`
	PhotosCount       int64 `json:"photos_count"`
	VideosCount       int64 `json:"videos_count"`
	ParticipationRate int   `json:"participation_rate"`
}

type Pagination struct {
	Page       int   `json:"page"`
	PerPage    int   `json:"per_page"`
	Total      int64 `json:"total"`
	TotalPages int   `json:"total_pages"`
	HasNext    bool  `json:"has_next"`
	HasPrev    bool  `json:"has_prev"`
}
