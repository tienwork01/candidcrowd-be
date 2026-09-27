// Package realtime carries event-scoped change notifications from the API
// process that observed a change to every browser watching that event.
//
// The package is split in two halves. A Bus moves an encoded message between
// API instances; a Hub delivers messages to the connections held by one
// instance. Browser transport (SSE) stays in the HTTP layer, so replacing it
// later does not reach into this package.
//
// Delivery is a hint, never a source of truth. A client that misses messages
// while reconnecting recovers by refetching, which is why no message log,
// replay, or ordering guarantee exists here.
package realtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Kind names a change that a browser can react to. Values are part of the
// public API contract: they are written straight into the SSE event field.
type Kind string

const (
	KindMediaCreated         Kind = "media.created"
	KindMediaThumbnail       Kind = "media.thumbnail.ready"
	KindMediaUpdated         Kind = "media.updated"
	KindMediaDeleted         Kind = "media.deleted"
	KindEventUpdated         Kind = "event.updated"
	KindLiveWallPresentation Kind = "live_wall.presentation"
)

func (k Kind) Valid() bool {
	switch k {
	case KindMediaCreated, KindMediaThumbnail, KindMediaUpdated, KindMediaDeleted, KindEventUpdated, KindLiveWallPresentation:
		return true
	}
	return false
}

// Audience is a bitmask of who may receive a message. A host stream and a
// guest stream watch the same event but must not see the same payload: hidden
// media stays host-only, and public payloads never carry storage keys.
//
// A producer that needs a different payload per audience publishes one message
// per audience rather than one message carrying both.
type Audience uint8

const (
	AudienceHost Audience = 1 << iota
	AudiencePublic
)

// Includes reports whether a message addressed to this mask reaches a
// subscriber holding the other role.
func (a Audience) Includes(other Audience) bool { return a&other != 0 }

func (a Audience) Valid() bool {
	return a != 0 && a&^(AudienceHost|AudiencePublic) == 0
}

// Message is the envelope that crosses the bus. Data stays raw so the hub
// never re-encodes a payload it does not need to inspect.
type Message struct {
	ID       string          `json:"id"`
	Kind     Kind            `json:"kind"`
	EventID  uuid.UUID       `json:"event_id"`
	Audience Audience        `json:"audience"`
	At       time.Time       `json:"at"`
	Data     json.RawMessage `json:"data"`
}

var errInvalidMessage = errors.New("realtime: invalid message")

// NewMessage builds an envelope around a payload. The generated ID becomes the
// SSE id field, which is how a browser reports its position on reconnect.
func NewMessage(eventID uuid.UUID, kind Kind, audience Audience, payload any) (Message, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return Message{}, fmt.Errorf("realtime: encode payload: %w", err)
	}
	msg := Message{
		ID:       uuid.NewString(),
		Kind:     kind,
		EventID:  eventID,
		Audience: audience,
		At:       time.Now().UTC(),
		Data:     data,
	}
	if err := msg.Validate(); err != nil {
		return Message{}, err
	}
	return msg, nil
}

func (m Message) Validate() error {
	if m.EventID == uuid.Nil {
		return fmt.Errorf("%w: event id is required", errInvalidMessage)
	}
	if !m.Kind.Valid() {
		return fmt.Errorf("%w: unknown kind %q", errInvalidMessage, m.Kind)
	}
	if !m.Audience.Valid() {
		return fmt.Errorf("%w: audience %d", errInvalidMessage, m.Audience)
	}
	if len(m.Data) == 0 {
		return fmt.Errorf("%w: data is required", errInvalidMessage)
	}
	return nil
}

func Encode(m Message) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

func Decode(raw []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		return Message{}, fmt.Errorf("realtime: decode message: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Message{}, err
	}
	return m, nil
}
