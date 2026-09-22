package archive

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/google/uuid"
)

// Source is the hot-storage port. R2 is the current adapter.
type Source interface {
	OpenRead(context.Context, string) (io.ReadCloser, media.ObjectInfo, error)
	Delete(context.Context, string) error
}

// Target is the long-term archive port. Google Drive is the current adapter.
type Target interface {
	Upload(context.Context, string, string, io.Reader) (string, error)
	OpenRead(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
}

type Candidate struct {
	ID               uuid.UUID
	ObjectKey        string
	OriginalFilename string
	MIMEType         string
}

// Repository owns replica state transitions. The archive use case never knows
// SQL details or how storage_replicas is represented.
type Repository interface {
	ListForArchive(ctx context.Context, olderThan time.Time, limit int) ([]Candidate, error)
	ListSourceDeletionPending(ctx context.Context, limit int) ([]Candidate, error)
	RecordVerified(ctx context.Context, mediaID uuid.UUID, location string, size int64, checksum string, verifiedAt time.Time) error
	MarkSourceDeleted(ctx context.Context, mediaID uuid.UUID, objectKey string, deletedAt time.Time) error
}

type Service struct {
	repo   Repository
	source Source
	target Target
	minAge time.Duration
}

func New(repo Repository, source Source, target Target, minAges ...time.Duration) *Service {
	s := &Service{repo: repo, source: source, target: target, minAge: 24 * time.Hour}
	if len(minAges) > 0 {
		s.minAge = minAges[0]
	}
	return s
}

func (s *Service) Run(ctx context.Context, limit int) error {
	if limit < 1 {
		limit = 50
	}
	// A prior run may have verified Drive but failed while deleting R2. Retry
	// this idempotent operation before creating new archive copies.
	pending, err := s.repo.ListSourceDeletionPending(ctx, limit)
	if err != nil {
		return err
	}
	for _, candidate := range pending {
		if err := s.deleteSource(ctx, candidate); err != nil {
			return err
		}
	}

	candidates, err := s.repo.ListForArchive(ctx, time.Now().Add(-s.minAge), limit)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if err := s.archive(ctx, candidate); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) archive(ctx context.Context, candidate Candidate) error {
	body, info, err := s.source.OpenRead(ctx, candidate.ObjectKey)
	if err != nil {
		return err
	}
	defer body.Close()

	hash := sha256.New()
	location, err := s.target.Upload(ctx, candidate.OriginalFilename, candidate.MIMEType, io.TeeReader(body, hash))
	if err != nil {
		return err
	}
	checksum := fmt.Sprintf("%x", hash.Sum(nil))
	verified, err := s.target.OpenRead(ctx, location)
	if err != nil {
		_ = s.target.Delete(ctx, location)
		return err
	}
	verifiedHash := sha256.New()
	_, copyErr := io.Copy(verifiedHash, verified)
	closeErr := verified.Close()
	if copyErr != nil || closeErr != nil || checksum != fmt.Sprintf("%x", verifiedHash.Sum(nil)) {
		_ = s.target.Delete(ctx, location)
		return fmt.Errorf("archive checksum mismatch")
	}

	if err := s.repo.RecordVerified(ctx, candidate.ID, location, info.Size, checksum, time.Now()); err != nil {
		return err
	}
	return s.deleteSource(ctx, candidate)
}

func (s *Service) deleteSource(ctx context.Context, candidate Candidate) error {
	if err := s.source.Delete(ctx, candidate.ObjectKey); err != nil {
		return err
	}
	return s.repo.MarkSourceDeleted(ctx, candidate.ID, candidate.ObjectKey, time.Now())
}
