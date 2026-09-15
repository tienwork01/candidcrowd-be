package profile

import (
	"net/http"

	"github.com/candidcrowd/candidcrowd-backend/internal/apierror"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/auth"
	"github.com/gin-gonic/gin"
)

type Handler struct{ service *Service }

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) Me(c *gin.Context) {
	identity, err := auth.Get(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	view, err := h.service.Me(c.Request.Context(), identity)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}

type consentRequest struct {
	TermsVersion   string `json:"terms_version" binding:"required,max=40"`
	PrivacyVersion string `json:"privacy_version" binding:"required,max=40"`
}

func (h *Handler) Accept(c *gin.Context) {
	var request consentRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		apierror.Respond(c, apierror.New(http.StatusBadRequest, "invalid_request", err.Error()))
		return
	}
	identity, err := auth.Get(c)
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	view, err := h.service.Accept(c.Request.Context(), identity, request.TermsVersion, request.PrivacyVersion)
	if err == ErrInvalidConsent {
		apierror.Respond(c, apierror.New(http.StatusUnprocessableEntity, "invalid_consent_version", "consent document version is invalid"))
		return
	}
	if err != nil {
		apierror.Respond(c, err)
		return
	}
	c.JSON(http.StatusOK, view)
}
