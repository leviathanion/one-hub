package middleware

import (
	"fmt"
	"net/http"
	"one-api/common"
	"one-api/model"
	"one-api/types"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	LIMIT_KEY               = "api-limiter:%d"
	INTERNAL                = 1 * time.Minute
	RATE_LIMIT_EXCEEDED_MSG = "您的速率达到上限，请稍后再试。"
	SERVER_ERROR_MSG        = "Server error"
)

func DynamicRedisRateLimiter() gin.HandlerFunc {
	return func(c *gin.Context) {
		if apiErr := AllowCurrentUserRequest(c); apiErr != nil {
			abortWithMessage(c, apiErr.StatusCode, apiErr.Message)
			return
		}

		c.Next()
	}
}

func EnsureCurrentUserRequestAllowed(c *gin.Context) *types.OpenAIErrorWithStatusCode {
	if c == nil {
		return common.StringErrorWrapperLocal("request context is required", "invalid_request", http.StatusBadRequest)
	}
	userGroup := c.GetString("group")
	limiter := model.GlobalUserGroupRatio.GetAPILimiter(userGroup)
	if limiter == nil {
		return common.StringErrorWrapperLocal("API requests are not allowed", "api_requests_not_allowed", http.StatusForbidden)
	}
	return nil
}

func AllowCurrentUserRequest(c *gin.Context) *types.OpenAIErrorWithStatusCode {
	if err := EnsureCurrentUserRequestAllowed(c); err != nil {
		return err
	}

	userID := c.GetInt("id")
	if userID <= 0 {
		return common.StringErrorWrapperLocal("authenticated user id is required", "invalid_user", http.StatusUnauthorized)
	}
	limiter := model.GlobalUserGroupRatio.GetAPILimiter(c.GetString("group"))
	if !limiter.Allow(fmt.Sprintf(LIMIT_KEY, userID)) {
		return common.StringErrorWrapperLocal(RATE_LIMIT_EXCEEDED_MSG, "rate_limit_exceeded", http.StatusTooManyRequests)
	}
	return nil
}
