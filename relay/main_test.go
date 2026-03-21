package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/model"

	"github.com/gin-gonic/gin"
)

func TestApplyPreMappingBeforeRequestReSelectsProviderWhenToolsAreInjected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalChannelGroup := model.ChannelGroup
	t.Cleanup(func() {
		model.ChannelGroup = originalChannelGroup
	})

	weight := uint(1)
	proxy := ""
	preAdd := `{"pre_add":true,"tools":[{"type":"function","function":{"name":"lookup_weather","description":"lookup weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`

	model.ChannelGroup = model.ChannelsChooser{
		Channels: map[int]*model.ChannelChoice{
			1: {
				Channel: &model.Channel{
					Id:              1,
					Type:            config.ChannelTypeOpenAI,
					Status:          config.ChannelStatusEnabled,
					Group:           "default",
					Models:          "gpt-4o",
					Weight:          &weight,
					Proxy:           &proxy,
					OnlyChat:        true,
					CustomParameter: &preAdd,
				},
			},
			2: {
				Channel: &model.Channel{
					Id:       2,
					Type:     config.ChannelTypeOpenAI,
					Status:   config.ChannelStatusEnabled,
					Group:    "default",
					Models:   "gpt-4o",
					Weight:   &weight,
					Proxy:    &proxy,
					OnlyChat: false,
				},
			},
		},
		Rule: map[string]map[string][][]int{
			"default": {
				"gpt-4o": {{1}, {2}},
			},
		},
		ModelGroup: map[string]map[string]bool{
			"gpt-4o": {
				"default": true,
			},
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("token_group", "default")

	relay := NewRelayChat(ctx)
	applyPreMappingBeforeRequest(ctx)

	if !ctx.GetBool("skip_only_chat") {
		t.Fatal("expected pre-mapping to refresh skip_only_chat after injected tools are applied")
	}

	if err := relay.setRequest(); err != nil {
		t.Fatalf("setRequest failed: %v", err)
	}

	ctx.Set("is_stream", relay.IsStream())
	if relay.chatRequest.Tools == nil {
		t.Fatal("expected injected tools to be visible before provider re-selection")
	}

	if err := relay.setProvider(relay.getOriginalModel()); err != nil {
		t.Fatalf("setProvider failed: %v", err)
	}

	if got := relay.getProvider().GetChannel().Id; got != 2 {
		t.Fatalf("expected provider to be re-selected onto non-OnlyChat channel, got %d", got)
	}

	if !ctx.GetBool(config.GinRequestBodyReparseKey) {
		t.Fatal("expected provider re-selection to request a body reparse")
	}

	if err := reparseRequestAfterProviderSelection(relay); err != nil {
		t.Fatalf("reparseRequestAfterProviderSelection failed: %v", err)
	}

	if relay.chatRequest.Tools != nil {
		t.Fatalf("expected reparsed request to drop injected tools, got %#v", relay.chatRequest.Tools)
	}
	if ctx.GetBool("skip_only_chat") {
		t.Fatal("expected skip_only_chat to be refreshed from the re-selected provider body")
	}

	requestMap, err := common.CloneReusableBodyMap(ctx)
	if err != nil {
		t.Fatalf("CloneReusableBodyMap failed: %v", err)
	}
	if _, exists := requestMap["tools"]; exists {
		encoded, _ := json.Marshal(requestMap["tools"])
		t.Fatalf("expected request body to be rebuilt from the original payload, got tools=%s", encoded)
	}
}
