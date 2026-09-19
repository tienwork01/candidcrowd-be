package profile

import (
	"context"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	"github.com/candidcrowd/candidcrowd-backend/internal/user"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Repository interface {
	EnsureUser(ctx context.Context, identity auth.Identity) (user.User, error)
	SaveConsents(ctx context.Context, consents []Consent) error
	HasRequiredConsents(ctx context.Context, userID uuid.UUID, termsVersion, privacyVersion string) (bool, error)
}

type gormRepository struct {
	db *gorm.DB
}

func NewGormRepository(db *gorm.DB) Repository {
	return &gormRepository{db: db}
}

func (r *gormRepository) EnsureUser(ctx context.Context, identity auth.Identity) (user.User, error) {
	now := time.Now().UTC()
	updates := map[string]any{
		"email":                 identity.Email,
		"display_name":          identity.Name,
		"last_authenticated_at": now,
		"updated_at":            now,
	}
	if identity.EmailVerified {
		updates["email_verified_at"] = now
	}
	var u user.User
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Where("better_auth_user_id = ?", identity.BetterAuthUserID).First(&u)
		if result.Error == gorm.ErrRecordNotFound {
			u = user.User{
				BetterAuthUserID:    identity.BetterAuthUserID,
				Email:               identity.Email,
				DisplayName:         identity.Name,
				LastAuthenticatedAt: &now,
			}
			if identity.EmailVerified {
				u.EmailVerifiedAt = &now
			}
			return tx.Create(&u).Error
		}
		if result.Error != nil {
			return result.Error
		}
		if err := tx.Model(&u).Updates(updates).Error; err != nil {
			return err
		}
		return tx.First(&u, "id = ?", u.ID).Error
	})
	return u, err
}

func (r *gormRepository) SaveConsents(ctx context.Context, consents []Consent) error {
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&consents).Error
}

func (r *gormRepository) HasRequiredConsents(ctx context.Context, userID uuid.UUID, termsVersion, privacyVersion string) (bool, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&Consent{}).Where("user_id = ? AND ((document_type = ? AND document_version = ?) OR (document_type = ? AND document_version = ?))", userID, DocumentTerms, termsVersion, DocumentPrivacy, privacyVersion).Count(&count).Error
	return count == 2, err
}
