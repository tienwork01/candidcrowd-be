package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is returned when a media record is absent or outside the caller's
// event/session scope. HTTP adapters translate it without importing an ORM.
var ErrNotFound = errors.New("media: not found")

type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// Repository is the persistence port for media use cases. Implementations own
// transaction and locking details; the media service remains database-agnostic.
type Repository interface {
	ReserveUpload(ctx context.Context, record Media, maxEventBytes int64) error
	Delete(ctx context.Context, mediaID uuid.UUID) error
	FindUploadForSession(ctx context.Context, mediaID, eventID, sessionID uuid.UUID) (Media, error)
	MarkReady(ctx context.Context, eventID, mediaID uuid.UUID, actualSize int64, uploadedAt time.Time) error
	ListReady(ctx context.Context, eventID uuid.UUID, limit int, before *Cursor) ([]Media, error)
	FindReady(ctx context.Context, eventID, mediaID uuid.UUID) (Media, error)
	FindArchivedLocation(ctx context.Context, eventID, mediaID uuid.UUID) (string, error)
}
