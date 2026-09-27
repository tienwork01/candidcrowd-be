package livewall

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type createCaptureRepository struct {
	created Session
}

func (r *createCaptureRepository) Create(_ context.Context, session Session) (Session, error) {
	r.created = session
	return session, nil
}
func (r *createCaptureRepository) FindOwned(context.Context, uuid.UUID, uuid.UUID) (Session, error) {
	return Session{}, ErrNotFound
}
func (r *createCaptureRepository) FindToken(context.Context, string, time.Time) (Session, error) {
	return Session{}, ErrNotFound
}
func (r *createCaptureRepository) End(context.Context, uuid.UUID, uuid.UUID, Status, time.Time) (Session, error) {
	return Session{}, ErrNotFound
}
func (r *createCaptureRepository) Command(context.Context, uuid.UUID, uuid.UUID, Command) (Session, error) {
	return Session{}, ErrNotFound
}
func (r *createCaptureRepository) UpdateContentPolicy(context.Context, uuid.UUID, uuid.UUID, ContentPolicy) (Session, error) {
	return Session{}, ErrNotFound
}
func (r *createCaptureRepository) UpdateCTAEveryMedia(context.Context, uuid.UUID, uuid.UUID, int) (Session, error) {
	return Session{}, ErrNotFound
}

func TestSessionTableName(t *testing.T) {
	if got, want := (Session{}).TableName(), "live_wall_sessions"; got != want {
		t.Fatalf("Session.TableName() = %q, want %q", got, want)
	}
}

func TestCreateDefaultsToAutoApprovedMedia(t *testing.T) {
	repo := &createCaptureRepository{}
	service := NewService(repo, 0)

	_, _, err := service.Create(t.Context(), uuid.New(), "Event", "event")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if got, want := repo.created.ContentPolicy, ContentPolicyAutoApproved; got != want {
		t.Fatalf("created content policy = %q, want %q", got, want)
	}
	if got, want := repo.created.CTAEveryMedia, DefaultCTAEveryMedia; got != want {
		t.Fatalf("created CTA cadence = %d, want %d", got, want)
	}
}

func TestUpdateCTAEveryMediaRejectsUnsafeCadence(t *testing.T) {
	service := NewService(&createCaptureRepository{}, 0)

	_, err := service.UpdateCTAEveryMedia(
		t.Context(),
		uuid.New(),
		uuid.New(),
		MinCTAEveryMedia-1,
	)
	if !errors.Is(err, ErrInvalidCTAEveryMedia) {
		t.Fatalf("UpdateCTAEveryMedia() error = %v, want %v", err, ErrInvalidCTAEveryMedia)
	}
}
