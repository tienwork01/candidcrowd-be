package realtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func decodePayload(t *testing.T, msg Message) mediaPayload {
	t.Helper()
	var payload mediaPayload
	require.NoError(t, json.Unmarshal(msg.Data, &payload))
	return payload
}

func sampleItem(id uuid.UUID, status media.Status) *media.Item {
	return &media.Item{
		ID:        id,
		MIMEType:  "image/jpeg",
		CreatedAt: time.Now().UTC(),
		Status:    status,
	}
}

func TestMediaMessagesForNewUpload(t *testing.T) {
	eventID, mediaID := uuid.New(), uuid.New()

	messages, err := mediaMessages(media.Change{
		Kind:    media.ChangeCreated,
		EventID: eventID,
		IDs:     []uuid.UUID{mediaID},
		Status:  media.StatusReady,
		Item:    sampleItem(mediaID, media.StatusReady),
	})
	require.NoError(t, err)
	require.Len(t, messages, 1, "a ready upload is public-safe, so one message serves both audiences")

	msg := messages[0]
	require.Equal(t, KindMediaCreated, msg.Kind)
	require.Equal(t, eventID, msg.EventID)
	require.True(t, msg.Audience.Includes(AudienceHost))
	require.True(t, msg.Audience.Includes(AudiencePublic))

	payload := decodePayload(t, msg)
	require.Equal(t, []uuid.UUID{mediaID}, payload.IDs)
	require.NotNil(t, payload.Item, "a guest gallery inserts from this payload without refetching")
	require.Equal(t, "image/jpeg", payload.Item.MIMEType)

	// No storage location may ever cross the bus.
	require.NotContains(t, string(msg.Data), "object_key")
	require.NotContains(t, string(msg.Data), "events/")
}

func TestMediaMessagesForThumbnailAndDelete(t *testing.T) {
	eventID, mediaID := uuid.New(), uuid.New()

	thumbnail, err := mediaMessages(media.Change{Kind: media.ChangeThumbnail, EventID: eventID, IDs: []uuid.UUID{mediaID}})
	require.NoError(t, err)
	require.Len(t, thumbnail, 1)
	require.Equal(t, KindMediaThumbnail, thumbnail[0].Kind)
	require.True(t, decodePayload(t, thumbnail[0]).ThumbnailReady)

	deleted, err := mediaMessages(media.Change{Kind: media.ChangeDeleted, EventID: eventID, IDs: []uuid.UUID{mediaID}})
	require.NoError(t, err)
	require.Len(t, deleted, 1)
	require.Equal(t, KindMediaDeleted, deleted[0].Kind)
	require.True(t, deleted[0].Audience.Includes(AudiencePublic), "a guest gallery must drop deleted media at once")
	require.Equal(t, []uuid.UUID{mediaID}, decodePayload(t, deleted[0]).IDs)
}

func TestHidingMediaReachesGuestsAsADeletion(t *testing.T) {
	eventID, mediaID := uuid.New(), uuid.New()

	messages, err := mediaMessages(media.Change{
		Kind:    media.ChangeModerated,
		EventID: eventID,
		IDs:     []uuid.UUID{mediaID},
		Status:  media.StatusHidden,
		Item:    sampleItem(mediaID, media.StatusHidden),
	})
	require.NoError(t, err)
	require.Len(t, messages, 2)

	host, guest := messages[0], messages[1]

	require.Equal(t, KindMediaUpdated, host.Kind)
	require.Equal(t, AudienceHost, host.Audience, "hidden media is host-only")
	require.Equal(t, media.StatusHidden, decodePayload(t, host).Status)

	// The guest side is the privacy-critical half: guests learn the item is
	// gone and learn nothing else about it.
	require.Equal(t, KindMediaDeleted, guest.Kind)
	require.Equal(t, AudiencePublic, guest.Audience)
	guestPayload := decodePayload(t, guest)
	require.Equal(t, []uuid.UUID{mediaID}, guestPayload.IDs)
	require.Nil(t, guestPayload.Item, "a hidden item's metadata must not reach guests")
	require.NotContains(t, string(guest.Data), string(media.StatusHidden))
}

func TestFeaturingMediaIsNotLeakedToGuests(t *testing.T) {
	eventID, mediaID := uuid.New(), uuid.New()

	messages, err := mediaMessages(media.Change{
		Kind:    media.ChangeModerated,
		EventID: eventID,
		IDs:     []uuid.UUID{mediaID},
		Status:  media.StatusFeatured,
		Item:    sampleItem(mediaID, media.StatusFeatured),
	})
	require.NoError(t, err)
	require.Len(t, messages, 2)

	host, guest := messages[0], messages[1]
	require.Equal(t, media.StatusFeatured, decodePayload(t, host).Status)

	// Favouriting is host curation. A guest is told only that the item is
	// visible, and gets no item payload, so the client resolves it by
	// refetching rather than trusting a partial record.
	require.Equal(t, KindMediaUpdated, guest.Kind)
	require.Equal(t, AudiencePublic, guest.Audience)
	guestPayload := decodePayload(t, guest)
	require.Equal(t, media.StatusReady, guestPayload.Status)
	require.Nil(t, guestPayload.Item)
	require.NotContains(t, string(guest.Data), string(media.StatusFeatured))
}

func TestMediaMessagesIgnoresUnknownChange(t *testing.T) {
	messages, err := mediaMessages(media.Change{Kind: media.ChangeKind("exploded"), EventID: uuid.New()})
	require.NoError(t, err)
	require.Empty(t, messages)
}

func TestNotifierPublishesMediaAndEventChanges(t *testing.T) {
	bus := newFakeBus()
	hub := quietHub(bus, DefaultBuffer)
	defer hub.Close()

	eventID, mediaID := uuid.New(), uuid.New()
	sub, err := hub.Subscribe(eventID, AudiencePublic)
	require.NoError(t, err)
	waitForBus(t, bus, 1)

	notifier := NewNotifier(hub, quietLogger())
	notifier.MediaChanged(t.Context(), media.Change{
		Kind:    media.ChangeCreated,
		EventID: eventID,
		IDs:     []uuid.UUID{mediaID},
		Item:    sampleItem(mediaID, media.StatusReady),
	})

	received := receive(t, sub)
	require.Equal(t, KindMediaCreated, received.Kind)
	require.Equal(t, []uuid.UUID{mediaID}, decodePayload(t, received).IDs)
}

func TestNotifierSurvivesACancelledRequestContext(t *testing.T) {
	bus := newFakeBus()
	hub := quietHub(bus, DefaultBuffer)
	defer hub.Close()

	eventID := uuid.New()
	sub, err := hub.Subscribe(eventID, AudiencePublic)
	require.NoError(t, err)
	waitForBus(t, bus, 1)

	// A guest's HTTP request is finished the moment the upload is confirmed,
	// so the broadcast must not be cancelled along with it.
	ctx, cancel := context.WithCancel(context.Background())
	notifier := NewNotifier(hub, quietLogger())
	notifier.MediaChanged(ctx, media.Change{
		Kind:    media.ChangeDeleted,
		EventID: eventID,
		IDs:     []uuid.UUID{uuid.New()},
	})
	cancel()

	require.Equal(t, KindMediaDeleted, receive(t, sub).Kind)
}
