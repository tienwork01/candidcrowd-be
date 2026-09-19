package profile

import (
	"context"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
)

type Service struct {
	repo           Repository
	termsVersion   string
	privacyVersion string
}

func NewService(repo Repository, termsVersion, privacyVersion string) *Service {
	return &Service{repo: repo, termsVersion: termsVersion, privacyVersion: privacyVersion}
}

func (s *Service) Me(ctx context.Context, identity auth.Identity) (View, error) {
	u, err := s.repo.EnsureUser(ctx, identity)
	if err != nil {
		return View{}, err
	}
	if identity.TermsVersion == s.termsVersion && identity.PrivacyVersion == s.privacyVersion {
		now := time.Now().UTC()
		consents := []Consent{
			{UserID: u.ID, DocumentType: DocumentTerms, DocumentVersion: s.termsVersion, AcceptedAt: now},
			{UserID: u.ID, DocumentType: DocumentPrivacy, DocumentVersion: s.privacyVersion, AcceptedAt: now},
		}
		_ = s.repo.SaveConsents(ctx, consents)
	}
	hasConsents, err := s.repo.HasRequiredConsents(ctx, u.ID, s.termsVersion, s.privacyVersion)
	if err != nil {
		return View{}, err
	}
	return View{
		ID:                  u.ID,
		Email:               u.Email,
		Name:                u.DisplayName,
		EmailVerified:       u.EmailVerifiedAt != nil,
		HasRequiredConsents: hasConsents,
	}, nil
}

func (s *Service) Accept(ctx context.Context, identity auth.Identity, termsVersion, privacyVersion string) (View, error) {
	if termsVersion != s.termsVersion || privacyVersion != s.privacyVersion {
		return View{}, ErrInvalidConsent
	}
	u, err := s.repo.EnsureUser(ctx, identity)
	if err != nil {
		return View{}, err
	}
	now := time.Now().UTC()
	consents := []Consent{
		{UserID: u.ID, DocumentType: DocumentTerms, DocumentVersion: termsVersion, AcceptedAt: now},
		{UserID: u.ID, DocumentType: DocumentPrivacy, DocumentVersion: privacyVersion, AcceptedAt: now},
	}
	if err := s.repo.SaveConsents(ctx, consents); err != nil {
		return View{}, err
	}
	return s.Me(ctx, identity)
}

func (s *Service) RequireEventCreation(ctx context.Context, identity auth.Identity) (View, error) {
	view, err := s.Me(ctx, identity)
	if err != nil {
		return View{}, err
	}
	if !view.EmailVerified {
		return View{}, ErrEmailUnverified
	}
	if !view.HasRequiredConsents {
		return View{}, ErrConsentRequired
	}
	return view, nil
}
