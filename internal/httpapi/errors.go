package httpapi

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

type Error struct {
	Status        int
	Code, Message string
}

func (e *Error) Error() string                         { return e.Message }
func NewError(status int, code, message string) *Error { return &Error{status, code, message} }
func RespondError(c *gin.Context, err error) {
	var api *Error
	if errors.As(err, &api) {
		c.JSON(api.Status, gin.H{"error": gin.H{"code": api.Code, "message": api.Message}})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"code": "internal_error", "message": "an unexpected error occurred"}})
}
