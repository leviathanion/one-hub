package codex

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/internal/requesthints"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestRoutingHintSettingsSetFromJSONRejectsInvalidRegex(t *testing.T) {
	t.Run("model regex", func(t *testing.T) {
		var settings RoutingHintSettings
		err := settings.SetFromJSON(`{"prompt_cache_key_strategy":"auto","model_regex":"["}`)
		if err == nil {
			t.Fatal("expected invalid model regex to fail")
		}
		if !strings.Contains(err.Error(), "model_regex") {
			t.Fatalf("expected model_regex validation error, got %v", err)
		}
	})

	t.Run("user agent regex", func(t *testing.T) {
		var settings RoutingHintSettings
		err := settings.SetFromJSON(`{"prompt_cache_key_strategy":"auto","user_agent_regex":"["}`)
		if err == nil {
			t.Fatal("expected invalid user agent regex to fail")
		}
		if !strings.Contains(err.Error(), "user_agent_regex") {
			t.Fatalf("expected user_agent_regex validation error, got %v", err)
		}
	})
}

func TestCodexResponsesHintResolverSupportsArbitraryModelsWhenUnfiltered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("token_id", 42)

	originalSettings := RoutingHintSettingsInstance
	RoutingHintSettingsInstance = RoutingHintSettings{PromptCacheKeyStrategy: codexPromptCacheStrategyAuto}
	t.Cleanup(func() {
		RoutingHintSettingsInstance = originalSettings
	})

	request := &types.OpenAIResponsesRequest{Model: "future-model"}
	hints := (codexResponsesHintResolver{}).ResolveResponsesHints(ctx, request)
	if got := hints[requesthints.ResponsesPromptCacheKey]; got == "" {
		t.Fatalf("expected unfiltered routing hint policy to support an arbitrary model, got %#v", hints)
	}
}
