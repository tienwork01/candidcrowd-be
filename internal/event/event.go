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
	StatusDraft  Status = "draft"
	StatusActive Status = "active"
	StatusClosed Status = "closed"
)

type Event struct {
	ID                 uuid.UUID       `gorm:"type:uuid;default:gen_random_uuid();primaryKey" json:"id"`
	HostID             uuid.UUID       `gorm:"type:uuid;not null;index" json:"host_id"`
	Name               string          `json:"name"`
	Slug               string          `gorm:"uniqueIndex" json:"slug"`
	EventDate          *time.Time      `json:"event_date"`
	EventType          string          `json:"event_type"`
	ExpectedGuestCount int             `json:"expected_guest_count"`
	Status             Status          `gorm:"type:event_status" json:"status"`
	GalleryEnabled     bool            `json:"gallery_enabled"`
	GuestTheme         *datatypes.JSON `gorm:"type:jsonb" json:"guest_theme,omitempty"`
	MaxMediaBytes      int64           `json:"max_media_bytes"`
	UsedMediaBytes     int64           `json:"used_media_bytes"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

type Pagination struct {
	Page       int   `json:"page"`
	PerPage    int   `json:"per_page"`
	Total      int64 `json:"total"`
	TotalPages int   `json:"total_pages"`
	HasNext    bool  `json:"has_next"`
	HasPrev    bool  `json:"has_prev"`
}
