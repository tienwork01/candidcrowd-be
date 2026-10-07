package apierror

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

type Error struct {
	Status        int
	Code, Message string
	// Details are extra machine-readable fields merged into the error object,
	// e.g. the plan a feature requires or the limit an upload hit.
	Details map[string]any
}

func (e *Error) Error() string { return e.Message }
func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

// WithDetails returns a copy of e carrying details.
func (e *Error) WithDetails(details map[string]any) *Error {
	copied := *e
	copied.Details = details
	return &copied
}

// Coder lets a domain error describe its own API representation, so every
// handler that returns it answers the same way without importing its package.
type Coder interface {
	APIError() *Error
}

func Respond(c *gin.Context, err error) {
	var api *Error
	if !errors.As(err, &api) {
		var coder Coder
		if errors.As(err, &coder) {
			api = coder.APIError()
		}
	}
	if api != nil {
		body := gin.H{"code": api.Code, "message": api.Message}
		for k, v := range api.Details {
			if k != "code" && k != "message" {
				body[k] = v
			}
		}
		c.JSON(api.Status, gin.H{"error": body})
		return
	}
	slog.Error("unhandled API error",
		"error", err,
		"method", c.Request.Method,
		"path", c.FullPath(),
		"request_id", c.GetString("request_id"),
	)
	c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "internal_error", "message": "an unexpected error occurred"}})
}
