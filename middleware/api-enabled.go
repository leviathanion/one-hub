package middleware

import (
	"net/http"
	"one-api/common/config"

	"github.com/gin-gonic/gin"
)

func APIEnabled(types string) gin.HandlerFunc {
	return func(c *gin.Context) {
		options := config.GlobalOption.RuntimeSnapshot()
		switch types {
		case "gemini":
			if !options.Bool("GeminiAPIEnabled", config.GeminiAPIEnabled) {
				c.JSON(http.StatusForbidden, gin.H{
					"error": gin.H{
						"code":    500,
						"status":  "one_hub_error",
						"message": "Gemini API is not supported",
					},
				})
				c.Abort()
				return
			}
		case "claude":
			if !options.Bool("ClaudeAPIEnabled", config.ClaudeAPIEnabled) {
				c.JSON(http.StatusForbidden, gin.H{
					"type": "one_hub_error",
					"error": gin.H{
						"type":    "api_not_supported",
						"message": "Claude API is not supported",
					},
				})
				c.Abort()
				return
			}
		}

		c.Next()
	}
}
