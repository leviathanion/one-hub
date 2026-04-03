package requesthints

import (
	"strings"
	"sync"

	"one-api/types"

	"github.com/gin-gonic/gin"
)

const (
	ginRequestHintsContextKey = "request_hints"

	ResponsesPromptCacheKey = "responses.prompt_cache_key"
)

type ResponsesResolver interface {
	Name() string
	ResolveResponsesHints(ctx *gin.Context, request *types.OpenAIResponsesRequest) map[string]string
}

var (
	responsesResolversMu sync.RWMutex
	responsesResolvers   []ResponsesResolver
)

func RegisterResponsesResolver(resolver ResponsesResolver) {
	if resolver == nil || strings.TrimSpace(resolver.Name()) == "" {
		return
	}

	responsesResolversMu.Lock()
	defer responsesResolversMu.Unlock()

	for index, existing := range responsesResolvers {
		if existing == nil {
			continue
		}
		if existing.Name() != resolver.Name() {
			continue
		}
		responsesResolvers[index] = resolver
		return
	}

	responsesResolvers = append(responsesResolvers, resolver)
}

func ResolveResponses(c *gin.Context, request *types.OpenAIResponsesRequest) map[string]string {
	responsesResolversMu.RLock()
	resolvers := append([]ResponsesResolver(nil), responsesResolvers...)
	responsesResolversMu.RUnlock()

	hints := make(map[string]string)
	for _, resolver := range resolvers {
		if resolver == nil {
			continue
		}
		for key, value := range resolver.ResolveResponsesHints(c, request) {
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if key == "" || value == "" {
				continue
			}
			hints[key] = value
		}
	}

	Set(c, hints)
	return hints
}

func Set(c *gin.Context, hints map[string]string) {
	if c == nil {
		return
	}

	normalized := make(map[string]string)
	for key, value := range hints {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		normalized[key] = value
	}
	c.Set(ginRequestHintsContextKey, normalized)
}

func Get(c *gin.Context, key string) string {
	key = strings.TrimSpace(key)
	if c == nil || key == "" {
		return ""
	}

	value, exists := c.Get(ginRequestHintsContextKey)
	if !exists || value == nil {
		return ""
	}
	hints, ok := value.(map[string]string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(hints[key])
}

func Snapshot(c *gin.Context) map[string]string {
	if c == nil {
		return nil
	}

	value, exists := c.Get(ginRequestHintsContextKey)
	if !exists || value == nil {
		return nil
	}
	hints, ok := value.(map[string]string)
	if !ok || len(hints) == 0 {
		return nil
	}

	cloned := make(map[string]string, len(hints))
	for key, hint := range hints {
		cloned[key] = hint
	}
	return cloned
}
