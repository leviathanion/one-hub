package codex

import (
	"strconv"
	"strings"

	"one-api/common/authutil"
	"one-api/internal/requesthints"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func normalizePromptCacheStrategy(strategy string) string {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "":
		return codexPromptCacheStrategyOff
	case codexPromptCacheStrategyAuto:
		return codexPromptCacheStrategyAuto
	case codexPromptCacheStrategyOff:
		return codexPromptCacheStrategyOff
	case codexPromptCacheStrategySessionID:
		return codexPromptCacheStrategySessionID
	case codexPromptCacheStrategyTokenID:
		return codexPromptCacheStrategyTokenID
	case codexPromptCacheStrategyUserID:
		return codexPromptCacheStrategyUserID
	case codexPromptCacheStrategyAuthHeader:
		return codexPromptCacheStrategyAuthHeader
	default:
		return codexPromptCacheStrategyOff
	}
}

func promptCacheKeyForStrategy(ctx *gin.Context, strategy string) string {
	identity := codexPromptCacheIdentity(ctx, strategy)
	if identity == "" {
		return ""
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity)).String()
}

func ensureStablePromptCacheKey(request *types.OpenAIResponsesRequest, ctx *gin.Context, strategy string) {
	if request == nil || strings.TrimSpace(request.PromptCacheKey) != "" {
		return
	}

	if resolvedHint := requesthints.Get(ctx, requesthints.ResponsesPromptCacheKey); resolvedHint != "" {
		request.PromptCacheKey = resolvedHint
		return
	}

	request.PromptCacheKey = promptCacheKeyForStrategy(ctx, strategy)
}

func codexPromptCacheIdentity(ctx *gin.Context, strategy string) string {
	if ctx == nil {
		return ""
	}

	switch normalizePromptCacheStrategy(strategy) {
	case codexPromptCacheStrategyOff:
		return ""
	case codexPromptCacheStrategySessionID:
		return codexPromptCacheIdentityFromSessionID(ctx)
	case codexPromptCacheStrategyTokenID:
		return codexPromptCacheIdentityFromTokenID(ctx)
	case codexPromptCacheStrategyUserID:
		return codexPromptCacheIdentityFromUserID(ctx)
	case codexPromptCacheStrategyAuthHeader:
		return codexPromptCacheIdentityFromAuthHeader(ctx)
	default:
		if identity := codexPromptCacheIdentityFromSessionID(ctx); identity != "" {
			return identity
		}
		if identity := codexPromptCacheIdentityFromAuthHeader(ctx); identity != "" {
			return identity
		}
		if identity := codexPromptCacheIdentityFromTokenID(ctx); identity != "" {
			return identity
		}
		return codexPromptCacheIdentityFromUserID(ctx)
	}
}

func codexPromptCacheIdentityFromSessionID(ctx *gin.Context) string {
	if ctx == nil || ctx.Request == nil {
		return ""
	}

	sessionID := strings.TrimSpace(runtimesession.ReadClientSessionID(ctx.Request))
	if sessionID == "" {
		return ""
	}
	return "one-hub:codex:prompt-cache:session:" + sessionID
}

func codexContextInt(ctx *gin.Context, key string) (int, bool) {
	if ctx == nil {
		return 0, false
	}

	value, exists := ctx.Get(key)
	if !exists || value == nil {
		return 0, false
	}

	switch typed := value.(type) {
	case int:
		return typed, true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func codexPromptCacheIdentityFromTokenID(ctx *gin.Context) string {
	if tokenID, ok := codexContextInt(ctx, "token_id"); ok && tokenID > 0 {
		return "one-hub:codex:prompt-cache:token:" + strconv.Itoa(tokenID)
	}
	return ""
}

func codexPromptCacheIdentityFromUserID(ctx *gin.Context) string {
	if userID, ok := codexContextInt(ctx, "id"); ok && userID > 0 {
		return "one-hub:codex:prompt-cache:user:" + strconv.Itoa(userID)
	}
	return ""
}

func codexPromptCacheIdentityFromAuthHeader(ctx *gin.Context) string {
	if ctx == nil || ctx.Request == nil {
		return ""
	}

	credential := authutil.ExtractStableRequestCredential(ctx.Request)
	if credential.Empty() {
		return ""
	}
	return "one-hub:codex:prompt-cache:auth:" + credential.Value
}
