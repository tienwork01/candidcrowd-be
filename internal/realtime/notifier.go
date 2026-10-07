package realtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/livewall"
	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/google/uuid"
)

// publishTimeout bounds a broadcast that outlives the request that caused it.
const publishTimeout = 5 * time.Second

// Notifier adapts feature-level changes to bus messages. The feature packages
// declare only their own narrow interfaces, so nothing outside this file knows
// both a domain change and the realtime envelope.
type Notifier struct {
	hub *Hub
	log *slog.Logger
}

func NewNotifier(hub *Hub, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{hub: hub, log: log}
}

var (
	_ media.Notifier    = (*Notifier)(nil)
	_ event.Notifier    = (*Notifier)(nil)
	_ livewall.Notifier = (*Notifier)(nil)
)

func (n *Notifier) MediaChanged(ctx context.Context, change media.Change) {
	messages, err := mediaMessages(change)
	if err != nil {
		n.log.Error("realtime: could not build media messages", "event_id", change.EventID, "kind", change.Kind, "error", err)
		return
	}
	n.dispatch(ctx, messages)
}

func (n *Notifier) EventChanged(ctx context.Context, change event.Change) {
	msg, err := NewMessage(change.EventID, KindEventUpdated, AudienceHost|AudiencePublic, change)
	if err != nil {
		n.log.Error("realtime: could not build event message", "event_id", change.EventID, "error", err)
		return
	}
	n.dispatch(ctx, []Message{msg})
}

func (n *Notifier) LiveWallChanged(ctx context.Context, change livewall.Change) {
	session := change.Session
	msg, err := NewMessage(session.EventID, KindLiveWallPresentation, AudiencePublic, map[string]any{
		"session_id":             session.ID,
		"status":                 session.Status,
		"is_playing":             session.IsPlaying,
		"show_cta":               session.ShowCTA,
		"is_blackout":            session.IsBlackout,
		"cta_every_media":        session.CTAEveryMedia,
		"revision":               session.Revision,
		"command":                change.Command,
		"content_policy":         session.ContentPolicy,
		"layout_mode":            session.LayoutMode,
		"slide_duration_seconds": session.SlideDuration,
		"qr_strategy":            session.QRStrategy,
		"arrival_behavior":       session.ArrivalBehavior,
		"transition_mode":        session.TransitionMode,
	})
	if err != nil {
		n.log.Error("realtime: could not build live wall message", "event_id", session.EventID, "error", err)
		return
	}
	n.dispatch(ctx, []Message{msg})
}

// dispatch publishes out of band. A guest's upload confirmation and a host's
// moderation click must not wait on Redis, and neither should fail when the
// broadcast does.
func (n *Notifier) dispatch(ctx context.Context, messages []Message) {
	if len(messages) == 0 {
		return
	}
	// The request context is about to be cancelled; keep its values (request
	// id, tracing) but not its deadline.
	base := context.WithoutCancel(ctx)
	go func() {
		publishCtx, cancel := context.WithTimeout(base, publishTimeout)
		defer cancel()
		for _, msg := range messages {
			if err := n.hub.Publish(publishCtx, msg); err != nil {
				n.log.Error("realtime: publish failed", "event_id", msg.EventID, "kind", msg.Kind, "error", err)
			}
		}
	}()
}

// mediaPayload is what a browser receives for a media change. It is the same
// shape for both audiences and never carries a URL: a host URL has to be
// presigned per request, and a guest URL is a stable route the client builds
// from the event slug it already has.
type mediaPayload struct {
	IDs            []uuid.UUID  `json:"ids"`
	Status         media.Status `json:"status,omitempty"`
	Item           *media.Item  `json:"item,omitempty"`
	ThumbnailReady bool         `json:"thumbnail_ready,omitempty"`
}

// mediaMessages encodes the audience policy and is deliberately pure so the
// rules can be tested without a bus.
//
// Two rules matter for privacy. Hidden media must never reach a guest, so a
// hide is published to guests as a deletion. And host curation is not a guest
// concern, so a featured item is reported to guests as plain ready.
func mediaMessages(change media.Change) ([]Message, error) {
	switch change.Kind {
	case media.ChangeCreated:
		// A newly ready upload is public-safe, so one message serves both.
		return buildOne(change.EventID, KindMediaCreated, AudienceHost|AudiencePublic, mediaPayload{
			IDs:    change.IDs,
			Status: media.StatusReady,
			Item:   change.Item,
		})

	case media.ChangeThumbnail:
		return buildOne(change.EventID, KindMediaThumbnail, AudienceHost|AudiencePublic, mediaPayload{
			IDs:            change.IDs,
			ThumbnailReady: true,
		})

	case media.ChangeDeleted:
		return buildOne(change.EventID, KindMediaDeleted, AudienceHost|AudiencePublic, mediaPayload{
			IDs: change.IDs,
		})

	case media.ChangeModerated:
		host, err := NewMessage(change.EventID, KindMediaUpdated, AudienceHost, mediaPayload{
			IDs:    change.IDs,
			Status: change.Status,
			Item:   change.Item,
		})
		if err != nil {
			return nil, err
		}
		if change.Status == media.StatusHidden {
			// Guests are told the item is gone. They are never told it exists
			// but is hidden, and they receive no metadata about it.
			guest, guestErr := NewMessage(change.EventID, KindMediaDeleted, AudiencePublic, mediaPayload{IDs: change.IDs})
			if guestErr != nil {
				return nil, guestErr
			}
			return []Message{host, guest}, nil
		}
		// Unhiding restores an item a guest gallery may never have loaded, so
		// the guest payload carries no item and the client resolves it by
		// refetching. Status is normalised: "featured" is host curation.
		guest, err := NewMessage(change.EventID, KindMediaUpdated, AudiencePublic, mediaPayload{
			IDs:    change.IDs,
			Status: media.StatusReady,
		})
		if err != nil {
			return nil, err
		}
		return []Message{host, guest}, nil
	}
	return nil, nil
}

func buildOne(eventID uuid.UUID, kind Kind, audience Audience, payload mediaPayload) ([]Message, error) {
	msg, err := NewMessage(eventID, kind, audience, payload)
	if err != nil {
		return nil, err
	}
	return []Message{msg}, nil
}

// PlanChanged tells the event's host streams that its plan changed. Guests do
// not see plans, so the message is host-only.
func (n *Notifier) PlanChanged(ctx context.Context, change entitlement.PlanChange) {
	msg, err := planMessage(change)
	if err != nil {
		n.log.Error("realtime: could not build plan message", "event_id", change.EventID, "error", err)
		return
	}
	n.dispatch(ctx, []Message{msg})
}

func planMessage(change entitlement.PlanChange) (Message, error) {
	return NewMessage(change.EventID, KindEventPlanUpdated, AudienceHost, change)
}

// SyncPlanNotifier announces plan changes before returning. Short-lived tools
// such as planctl use it: they exit right after the grant, which would drop a
// message still waiting in Notifier's background send.
type SyncPlanNotifier struct {
	Hub *Hub
	Log *slog.Logger
}

func (n SyncPlanNotifier) PlanChanged(ctx context.Context, change entitlement.PlanChange) {
	msg, err := planMessage(change)
	if err == nil {
		publishCtx, cancel := context.WithTimeout(ctx, publishTimeout)
		defer cancel()
		err = n.Hub.Publish(publishCtx, msg)
	}
	if err != nil && n.Log != nil {
		n.Log.Error("realtime: plan change not announced", "event_id", change.EventID, "error", err)
	}
}
