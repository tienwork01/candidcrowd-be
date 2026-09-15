package profile

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrEmailUnverified = errors.New("profile: email unverified")
	ErrConsentRequired = errors.New("profile: consent required")
	ErrInvalidConsent  = errors.New("profile: invalid consent")
)

const (
	DocumentTerms   = "terms"
	DocumentPrivacy = "privacy"
)

type Consent struct {
	UserID          uuid.UUID `gorm:"type:uuid;primaryKey"`
	DocumentType    string    `gorm:"primaryKey"`
	DocumentVersion string    `gorm:"primaryKey"`
	AcceptedAt      time.Time
}

func (Consent) TableName() string { return "user_consents" }

type View struct {
	ID                  uuid.UUID `json:"id"`
	Email               string    `json:"email"`
	Name                string    `json:"name"`
	EmailVerified       bool      `json:"email_verified"`
	HasRequiredConsents bool      `json:"has_required_consents"`
}
