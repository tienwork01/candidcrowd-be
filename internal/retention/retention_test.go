package retention

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type fakeRepo struct {
	candidates []Candidate
	replicas   map[uuid.UUID][]Replica
	deleted    []Replica
	purged     []uuid.UUID
	cutoff     time.Time
}

func (r *fakeRepo) Candidates(_ context.Context, cutoff time.Time, limit int) ([]Candidate, error) {
	r.cutoff = cutoff
	var out []Candidate
	for _, c := range r.candidates {
		if !c.RetentionExpiresAt.After(cutoff) && len(out) < limit {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r *fakeRepo) ArchivedReplicas(_ context.Context, eventID uuid.UUID) ([]Replica, error) {
	return r.replicas[eventID], nil
}

func (r *fakeRepo) MarkReplicaDeleted(_ context.Context, replica Replica, _ time.Time) error {
	r.deleted = append(r.deleted, replica)
	return nil
}

func (r *fakeRepo) MarkPurged(_ context.Context, eventID uuid.UUID, _ time.Time) (Purged, error) {
	r.purged = append(r.purged, eventID)
	return Purged{Media: 3}, nil
}

type fakeObjects struct {
	prefixes []string
	failFor  string
}

func (o *fakeObjects) DeletePrefix(_ context.Context, prefix string) (int, error) {
	if prefix == o.failFor {
		return 0, errors.New("storage unavailable")
	}
	o.prefixes = append(o.prefixes, prefix)
	return 1, nil
}

type fakeArchive struct{ deleted []string }

func (a *fakeArchive) Delete(_ context.Context, location string) error {
	a.deleted = append(a.deleted, location)
	return nil
}

var now = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

func service(repo *fakeRepo, objects *fakeObjects, opts ...Option) *Service {
	s := NewService(repo, objects, opts...)
	s.now = func() time.Time { return now }
	return s
}

func TestRunWithoutEnforcementOnlyReports(t *testing.T) {
	due := Candidate{EventID: uuid.New(), RetentionExpiresAt: now.Add(-31 * 24 * time.Hour)}
	repo := &fakeRepo{candidates: []Candidate{due}}
	objects := &fakeObjects{}

	report, err := service(repo, objects).Run(context.Background(), 10, false)
	require.NoError(t, err)
	require.Equal(t, 1, report.Due)
	require.Zero(t, report.Purged)
	require.Empty(t, objects.prefixes, "nothing is deleted while enforcement is off")
	require.Empty(t, repo.purged)
}

func TestRunHonoursTheGracePeriod(t *testing.T) {
	inGrace := Candidate{EventID: uuid.New(), RetentionExpiresAt: now.Add(-10 * 24 * time.Hour)}
	pastGrace := Candidate{EventID: uuid.New(), RetentionExpiresAt: now.Add(-31 * 24 * time.Hour)}
	repo := &fakeRepo{candidates: []Candidate{inGrace, pastGrace}}
	objects := &fakeObjects{}

	report, err := service(repo, objects, WithEnforcement(true)).Run(context.Background(), 10, false)
	require.NoError(t, err)
	require.Equal(t, now.Add(-DefaultGrace), repo.cutoff)
	require.Equal(t, 1, report.Purged)
	require.Equal(t, []uuid.UUID{pastGrace.EventID}, repo.purged)
	require.Equal(t, []string{"events/" + pastGrace.EventID.String() + "/"}, objects.prefixes)
}

func TestDryRunNeverDeletesEvenWhenEnforced(t *testing.T) {
	repo := &fakeRepo{candidates: []Candidate{{EventID: uuid.New(), RetentionExpiresAt: now.AddDate(-1, 0, 0)}}}
	objects := &fakeObjects{}
	report, err := service(repo, objects, WithEnforcement(true)).Run(context.Background(), 10, true)
	require.NoError(t, err)
	require.Equal(t, 1, report.Due)
	require.Empty(t, objects.prefixes)
	require.Len(t, report.Candidates, 1)
}

func TestArchiveCopiesAreRemovedFirstOrThePurgeStops(t *testing.T) {
	eventID := uuid.New()
	replica := Replica{MediaID: uuid.New(), Location: "drive-file-1"}
	candidate := Candidate{EventID: eventID, RetentionExpiresAt: now.AddDate(-1, 0, 0)}

	repo := &fakeRepo{candidates: []Candidate{candidate}, replicas: map[uuid.UUID][]Replica{eventID: {replica}}}
	objects := &fakeObjects{}
	report, err := service(repo, objects, WithEnforcement(true)).Run(context.Background(), 10, false)
	require.NoError(t, err)
	require.Len(t, report.Failed, 1)
	require.ErrorIs(t, report.Failed[0].Err, ErrArchiveUnavailable)
	require.Empty(t, objects.prefixes, "without an archive client nothing is deleted")

	archive := &fakeArchive{}
	report, err = service(repo, objects, WithEnforcement(true), WithArchive(archive)).Run(context.Background(), 10, false)
	require.NoError(t, err)
	require.Equal(t, 1, report.Purged)
	require.Equal(t, []string{"drive-file-1"}, archive.deleted)
	require.Equal(t, []Replica{replica}, repo.deleted)
}

func TestFailedStorageDeletionLeavesTheEventForTheNextRun(t *testing.T) {
	failing := Candidate{EventID: uuid.New(), RetentionExpiresAt: now.AddDate(-1, 0, 0)}
	ok := Candidate{EventID: uuid.New(), RetentionExpiresAt: now.AddDate(-1, 0, 0)}
	repo := &fakeRepo{candidates: []Candidate{failing, ok}}
	objects := &fakeObjects{failFor: EventPrefix(failing.EventID)}

	report, err := service(repo, objects, WithEnforcement(true)).Run(context.Background(), 10, false)
	require.NoError(t, err)
	require.Equal(t, 1, report.Purged)
	require.Len(t, report.Failed, 1)
	require.Equal(t, []uuid.UUID{ok.EventID}, repo.purged, "a failed event is not marked purged")
}

func TestEventPrefixIsScoped(t *testing.T) {
	id := uuid.New()
	require.Equal(t, "events/"+id.String()+"/", EventPrefix(id))
}
