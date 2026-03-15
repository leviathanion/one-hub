package controller

import (
	"net/http"

	"one-api/providers/codex"

	"github.com/gin-gonic/gin"
)

// GetCodexAutoRefreshStatus returns Codex auto refresh runtime status.
func GetCodexAutoRefreshStatus(c *gin.Context) {
	status := codex.GetAutoRefreshStatus()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    status,
	})
}
