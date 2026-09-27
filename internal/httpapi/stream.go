package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/candidcrowd/candidcrowd-backend/internal/realtime"
	"github.com/gin-gonic/gin"
)

// StreamLimiter throttles how often one client may open a stream. The guest
// endpoint is unauthenticated, so a reconnect loop must not be free.
type StreamLimiter interface {
	Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, error)
}

// StreamConfig tunes the SSE transport.
type StreamConfig struct {
	// Heartbeat keeps idle connections alive through proxies that close a
	// silent response. Cloudflare's idle timeout is 100s.
	Heartbeat time.Duration
	// MaxAge closes a connection on purpose so a live wall running all night
	// cannot accumulate server state forever. The client reconnects.
	MaxAge time.Duration
	// Retry is the reconnect delay suggested to the browser, in milliseconds.
	Retry time.Duration
	// MaxPerEvent caps connections per event on this instance.
	MaxPerEvent int
	// ConnectRate and ConnectWindow throttle new connections per client IP.
	ConnectRate   int
	ConnectWindow time.Duration
}

func (c StreamConfig) withDefaults() StreamConfig {
	if c.Heartbeat <= 0 {
		c.Heartbeat = 20 * time.Second
	}
	if c.MaxAge <= 0 {
		c.MaxAge = 30 * time.Minute
	}
	if c.Retry <= 0 {
		c.Retry = 3 * time.Second
	}
	if c.MaxPerEvent <= 0 {
		c.MaxPerEvent = 500
	}
	if c.ConnectRate <= 0 {
		c.ConnectRate = 30
	}
	if c.ConnectWindow <= 0 {
		c.ConnectWindow = time.Minute
	}
	return c
}

// StreamHandler serves the host and guest realtime streams.
//
// Every message it emits is a hint, not a source of truth. A client that
// reconnects refetches, which is why no message history is replayed and no
// Last-Event-ID is honoured.
type StreamHandler struct {
	events   *event.Service
	profiles *profile.Service
	hub      *realtime.Hub
	limiter  StreamLimiter
	cfg      StreamConfig
}

func NewStreamHandler(events *event.Service, profiles *profile.Service, hub *realtime.Hub, limiter StreamLimiter, cfg StreamConfig) *StreamHandler {
	return &StreamHandler{events: events, profiles: profiles, hub: hub, limiter: limiter, cfg: cfg.withDefaults()}
}

// Host streams changes for an event the caller owns. The route sits behind the
// authenticated group, so the browser reaches it with fetch and a bearer token
// rather than EventSource, which cannot set headers.
func (h *StreamHandler) Host(c *gin.Context) {
	evt, ok := resolveOwnedEvent(c, h.events, h.profiles)
	if !ok {
		return
	}
	h.serve(c, evt, realtime.AudienceHost)
}

// Public streams changes for a guest gallery. It is unauthenticated, matching
// the public media list, and refuses when the host has the gallery switched off.
func (h *StreamHandler) Public(c *gin.Context) {
	evt, err := h.events.GetPublic(c.Request.Context(), c.Param("slug"))
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "event not found"))
		} else {
			apierror.Respond(c, err)
		}
		return
	}
	if !evt.GalleryEnabled {
		apierror.Respond(c, apierror.New(http.StatusForbidden, "gallery_disabled", "event gallery is disabled"))
		return
	}
	h.serve(c, evt, realtime.AudiencePublic)
}

func (h *StreamHandler) serve(c *gin.Context, evt event.Event, audience realtime.Audience) {
	if !h.allowConnection(c, evt) {
		return
	}
	sub, err := h.hub.Subscribe(evt.ID, audience)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusServiceUnavailable, "stream_unavailable", "realtime stream is unavailable"))
		return
	}
	defer sub.Close()
	h.pump(c, sub, audience)
}

func (h *StreamHandler) allowConnection(c *gin.Context, evt event.Event) bool {
	if h.limiter != nil {
		allowed, err := h.limiter.Allow(c.Request.Context(), "realtime:connect:"+c.ClientIP(), h.cfg.ConnectRate, h.cfg.ConnectWindow)
		if err == nil && !allowed {
			apierror.Respond(c, apierror.New(http.StatusTooManyRequests, "rate_limited", "too many stream connections"))
			return false
		}
		// A limiter failure must not take the gallery offline; uploads and
		// moderation are already protected by their own limits.
	}
	// This cap is per instance. It bounds what one process holds rather than
	// the whole deployment, which is what actually protects this server.
	if h.hub.Subscribers(evt.ID) >= h.cfg.MaxPerEvent {
		apierror.Respond(c, apierror.New(http.StatusServiceUnavailable, "stream_capacity", "too many live viewers for this event"))
		return false
	}
	return true
}

func (h *StreamHandler) pump(c *gin.Context, sub *realtime.Subscription, audience realtime.Audience) {
	w := c.Writer
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	// no-transform asks intermediaries not to buffer or recompress the body.
	header.Set("Cache-Control", "no-cache, no-transform")
	header.Set("Connection", "keep-alive")
	// nginx buffers proxied responses by default, which would hold every
	// message until the connection closes.
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if _, err := fmt.Fprintf(w, "retry: %d\n\n", h.cfg.Retry.Milliseconds()); err != nil {
		return
	}
	w.Flush()

	heartbeat := time.NewTicker(h.cfg.Heartbeat)
	defer heartbeat.Stop()
	deadline := time.NewTimer(h.cfg.MaxAge)
	defer deadline.Stop()

	requestCtx := c.Request.Context()
	for {
		select {
		case <-requestCtx.Done():
			return

		case <-deadline.C:
			// A deliberate close. The browser reconnects after the retry
			// delay and refetches, which also refreshes expiring media URLs.
			return

		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ":ping\n\n"); err != nil {
				return
			}
			w.Flush()

		case msg, open := <-sub.Messages():
			if !open {
				// Falling behind means this client's view is stale. Say so
				// explicitly instead of letting it believe it is current.
				if sub.Dropped() {
					_, _ = fmt.Fprint(w, "event: resync\ndata: {}\n\n")
					w.Flush()
				}
				return
			}
			if err := writeMessage(w, msg); err != nil {
				return
			}
			w.Flush()
			// A gallery switched off must not keep feeding guest viewers.
			if audience == realtime.AudiencePublic && msg.Kind == realtime.KindEventUpdated && galleryDisabled(msg) {
				return
			}
		}
	}
}

func writeMessage(w gin.ResponseWriter, msg realtime.Message) error {
	// Data is compact JSON from json.Marshal, so it never contains a raw
	// newline that would split the SSE frame.
	_, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", msg.ID, msg.Kind, msg.Data)
	return err
}

func galleryDisabled(msg realtime.Message) bool {
	var payload struct {
		GalleryEnabled *bool `json:"gallery_enabled"`
	}
	if err := json.Unmarshal(msg.Data, &payload); err != nil {
		return false
	}
	return payload.GalleryEnabled != nil && !*payload.GalleryEnabled
}
