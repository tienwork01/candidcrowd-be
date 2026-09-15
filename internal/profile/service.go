package profile

import (
	"context"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	"github.com/candidcrowd/candidcrowd-backend/internal/user"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Service struct {
	db             *gorm.DB
	termsVersion   string
	privacyVersion string
}

func NewService(db *gorm.DB, termsVersion, privacyVersion string) *Service {
	return &Service{db: db, termsVersion: termsVersion, privacyVersion: privacyVersion}
}

func (s *Service) Me(ctx context.Context, identity auth.Identity) (View, error) {
	u, err := s.ensureUser(ctx, identity)
	if err != nil {
		return View{}, err
	}
	hasConsents, err := s.hasRequiredConsents(ctx, u)
	if err != nil {
		return View{}, err
	}
	return View{ID: u.ID, Email: u.Email, Name: u.DisplayName, EmailVerified: u.EmailVerifiedAt != nil, HasRequiredConsents: hasConsents}, nil
}

func (s *Service) Accept(ctx context.Context, identity auth.Identity, termsVersion, privacyVersion string) (View, error) {
	if termsVersion != s.termsVersion || privacyVersion != s.privacyVersion {
		return View{}, ErrInvalidConsent
	}
	u, err := s.ensureUser(ctx, identity)
	if err != nil {
		return View{}, err
	}
	now := time.Now().UTC()
	consents := []Consent{{UserID: u.ID, DocumentType: DocumentTerms, DocumentVersion: termsVersion, AcceptedAt: now}, {UserID: u.ID, DocumentType: DocumentPrivacy, DocumentVersion: privacyVersion, AcceptedAt: now}}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&consents).Error; err != nil {
		return View{}, err
	}
	return s.Me(ctx, identity)
}

func (s *Service) RequireEventCreation(ctx context.Context, identity auth.Identity) error {
	view, err := s.Me(ctx, identity)
	if err != nil {
		return err
	}
	if !view.EmailVerified {
		return ErrEmailUnverified
	}
	if !view.HasRequiredConsents {
		return ErrConsentRequired
	}
	return nil
}

func (s *Service) ensureUser(ctx context.Context, identity auth.Identity) (user.User, error) {
	now := time.Now().UTC()
	updates := map[string]any{"email": identity.Email, "display_name": identity.Name, "last_authenticated_at": now, "updated_at": now}
	if identity.EmailVerified {
		updates["email_verified_at"] = now
	}
	var u user.User
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Where("better_auth_user_id = ?", identity.BetterAuthUserID).First(&u)
		if result.Error == gorm.ErrRecordNotFound {
			u = user.User{BetterAuthUserID: identity.BetterAuthUserID, Email: identity.Email, DisplayName: identity.Name, LastAuthenticatedAt: &now}
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
	if err == nil && identity.TermsVersion == s.termsVersion && identity.PrivacyVersion == s.privacyVersion {
		_, err = s.acceptForUser(ctx, u, identity.TermsVersion, identity.PrivacyVersion)
	}
	return u, err
}

func (s *Service) acceptForUser(ctx context.Context, u user.User, termsVersion, privacyVersion string) (user.User, error) {
	now := time.Now().UTC()
	consents := []Consent{{UserID: u.ID, DocumentType: DocumentTerms, DocumentVersion: termsVersion, AcceptedAt: now}, {UserID: u.ID, DocumentType: DocumentPrivacy, DocumentVersion: privacyVersion, AcceptedAt: now}}
	return u, s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&consents).Error
}

func (s *Service) hasRequiredConsents(ctx context.Context, u user.User) (bool, error) {
	var count int64
	err := s.db.WithContext(ctx).Model(&Consent{}).Where("user_id = ? AND ((document_type = ? AND document_version = ?) OR (document_type = ? AND document_version = ?))", u.ID, DocumentTerms, s.termsVersion, DocumentPrivacy, s.privacyVersion).Count(&count).Error
	return count == 2, err
}
