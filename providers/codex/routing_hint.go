package codex

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"one-api/common/config"
	"one-api/internal/requesthints"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

type RoutingHintSettings struct {
	PromptCacheKeyStrategy string `json:"prompt_cache_key_strategy,omitempty"`
	ModelRegex             string `json:"model_regex,omitempty"`
	UserAgentRegex         string `json:"user_agent_regex,omitempty"`
}

var (
	RoutingHintSettingsInstance = DefaultRoutingHintSettings()

	codexRoutingHintRegexCache sync.Map
)

func init() {
	config.GlobalOption.RegisterCustom("CodexRoutingHintSetting", func() string {
		return RoutingHintSettingsInstance.JSONString()
	}, func(value string) error {
		return RoutingHintSettingsInstance.SetFromJSON(value)
	}, RoutingHintSettingsInstance.JSONString())

	requesthints.RegisterResponsesResolver(codexResponsesHintResolver{})
}

func DefaultRoutingHintSettings() RoutingHintSettings {
	settings := RoutingHintSettings{
		PromptCacheKeyStrategy: codexPromptCacheStrategyOff,
	}
	settings.Normalize()
	return settings
}

func (s *RoutingHintSettings) SetFromJSON(data string) error {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		*s = DefaultRoutingHintSettings()
		return nil
	}

	settings := DefaultRoutingHintSettings()
	if err := json.Unmarshal([]byte(trimmed), &settings); err != nil {
		return err
	}
	settings.Normalize()
	*s = settings
	return nil
}

func (s RoutingHintSettings) JSONString() string {
	normalized := s
	normalized.Normalize()

	data, err := json.Marshal(normalized)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func (s *RoutingHintSettings) Normalize() {
	s.PromptCacheKeyStrategy = normalizePromptCacheStrategy(s.PromptCacheKeyStrategy)
	s.ModelRegex = strings.TrimSpace(s.ModelRegex)
	s.UserAgentRegex = strings.TrimSpace(s.UserAgentRegex)
}

type codexResponsesHintResolver struct{}

func (codexResponsesHintResolver) Name() string {
	return "codex-responses"
}

func (codexResponsesHintResolver) ResolveResponsesHints(ctx *gin.Context, request *types.OpenAIResponsesRequest) map[string]string {
	if request == nil || strings.TrimSpace(request.PromptCacheKey) != "" {
		return nil
	}

	settings := RoutingHintSettingsInstance
	settings.Normalize()
	if settings.PromptCacheKeyStrategy == codexPromptCacheStrategyOff {
		return nil
	}
	if !codexRoutingHintMatches(ctx, request, settings) {
		return nil
	}

	key := promptCacheKeyForStrategy(ctx, settings.PromptCacheKeyStrategy)
	if key == "" {
		return nil
	}

	return map[string]string{
		requesthints.ResponsesPromptCacheKey: key,
	}
}

func codexRoutingHintMatches(ctx *gin.Context, request *types.OpenAIResponsesRequest, settings RoutingHintSettings) bool {
	modelName := ""
	if request != nil {
		modelName = strings.TrimSpace(request.Model)
	}
	if settings.ModelRegex != "" && !codexRoutingHintRegexMatch(settings.ModelRegex, modelName) {
		return false
	}

	userAgent := ""
	if ctx != nil && ctx.Request != nil {
		userAgent = ctx.Request.UserAgent()
	}
	if settings.UserAgentRegex != "" && !codexRoutingHintRegexMatch(settings.UserAgentRegex, userAgent) {
		return false
	}

	return true
}

func codexRoutingHintRegexMatch(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return true
	}

	compiled, ok := codexRoutingHintRegexCache.Load(pattern)
	if !ok {
		reg, err := regexp.Compile(pattern)
		if err != nil {
			return false
		}
		compiled, _ = codexRoutingHintRegexCache.LoadOrStore(pattern, reg)
	}

	reg, ok := compiled.(*regexp.Regexp)
	if !ok {
		return false
	}
	return reg.MatchString(strings.TrimSpace(value))
}
