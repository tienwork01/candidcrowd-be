package media

import (
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusPending    Status = "pending"
	StatusUploading  Status = "uploading"
	StatusUploaded   Status = "uploaded"
	StatusProcessing Status = "processing"
	StatusReady      Status = "ready"
	StatusFailed     Status = "failed"
	StatusDeleted    Status = "deleted"
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
	ChecksumSHA256   string `gorm:"column:checksum_sha256"`
	// ClientUploadID is generated once by the browser and makes target creation
	// safe to retry when a response is lost on a flaky connection.
	ClientUploadID *uuid.UUID `gorm:"column:client_upload_id"`
	Status         Status     `gorm:"type:media_status"`
	CreatedAt      time.Time
	LastActivityAt time.Time
	UploadedAt     *time.Time
	FailedAt       *time.Time
}

// PublicView deliberately contains no provider URL, bucket, or object key.
// Its URL is a stable application route that can resolve any active replica.
type PublicView struct {
	ID        uuid.UUID `json:"id"`
	URL       string    `json:"url"`
	MIMEType  string    `json:"mime_type"`
	CreatedAt time.Time `json:"created_at"`
	IsVideo   bool      `json:"is_video"`
}
