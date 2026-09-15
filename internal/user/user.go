package user

import (
	"time"

	"github.com/google/uuid"
)

type User struct {
	ID                  uuid.UUID `gorm:"type:uuid;default:gen_random_uuid();primaryKey"`
	BetterAuthUserID    string    `gorm:"uniqueIndex;not null"`
	Email               string    `gorm:"not null"`
	DisplayName         string    `gorm:"not null"`
	EmailVerifiedAt     *time.Time
	LastAuthenticatedAt *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}
