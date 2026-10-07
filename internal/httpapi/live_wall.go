package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/livewall"
	"github.com/candidcrowd/candidcrowd-backend/internal/media"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/candidcrowd/candidcrowd-backend/internal/realtime"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type LiveWallHandler struct {
	events   *event.Service
	profiles *profile.Service
	media    *media.Service
	sessions *livewall.Service
	hub      *realtime.Hub
	gate     planGate
}

// UsePlans makes the handler respect each event's plan.
func (h *LiveWallHandler) UsePlans(plans *entitlement.Service) *LiveWallHandler {
	h.gate = planGate{plans: plans}
	return h
}

func NewLiveWallHandler(events *event.Service, profiles *profile.Service, mediaService *media.Service, sessions *livewall.Service, hub *realtime.Hub) *LiveWallHandler {
	return &LiveWallHandler{events: events, profiles: profiles, media: mediaService, sessions: sessions, hub: hub}
}

func (h *LiveWallHandler) ownedEvent(c *gin.Context) (event.Event, bool) {
	return resolveOwnedEvent(c, h.events, h.profiles)
}

func (h *LiveWallHandler) Create(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	// A plan without the full Live Wall starts on Classic, the one transition
	// every plan includes.
	var transition []livewall.TransitionMode
	if !h.gate.view(c, evt.ID).allows(c, catalog.FeatureFullLiveWall) {
		transition = append(transition, livewall.TransitionModeClassic)
	}
	session, token, err := h.sessions.Create(c.Request.Context(), evt.ID, evt.Name, evt.Slug, transition...)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, gin.H{"id": session.ID, "token": token, "expires_at": session.ExpiresAt, "status": session.Status, "show_cta": session.ShowCTA, "content_policy": session.ContentPolicy, "cta_every_media": session.CTAEveryMedia, "layout_mode": session.LayoutMode, "slide_duration_seconds": session.SlideDuration, "qr_strategy": session.QRStrategy, "arrival_behavior": session.ArrivalBehavior, "transition_mode": session.TransitionMode, "revision": session.Revision})
}

func (h *LiveWallHandler) Get(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("sessionId"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "session id must be a UUID"))
		return
	}
	session, err := h.sessions.GetOwned(c.Request.Context(), evt.ID, id)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "live wall session not found"))
		return
	}
	c.JSON(http.StatusOK, session)
}

func (h *LiveWallHandler) End(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("sessionId"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "session id must be a UUID"))
		return
	}
	if err := h.sessions.End(c.Request.Context(), evt.ID, id); err != nil {
		apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "live wall session not found"))
		return
	}
	c.Status(http.StatusNoContent)
}

type liveWallUpdateRequest struct {
	ContentPolicy    *livewall.ContentPolicy   `json:"content_policy"`
	CTAEveryMedia    *int                      `json:"cta_every_media"`
	LayoutMode       *livewall.LayoutMode      `json:"layout_mode"`
	SlideDuration    *int                      `json:"slide_duration_seconds"`
	QRStrategy       *livewall.QRStrategy      `json:"qr_strategy"`
	ArrivalBehavior  *livewall.ArrivalBehavior `json:"arrival_behavior"`
	TransitionMode   *livewall.TransitionMode  `json:"transition_mode"`
	ExpectedRevision *int64                    `json:"expected_revision"`
}

func (h *LiveWallHandler) Update(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("sessionId"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "session id must be a UUID"))
		return
	}
	var request liveWallUpdateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_live_wall_settings", "invalid live wall settings"))
		return
	}
	if !h.gate.require(c, evt.ID, liveWallFeatures(request)...) {
		return
	}
	session, err := h.sessions.UpdatePresentation(c.Request.Context(), evt.ID, id, livewall.PresentationSettings{
		ContentPolicy: request.ContentPolicy, CTAEveryMedia: request.CTAEveryMedia,
		LayoutMode: request.LayoutMode, SlideDuration: request.SlideDuration,
		QRStrategy: request.QRStrategy, ArrivalBehavior: request.ArrivalBehavior, TransitionMode: request.TransitionMode,
		ExpectedRevision: request.ExpectedRevision,
	})
	if errors.Is(err, livewall.ErrInvalidPresentation) {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_live_wall_settings", "invalid live wall presentation settings"))
		return
	}
	if errors.Is(err, livewall.ErrRevisionConflict) {
		apierror.Respond(c, apierror.New(http.StatusConflict, "live_wall_revision_conflict", "live wall settings were updated elsewhere; refresh and try again"))
		return
	}
	if errors.Is(err, livewall.ErrNotFound) {
		apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "live wall session not found"))
		return
	}
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, session)
}

type liveWallCommandRequest struct {
	Command livewall.Command `json:"command" binding:"required"`
}

func (h *LiveWallHandler) Command(c *gin.Context) {
	evt, ok := h.ownedEvent(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("sessionId"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "session id must be a UUID"))
		return
	}
	var request liveWallCommandRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_command", "a live wall command is required"))
		return
	}
	session, err := h.sessions.Command(c.Request.Context(), evt.ID, id, request.Command)
	if errors.Is(err, livewall.ErrInvalidCommand) {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_command", "invalid live wall command"))
		return
	}
	if errors.Is(err, livewall.ErrNotFound) {
		apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "live wall session not found"))
		return
	}
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, session)
}

func (h *LiveWallHandler) Player(c *gin.Context) {
	token := c.Param("token")
	session, err := h.sessions.GetPublic(c.Request.Context(), token)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "live wall session not found"))
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "60"))
	// The plan shapes what the projector gets rather than refusing it: a wall
	// already on screen keeps playing, within what the event's plan includes.
	plan := h.gate.view(c, session.EventID)
	fullWall := plan.allows(c, catalog.FeatureFullLiveWall)
	maxItems := int64(0)
	if limits, ok := plan.limits(); ok && !fullWall && limits.MaxLiveWallItems > 0 {
		maxItems = limits.MaxLiveWallItems
		if limit <= 0 || int64(limit) > maxItems {
			limit = int(maxItems)
		}
	}
	transitionMode := session.TransitionMode
	if !liveWallTransitionAllowed(c, plan, transitionMode) {
		transitionMode = livewall.TransitionModeClassic
	}
	filter := media.FilterAll
	if session.ContentPolicy == livewall.ContentPolicyFeaturedOnly || session.LayoutMode == livewall.LayoutModeFeatured {
		filter = media.FilterFavorites
	}
	cursor := c.Query("cursor")
	if maxItems > 0 {
		// A capped wall is the newest maxItems media, one page, no paging.
		cursor = ""
	}
	items, err := h.media.ListGallery(c.Request.Context(), session.EventID, filter, media.SortNewest, limit, cursor, func(id uuid.UUID) string {
		return "/api/v1/public/live-wall-sessions/" + token + "/media/" + id.String() + "/content"
	})
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_query", err.Error()))
		return
	}
	// A projector loops through the same wall all evening. Signing the page
	// keeps that loop off the database entirely; the previous per-image
	// /content route was two queries per photo, every time round.
	if err := h.media.SignPage(c.Request.Context(), items.Data, mediaLinkExpiry); err != nil {
		apierror.Respond(c, err)
		return
	}
	if maxItems > 0 {
		items.NextCursor, items.HasMore = "", false
	}
	// Theme is optional presentation metadata. A missing or unavailable event
	// must not prevent an already-authorized player session from showing media.
	eventPayload := gin.H{"id": session.EventID, "name": session.EventName, "slug": session.EventSlug}
	if evt, eventErr := h.events.GetPublic(c.Request.Context(), session.EventSlug); eventErr == nil {
		eventPayload["guest_theme"] = evt.GuestTheme
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(http.StatusOK, gin.H{
		"event":   eventPayload,
		"session": gin.H{"id": session.ID, "expires_at": session.ExpiresAt, "content_policy": session.ContentPolicy},
		"presentation": gin.H{
			"is_playing":             session.IsPlaying,
			"show_cta":               session.ShowCTA,
			"is_blackout":            session.IsBlackout,
			"cta_every_media":        session.CTAEveryMedia,
			"revision":               session.Revision,
			"content_policy":         session.ContentPolicy,
			"layout_mode":            session.LayoutMode,
			"slide_duration_seconds": session.SlideDuration,
			"qr_strategy":            session.QRStrategy,
			"arrival_behavior":       session.ArrivalBehavior,
			"transition_mode":        transitionMode,
			// 0 means no cap. A capped player keeps only this many newest media,
			// including ones that arrive over the stream.
			"max_items": maxItems,
		},
		"data": items.Data,
		"page": gin.H{"next_cursor": items.NextCursor, "has_more": items.HasMore},
	})
}

func (h *LiveWallHandler) Content(c *gin.Context) {
	session, err := h.sessions.GetPublic(c.Request.Context(), c.Param("token"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "live wall session not found"))
		return
	}
	mediaID, err := uuid.Parse(c.Param("mediaId"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "media id must be a UUID"))
		return
	}
	url, err := h.media.ReadURL(c.Request.Context(), session.EventID, mediaID, 5*time.Minute)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "media not found"))
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Redirect(http.StatusTemporaryRedirect, url)
}

// Stream gives a projector an immediate notification for its presentation
// controls and for eligible new media. The opaque player token is validated
// before subscribing; it never becomes a Redis channel name or event payload.
func (h *LiveWallHandler) Stream(c *gin.Context) {
	if h.hub == nil {
		apierror.Respond(c, apierror.New(http.StatusServiceUnavailable, "stream_unavailable", "realtime stream is unavailable"))
		return
	}
	session, err := h.sessions.GetPublic(c.Request.Context(), c.Param("token"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "live wall session not found"))
		return
	}
	sub, err := h.hub.Subscribe(session.EventID, realtime.AudiencePublic)
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusServiceUnavailable, "stream_unavailable", "realtime stream is unavailable"))
		return
	}
	defer sub.Close()

	w := c.Writer
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache, no-transform")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}
	w.Flush()

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	deadline := time.NewTimer(30 * time.Minute)
	defer deadline.Stop()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-deadline.C:
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ":ping\n\n"); err != nil {
				return
			}
			w.Flush()
		case msg, open := <-sub.Messages():
			if !open {
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
		}
	}
}

// liveWallFeatures lists the plan features a presentation change needs.
// Classic and the layout and pacing controls are on every plan.
func liveWallFeatures(request liveWallUpdateRequest) []catalog.Feature {
	var features []catalog.Feature
	if request.TransitionMode != nil {
		switch *request.TransitionMode {
		case livewall.TransitionModeClassic:
		case livewall.TransitionModeLivingMosaic:
			features = append(features, catalog.FeatureThroughTheMoment)
		default:
			features = append(features, catalog.FeatureFullLiveWall)
		}
	}
	// Showing only featured media needs the ability to feature media.
	if (request.ContentPolicy != nil && *request.ContentPolicy == livewall.ContentPolicyFeaturedOnly) ||
		(request.LayoutMode != nil && *request.LayoutMode == livewall.LayoutModeFeatured) {
		features = append(features, catalog.FeatureFeatureMedia)
	}
	return features
}

func liveWallTransitionAllowed(c *gin.Context, plan planView, mode livewall.TransitionMode) bool {
	switch mode {
	case livewall.TransitionModeClassic:
		return true
	case livewall.TransitionModeLivingMosaic:
		return plan.allows(c, catalog.FeatureThroughTheMoment)
	default:
		return plan.allows(c, catalog.FeatureFullLiveWall)
	}
}
