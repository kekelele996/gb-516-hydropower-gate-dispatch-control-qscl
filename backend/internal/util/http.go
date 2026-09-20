package util

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

type Envelope struct {
	Data      any    `json:"data,omitempty"`
	Error     string `json:"error,omitempty"`
	Message   string `json:"message,omitempty"`
	Meta      any    `json:"meta,omitempty"`
	Conflicts any    `json:"conflicts,omitempty"`
}

func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Envelope{Data: data})
}

func Created(c *gin.Context, data any) {
	c.JSON(http.StatusCreated, Envelope{Data: data})
}

func NoContent(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

func Fail(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, Envelope{Error: code, Message: message})
}

// FailWithConflicts rejects a request and carries the conflicting gate list so
// the workbench can show exactly which gates blocked the whole dispatch.
func FailWithConflicts(c *gin.Context, status int, code, message string, conflicts any) {
	c.AbortWithStatusJSON(status, Envelope{Error: code, Message: message, Conflicts: conflicts})
}

func Page(c *gin.Context, data any, page, pageSize int, total int64) {
	c.JSON(http.StatusOK, Envelope{
		Data: data,
		Meta: gin.H{"page": page, "pageSize": pageSize, "total": total},
	})
}
