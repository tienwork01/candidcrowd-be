package media

import (
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusPending Status = "pending"
	StatusReady   Status = "ready"
	StatusFailed  Status = "failed"
)

type Media struct {
	ID               uuid.UUID `gorm:"type:uuid;default:gen_random_uuid();primaryKey"`
	EventID          uuid.UUID `gorm:"type:uuid;not null;index"`
	GuestSessionID   uuid.UUID `gorm:"type:uuid;not null"`
	ObjectKey        string    `gorm:"uniqueIndex"`
	OriginalFilename string
	MIMEType         string
	ExpectedSize     int64
	ActualSize       *int64
	Status           Status `gorm:"type:media_status"`
	CreatedAt        time.Time
	UploadedAt       *time.Time
}
