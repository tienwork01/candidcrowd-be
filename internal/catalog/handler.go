package catalog

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/gin-gonic/gin"
)

// Rollout tells the frontend which plan surfaces it may show. It is served
// with the catalog so the two repositories cannot disagree about it.
type Rollout struct {
	PricingUIEnabled        bool `json:"pricing_ui_enabled"`
	ManualActivationEnabled bool `json:"manual_activation_enabled"`
	// EnforcementEnabled tells the host UI to show locked states. While it is
	// off the backend only logs denials, so the UI must not lock anything.
	EnforcementEnabled bool `json:"enforcement_enabled"`
	CheckoutEnabled    bool `json:"checkout_enabled"`
}

type Handler struct {
	service *Service
	rollout Rollout
}

func NewHandler(service *Service, rollout Rollout) *Handler {
	return &Handler{service: service, rollout: rollout}
}

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

func (h *Handler) List(c *gin.Context) {
	currency := strings.ToUpper(strings.TrimSpace(c.DefaultQuery("currency", "USD")))
	if !currencyPattern.MatchString(currency) {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_query", "currency must be an ISO 4217 code"))
		return
	}
	plans, err := h.service.List(c.Request.Context(), currency)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	c.Header("Cache-Control", "public, max-age=300")
	c.JSON(http.StatusOK, gin.H{"plans": plans, "rollout": h.rollout})
}
