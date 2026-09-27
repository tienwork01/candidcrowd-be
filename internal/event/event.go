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
	Name                string          `json:"name"`
	Slug                string          `gorm:"uniqueIndex" json:"slug"`
	EventDate           *time.Time      `json:"event_date"`
	EventType           string          `json:"event_type"`
	ExpectedGuestCount  int             `json:"expected_guest_count"`
	Status              Status          `gorm:"type:event_status" json:"status"`
	GalleryEnabled      bool            `json:"gallery_enabled"`
	EventMode           string          `json:"event_mode"`
	LifecyclePhase      *string         `json:"lifecycle_phase,omitempty"`
	SetupChecklist      datatypes.JSON  `gorm:"type:jsonb" json:"setup_checklist"`
	CandidCameraEnabled bool            `json:"candid_camera_enabled"`
	GuestTheme          *datatypes.JSON `gorm:"type:jsonb" json:"guest_theme,omitempty"`
	MaxMediaBytes       int64           `json:"max_media_bytes"`
	UsedMediaBytes      int64           `json:"used_media_bytes"`
	// ReservedMediaBytes counts quota taken by uploads that have not completed.
	// It is released by MarkReady, ExpireStale and Delete.
	ReservedMediaBytes int64     `json:"reserved_media_bytes"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type Pagination struct {
	Page       int   `json:"page"`
	PerPage    int   `json:"per_page"`
	Total      int64 `json:"total"`
	TotalPages int   `json:"total_pages"`
	HasNext    bool  `json:"has_next"`
	HasPrev    bool  `json:"has_prev"`
}
