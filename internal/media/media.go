package media

import (
	"context"
	"strings"
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
	StatusFeatured   Status = "featured"
	StatusHidden     Status = "hidden"
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
	ThumbnailReady bool `gorm:"not null;default:false"`
	// SourceDeletedAt is set once the original has been archived and removed
	// from hot storage. Its object key no longer resolves, so such a record
	// must be served through the application rather than by a presigned URL.
	// The thumbnail is not archived and remains available either way.
	SourceDeletedAt *time.Time
}

// SourceArchived reports that the original is no longer in hot storage.
func (m Media) SourceArchived() bool { return m.SourceDeletedAt != nil }

type GalleryFilter string

const (
	FilterAll       GalleryFilter = "all"
	FilterPhotos    GalleryFilter = "photos"
	FilterVideos    GalleryFilter = "videos"
	FilterFavorites GalleryFilter = "favorites"
	FilterHidden    GalleryFilter = "hidden"
)

type GallerySort string

const (
	SortNewest GallerySort = "newest"
	SortOldest GallerySort = "oldest"
)

type GalleryCounts struct {
	All       int64 `json:"all"`
	Photos    int64 `json:"photos"`
	Videos    int64 `json:"videos"`
	Favorites int64 `json:"favorites"`
	Hidden    int64 `json:"hidden"`
}

// ChangeKind names what happened to media in an event.
type ChangeKind string

const (
	ChangeCreated   ChangeKind = "created"
	ChangeThumbnail ChangeKind = "thumbnail"
	ChangeModerated ChangeKind = "moderated"
	ChangeDeleted   ChangeKind = "deleted"
)

// Item is the projection a browser needs to place a new media record in a
// gallery. It carries no URL: a host URL must be presigned per request, and a
// guest URL is a stable route the client already knows how to build from the
// event slug. Keeping both out means no storage key ever leaves this process.
type Item struct {
	ID             uuid.UUID `json:"id"`
	MIMEType       string    `json:"mime_type"`
	CreatedAt      time.Time `json:"created_at"`
	IsVideo        bool      `json:"is_video"`
	HasEventFrame  bool      `json:"has_event_frame"`
	ThumbnailReady bool      `json:"thumbnail_ready"`
	Status         Status    `json:"status"`
}

// Change describes a media change worth telling connected browsers about.
type Change struct {
	Kind    ChangeKind
	EventID uuid.UUID
	IDs     []uuid.UUID
	Status  Status
	Item    *Item
}

// Notifier receives changes for broadcast. Implementations must return
// promptly: a guest's upload confirmation is never allowed to wait on a
// broadcast, so any network work belongs out of band.
type Notifier interface {
	MediaChanged(ctx context.Context, change Change)
}

func newItem(m Media) Item {
	return Item{
		ID:             m.ID,
		MIMEType:       m.MIMEType,
		CreatedAt:      m.CreatedAt,
		IsVideo:        strings.HasPrefix(m.MIMEType, "video/"),
		HasEventFrame:  strings.HasPrefix(m.OriginalFilename, "candid_"),
		ThumbnailReady: m.ThumbnailReady,
		Status:         m.Status,
	}
}

// PublicView carries no bucket or object key. Its URL is either a presigned,
// short-lived link to the object or a stable application route that resolves
// whichever replica currently holds the media; the caller cannot tell which
// storage provider answered, and neither can it be made to.
type PublicView struct {
	ID       uuid.UUID `json:"id"`
	URL      string    `json:"url"`
	MIMEType string    `json:"mime_type"`
	// ThumbnailURL is empty until the variant has been rendered.
	ThumbnailURL   string    `json:"thumbnail_url"`
	CreatedAt      time.Time `json:"created_at"`
	IsVideo        bool      `json:"is_video"`
	HasEventFrame  bool      `json:"has_event_frame"`
	ThumbnailReady bool      `json:"thumbnail_ready"`
	Status         Status    `json:"status"`
	objectKey      string
	thumbnailKey   string
	// routeURL is the application route kept as the fallback for media whose
	// original has been archived out of hot storage.
	routeURL string
	// sourceArchived mirrors Media.SourceDeletedAt for this view.
	sourceArchived bool
}
