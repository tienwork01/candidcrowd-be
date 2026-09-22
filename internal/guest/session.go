package guest

import (
	"time"

	"github.com/google/uuid"
)

type Session struct {
	ID        uuid.UUID `gorm:"type:uuid;default:gen_random_uuid();primaryKey"`
	EventID   uuid.UUID `gorm:"type:uuid;not null;index"`
	TokenHash []byte    `gorm:"uniqueIndex;not null"`
	ExpiresAt time.Time
	CreatedAt time.Time
}

// TableName maps the domain name to the existing database table. Without this,
// GORM pluralizes Session as "sessions", while the migration creates
// "guest_sessions".
func (Session) TableName() string {
	return "guest_sessions"
}
