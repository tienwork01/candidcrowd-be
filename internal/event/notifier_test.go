package event

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type recordingEventRepo struct {
	updated Event
	err     error
}

func (r *recordingEventRepo) Create(context.Context, *Event) error { return nil }
func (r *recordingEventRepo) GetByClientRequest(context.Context, uuid.UUID, uuid.UUID) (Event, error) {
	return Event{}, ErrNotFound
}
func (r *recordingEventRepo) ListByHost(context.Context, uuid.UUID, ListFilter) ([]Event, int64, error) {
	return nil, 0, nil
}
func (r *recordingEventRepo) GetOwned(context.Context, uuid.UUID, uuid.UUID) (Event, error) {
	return r.updated, nil
}
func (r *recordingEventRepo) GetPublic(context.Context, string) (Event, error) {
	return r.updated, nil
}
func (r *recordingEventRepo) UpdateOwned(context.Context, uuid.UUID, uuid.UUID, map[string]interface{}) (Event, error) {
	if r.err != nil {
		return Event{}, r.err
	}
	return r.updated, nil
}
func (r *recordingEventRepo) DeleteOwned(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (r *recordingEventRepo) CloseOwned(context.Context, uuid.UUID, uuid.UUID) error  { return nil }
func (r *recordingEventRepo) CountActiveTrialsByHost(context.Context, uuid.UUID) (int64, error) {
	return 0, nil
}
func (r *recordingEventRepo) CountTrialsSince(context.Context, uuid.UUID, time.Time) (int64, error) {
	return 0, nil
}

type recordingEventNotifier struct {
	mu      sync.Mutex
	changes []Change
}

func (n *recordingEventNotifier) EventChanged(_ context.Context, change Change) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.changes = append(n.changes, change)
}

func (n *recordingEventNotifier) all() []Change {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Change(nil), n.changes...)
}

func TestUpdateAnnouncesSettingsALivePageReactsTo(t *testing.T) {
	eventID := uuid.New()
	repo := &recordingEventRepo{updated: Event{ID: eventID, GalleryEnabled: false, Status: StatusActive}}
	notifier := &recordingEventNotifier{}
	service := NewService(repo, 1<<30, notifier)

	disabled := false
	_, err := service.Update(context.Background(), eventID, uuid.New(), UpdateInput{GalleryEnabled: &disabled})
	require.NoError(t, err)

	changes := notifier.all()
	require.Len(t, changes, 1)
	require.Equal(t, eventID, changes[0].EventID)
	require.False(t, changes[0].GalleryEnabled, "guest pages must learn the gallery closed without a reload")
}

func TestUpdateStaysQuietForSettingsNoBrowserRedraws(t *testing.T) {
	eventID := uuid.New()
	repo := &recordingEventRepo{updated: Event{ID: eventID, GalleryEnabled: true}}
	notifier := &recordingEventNotifier{}
	service := NewService(repo, 1<<30, notifier)

	name := "Renamed party"
	count := 120
	_, err := service.Update(context.Background(), eventID, uuid.New(), UpdateInput{Name: &name, ExpectedGuestCount: &count})
	require.NoError(t, err)

	require.Empty(t, notifier.all(), "a rename must not wake every connected browser")
}

func TestFailedUpdateIsNotAnnounced(t *testing.T) {
	repo := &recordingEventRepo{err: ErrNotFound}
	notifier := &recordingEventNotifier{}
	service := NewService(repo, 1<<30, notifier)

	disabled := false
	_, err := service.Update(context.Background(), uuid.New(), uuid.New(), UpdateInput{GalleryEnabled: &disabled})
	require.ErrorIs(t, err, ErrNotFound)
	require.Empty(t, notifier.all())
}
