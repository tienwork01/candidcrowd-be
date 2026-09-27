package realtime

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestNewMessageRoundTrip(t *testing.T) {
	eventID := uuid.New()
	msg, err := NewMessage(eventID, KindMediaCreated, AudienceHost|AudiencePublic, map[string]string{"id": "abc"})
	require.NoError(t, err)
	require.NotEmpty(t, msg.ID)
	require.Equal(t, eventID, msg.EventID)
	require.False(t, msg.At.IsZero())

	raw, err := Encode(msg)
	require.NoError(t, err)

	decoded, err := Decode(raw)
	require.NoError(t, err)
	require.Equal(t, msg.ID, decoded.ID)
	require.Equal(t, msg.Kind, decoded.Kind)
	require.Equal(t, msg.EventID, decoded.EventID)
	require.Equal(t, msg.Audience, decoded.Audience)
	require.JSONEq(t, string(msg.Data), string(decoded.Data))
}

func TestMessageValidation(t *testing.T) {
	valid := json.RawMessage(`{"ok":true}`)

	tests := map[string]Message{
		"missing event id": {ID: "1", Kind: KindMediaCreated, Audience: AudienceHost, Data: valid},
		"unknown kind":     {ID: "1", Kind: Kind("media.exploded"), EventID: uuid.New(), Audience: AudienceHost, Data: valid},
		"empty audience":   {ID: "1", Kind: KindMediaCreated, EventID: uuid.New(), Audience: 0, Data: valid},
		"unknown audience": {ID: "1", Kind: KindMediaCreated, EventID: uuid.New(), Audience: 1 << 4, Data: valid},
		"missing data":     {ID: "1", Kind: KindMediaCreated, EventID: uuid.New(), Audience: AudienceHost},
	}

	for name, msg := range tests {
		t.Run(name, func(t *testing.T) {
			require.Error(t, msg.Validate())
			_, err := Encode(msg)
			require.Error(t, err)
		})
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	_, err := Decode([]byte("not json"))
	require.Error(t, err)

	// Well-formed JSON that is not a valid message must also be refused, so a
	// stray publisher cannot push an unroutable envelope to every viewer.
	_, err = Decode([]byte(`{"kind":"media.created"}`))
	require.Error(t, err)
}

func TestAudienceIncludes(t *testing.T) {
	both := AudienceHost | AudiencePublic

	require.True(t, both.Includes(AudienceHost))
	require.True(t, both.Includes(AudiencePublic))
	require.True(t, AudienceHost.Includes(AudienceHost))
	require.False(t, AudienceHost.Includes(AudiencePublic))
	require.False(t, AudiencePublic.Includes(AudienceHost))
}
