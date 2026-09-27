package archive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type memoryArchiveRepository struct {
	candidates []Candidate
	pending    []Candidate
	verified   map[uuid.UUID]string
	deleted    map[uuid.UUID]bool
}

func (r *memoryArchiveRepository) ListForArchive(context.Context, time.Time, int) ([]Candidate, error) {
	return r.candidates, nil
}
func (r *memoryArchiveRepository) ListSourceDeletionPending(context.Context, int) ([]Candidate, error) {
	pending := r.pending
	r.pending = nil
	return pending, nil
}
func (r *memoryArchiveRepository) RecordVerified(_ context.Context, id uuid.UUID, location string, _ int64, _ string, _ time.Time) error {
	r.verified[id] = location
	return nil
}
func (r *memoryArchiveRepository) MarkSourceDeleted(_ context.Context, id uuid.UUID, _ string, _ time.Time) error {
	r.deleted[id] = true
	return nil
}

type memorySource struct{ deleted []string }

func (s *memorySource) OpenRead(context.Context, string) (io.ReadCloser, media.ObjectInfo, error) {
	return io.NopCloser(bytes.NewReader([]byte("photo"))), media.ObjectInfo{Size: 5, ContentType: "image/jpeg"}, nil
}
func (s *memorySource) Delete(_ context.Context, key string) error {
	s.deleted = append(s.deleted, key)
	return nil
}

type memoryTarget struct{ objects map[string][]byte }

func (t *memoryTarget) Upload(_ context.Context, _ string, _ string, body io.Reader) (string, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	t.objects["drive-file"] = data
	return "drive-file", nil
}
func (t *memoryTarget) OpenRead(_ context.Context, location string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(t.objects[location])), nil
}
func (t *memoryTarget) Delete(_ context.Context, location string) error {
	delete(t.objects, location)
	return nil
}

func TestRunArchivesAndRetriesSourceDeletionWithoutInfrastructure(t *testing.T) {
	archivedID, retryID := uuid.New(), uuid.New()
	repo := &memoryArchiveRepository{
		candidates: []Candidate{{ID: archivedID, ObjectKey: "r2/new.jpg", OriginalFilename: "new.jpg", MIMEType: "image/jpeg"}},
		pending:    []Candidate{{ID: retryID, ObjectKey: "r2/retry.jpg"}},
		verified:   map[uuid.UUID]string{}, deleted: map[uuid.UUID]bool{},
	}
	source := &memorySource{}
	target := &memoryTarget{objects: map[string][]byte{}}

	require.NoError(t, New(repo, source, target).Run(context.Background(), 50))
	require.Equal(t, "drive-file", repo.verified[archivedID])
	require.True(t, repo.deleted[archivedID])
	require.True(t, repo.deleted[retryID])
	require.ElementsMatch(t, []string{"r2/retry.jpg", "r2/new.jpg"}, source.deleted)
}

// poisonSource fails to delete one specific key.
type poisonSource struct {
	memorySource
	poison string
}

func (s *poisonSource) Delete(ctx context.Context, key string) error {
	if key == s.poison {
		return errors.New("provider rejected the delete")
	}
	return s.memorySource.Delete(ctx, key)
}

// ListSourceDeletionPending orders by verified_at, so a candidate that always
// fails stays at the head of every batch. Stopping the run on it would stall
// archiving for the whole deployment indefinitely.
func TestRunContinuesPastAFailingCandidate(t *testing.T) {
	stuckID, healthyID := uuid.New(), uuid.New()
	repo := &memoryArchiveRepository{
		pending: []Candidate{
			{ID: stuckID, ObjectKey: "r2/stuck.jpg"},
			{ID: healthyID, ObjectKey: "r2/healthy.jpg"},
		},
		verified: map[uuid.UUID]string{}, deleted: map[uuid.UUID]bool{},
	}
	source := &poisonSource{poison: "r2/stuck.jpg"}
	target := &memoryTarget{objects: map[string][]byte{}}

	err := New(repo, source, target).Run(context.Background(), 50)
	require.Error(t, err, "the run must report the failure")
	require.Contains(t, err.Error(), "1 of 2")

	require.False(t, repo.deleted[stuckID])
	require.True(t, repo.deleted[healthyID], "the healthy candidate must still be processed")
	require.Equal(t, []string{"r2/healthy.jpg"}, source.deleted)
}
