package middleware

import (
	"net/http"
	"strconv"

	"one-api/common"
	"one-api/common/config"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

var realtimeWSConnectionLimiter = websocketConnectionLimiterState{name: "RealtimeWS"}

func AllowRealtimeConnectionAttempt(c *gin.Context) *types.OpenAIErrorWithStatusCode {
	if c == nil || c.GetInt("id") <= 0 {
		return common.StringErrorWrapperLocal("authenticated user id is required", "invalid_user", http.StatusUnauthorized)
	}
	limit, err := config.RealtimeWSConnectPerUserPerMinute()
	if err != nil {
		return common.StringErrorWrapperLocal(err.Error(), "realtime_ws_configuration_error", http.StatusInternalServerError)
	}
	key := "realtime-ws-connect:user:" + strconv.Itoa(c.GetInt("id"))
	if !realtimeWSConnectionLimiter.allow(limit, key) {
		return common.StringErrorWrapperLocal("too many realtime websocket connection attempts", "realtime_ws_connection_rate_limited", http.StatusTooManyRequests)
	}
	return nil
}
