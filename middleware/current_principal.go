package middleware

import (
	"context"
	"errors"
	"net/http"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/utils"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const principalReadTimeout = 3 * time.Second

// ValidateCurrentPrincipal 无条件核验当前 SQL 凭据，不要求余额或新工作选组权限。
// 免费资源仍以用户为 owner；当前有效 token 无需是创建资源时使用的 token。
func ValidateCurrentPrincipal(c *gin.Context) *types.OpenAIErrorWithStatusCode {
	readCtx, cancel := principalReadContext(c)
	defer cancel()
	_, _, apiErr := loadCurrentPrincipal(c, readCtx)
	return apiErr
}

func loadCurrentPrincipal(c *gin.Context, readCtx context.Context) (*model.Token, *model.UserRoutingState, *types.OpenAIErrorWithStatusCode) {
	if c == nil {
		return nil, nil, common.StringErrorWrapperLocal("request context is required", "invalid_request", http.StatusBadRequest)
	}
	userID, tokenID := c.GetInt("id"), c.GetInt("token_id")
	if userID <= 0 || tokenID <= 0 {
		return nil, nil, common.StringErrorWrapperLocal("authenticated principal is required", "invalid_api_key", http.StatusUnauthorized)
	}
	if model.DB == nil {
		return nil, nil, common.StringErrorWrapperLocal("principal store is unavailable", "principal_read_failed", http.StatusServiceUnavailable)
	}
	token, err := model.GetTokenByIdWithContext(readCtx, tokenID)
	if err != nil {
		return nil, nil, principalReadError(err)
	}
	if token.UserId != userID {
		return nil, nil, common.StringErrorWrapperLocal("token owner changed", "invalid_api_key", http.StatusUnauthorized)
	}
	if token.Status != config.TokenStatusEnabled {
		return nil, nil, common.StringErrorWrapperLocal("token is disabled", "invalid_api_key", http.StatusUnauthorized)
	}
	if token.ExpiredTime != -1 && token.ExpiredTime <= utils.GetTimestamp() {
		return nil, nil, common.StringErrorWrapperLocal("token is expired", "invalid_api_key", http.StatusUnauthorized)
	}
	user, err := model.GetUserRoutingState(readCtx, userID)
	if err != nil {
		return nil, nil, principalReadError(err)
	}
	if user.Status != config.UserStatusEnabled {
		return nil, nil, common.StringErrorWrapperLocal("user is disabled", "invalid_api_key", http.StatusUnauthorized)
	}
	c.Set("token_setting", utils.GetPointer(token.Setting.Data()))
	if err := checkLimitIP(c); err != nil {
		return nil, nil, common.ErrorWrapperLocal(err, "permission_denied", http.StatusForbidden)
	}
	return token, user, nil
}

func principalReadContext(c *gin.Context) (context.Context, context.CancelFunc) {
	readCtx := context.Background()
	if c != nil && c.Request != nil {
		readCtx = c.Request.Context()
	}
	// 长连接是否脱离原 HTTP 请求，由各自的会话快照负责。
	return context.WithTimeout(readCtx, principalReadTimeout)
}

func principalReadError(err error) *types.OpenAIErrorWithStatusCode {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return common.StringErrorWrapperLocal("authenticated principal no longer exists", "invalid_api_key", http.StatusUnauthorized)
	}
	return common.ErrorWrapperLocal(err, "principal_read_failed", http.StatusServiceUnavailable)
}
