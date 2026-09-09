package middleware

import (
	"fmt"
	"net/http"
	"runtime/debug"

	"one-api/common/logger"

	"github.com/gin-gonic/gin"
)

// Recovery keeps Gin's normal 500 handling while allowing net/http's
// ErrAbortHandler signal to reach the server and abort a partially committed
// response instead of writing a clean end-of-body marker.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}
			logger.LogError(c.Request.Context(), fmt.Sprintf("panic recovered: %v\n%s", recovered, debug.Stack()))
			c.AbortWithStatus(http.StatusInternalServerError)
		}()
		c.Next()
	}
}
