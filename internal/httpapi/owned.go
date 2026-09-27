package httpapi

import (
	"errors"
	"net/http"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	"github.com/candidcrowd/candidcrowd-backend/internal/profile"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// resolveOwnedEvent authorizes the caller against the :id path parameter and
// answers the request itself when that fails.
//
// HostMediaHandler, InsightsHandler and ExportHandler each carry a private
// copy of this logic. They are left alone here, but new host handlers use this
// one so the rule has a single place to migrate to.
func resolveOwnedEvent(c *gin.Context, events *event.Service, profiles *profile.Service) (event.Event, bool) {
	eventID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_id", "event id must be a UUID"))
		return event.Event{}, false
	}
	identity, err := auth.Get(c)
	if err != nil {
		apierror.Respond(c, err)
		return event.Event{}, false
	}
	hostID, err := profiles.UserID(c.Request.Context(), identity)
	if err != nil {
		apierror.Respond(c, err)
		return event.Event{}, false
	}
	evt, err := events.GetOwned(c.Request.Context(), eventID, hostID)
	if err != nil {
		if errors.Is(err, event.ErrNotFound) {
			apierror.Respond(c, apierror.New(http.StatusNotFound, "not_found", "event not found"))
		} else {
			apierror.Respond(c, err)
		}
		return event.Event{}, false
	}
	return evt, true
}
