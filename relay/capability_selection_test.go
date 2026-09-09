package relay

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gorm.io/datatypes"

	"one-api/common"
	"one-api/common/config"
	"one-api/model"
	providersBase "one-api/providers/base"

	"github.com/gin-gonic/gin"
)

func TestChatCandidateSkipsCodexWhenMultipleChoicesCannotUseResponses(t *testing.T) {
	channels := []*model.Channel{
		capabilitySelectionChannel(1, config.ChannelTypeCodex, "gpt-5"),
		capabilitySelectionChannel(2, config.ChannelTypeOpenAI, "gpt-5"),
	}
	relay := capabilitySelectionChatRelay(t, `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}],"n":2}`, channels)
	if got := relay.getProvider().GetChannel().Id; got != 2 {
		t.Fatalf("Codex n=2 candidate was not skipped, selected channel %d", got)
	}
}

func TestChatResponsesTransportValidatesTargetCustomParameters(t *testing.T) {
	first := capabilitySelectionChannel(1, config.ChannelTypeOpenAI, "o3-pro")
	unsafeResponsesCustom := `{"overwrite":true,"store":true}`
	first.CustomParameter = &unsafeResponsesCustom
	second := capabilitySelectionChannel(2, config.ChannelTypeOpenAI, "o3-pro")
	relay := capabilitySelectionChatRelay(t, `{"model":"o3-pro","messages":[{"role":"user","content":"hello"}]}`, []*model.Channel{first, second})
	if got := relay.getProvider().GetChannel().Id; got != second.Id {
		t.Fatalf("Responses-target lifecycle custom parameter was not filtered, selected channel %d", got)
	}

	chatOnlyCustom := `{"web_search_options":{"search_context_size":"low"}}`
	second.CustomParameter = &chatOnlyCustom
	relay = capabilitySelectionChatRelay(t, `{"model":"o3-pro","messages":[{"role":"user","content":"hello"}]}`, []*model.Channel{second})
	if got := relay.getProvider().GetChannel().Id; got != second.Id {
		t.Fatalf("Responses transport custom parameter was misread as Chat billing input, selected channel %d", got)
	}
}

func TestOnlyChatAllowsExplicitEmptyToolsThroughSelectionAndFinalize(t *testing.T) {
	channel := capabilitySelectionChannel(1, config.ChannelTypeOpenAI, "gpt-5")
	channel.OnlyChat = true
	relay := capabilitySelectionChatRelay(t, `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}],"tools":[]}`, []*model.Channel{channel})
	if err := finalizeSelectedProviderRequest(relay); err != nil {
		t.Fatalf("empty tools changed OnlyChat semantics between selection and finalize: %v", err)
	}
}

func TestSpeechCandidateSkipsAdapterWithoutSSE(t *testing.T) {
	channels := []*model.Channel{
		capabilitySelectionChannel(1, config.ChannelTypeMiniMax, "gpt-5"),
		capabilitySelectionChannel(2, config.ChannelTypeOpenAI, "gpt-5"),
	}
	relay := capabilitySelectionSpeechRelay(t, `{"model":"gpt-5","input":"hello","voice":"alloy","stream_format":"sse"}`, channels)
	if got := relay.getProvider().GetChannel().Id; got != 2 {
		t.Fatalf("non-SSE speech candidate was not skipped, selected channel %d", got)
	}
}

func TestResponsesCrossProtocolCandidateUsesEffectiveChatOverlay(t *testing.T) {
	first := capabilitySelectionChannel(1, config.ChannelTypeAnthropic, "gpt-5")
	first.CompatibleResponse = true
	custom := `{"pre_add":true,"n":2}`
	first.CustomParameter = &custom
	second := capabilitySelectionChannel(2, config.ChannelTypeAnthropic, "gpt-5")
	second.CompatibleResponse = true

	relay := capabilitySelectionResponsesRelay(t, `{"model":"gpt-5","input":"hello","store":false}`, []*model.Channel{first, second})
	if got := relay.getProvider().GetChannel().Id; got != 2 {
		t.Fatalf("lossy Responses-to-Chat overlay candidate was not skipped, selected channel %d", got)
	}
}

func TestResponsesCrossProtocolDoesNotInterpretProviderNativeCustomParametersAsChat(t *testing.T) {
	channel := capabilitySelectionChannel(1, config.ChannelTypeAnthropic, "gpt-5")
	channel.CompatibleResponse = true
	nativeClaudeTools := `{"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`
	channel.CustomParameter = &nativeClaudeTools
	relay := capabilitySelectionResponsesRelay(t, `{"model":"gpt-5","input":"hello","store":false}`, []*model.Channel{channel})
	if got := relay.getProvider().GetChannel().Id; got != channel.Id {
		t.Fatalf("provider-native custom parameters were misread as Chat: selected channel %d", got)
	}
}

func TestDisabledCustomResponsesDoesNotInterpretChatNativeCustomAsResponses(t *testing.T) {
	channel := capabilitySelectionChannel(1, config.ChannelTypeCustom, "gpt-5")
	channel.CompatibleResponse = true
	baseURL := "https://compatible.example"
	channel.BaseURL = &baseURL
	plugin := datatypes.NewJSONType(model.PluginType{"endpoints": {"openai.chat_completions": map[string]any{"enabled": true, "upstream_url": ""}, "openai.responses": map[string]any{"enabled": false, "upstream_url": ""}}})
	channel.Plugin = &plugin
	chatNative := `{"previous_response_id":"chat-provider-extension"}`
	channel.CustomParameter = &chatNative
	relay := capabilitySelectionResponsesRelay(t, `{"model":"gpt-5","input":"hello","store":false}`, []*model.Channel{channel})
	if err := finalizeSelectedProviderRequest(relay); err != nil {
		t.Fatalf("Chat-native custom parameter was interpreted as Responses lifecycle: %v", err)
	}
	if relay.selectedDataPath != providersBase.DataPathCrossProtocol {
		t.Fatalf("disabled Responses endpoint selected path=%q", relay.selectedDataPath)
	}
}

func TestResponsesCrossProtocolSkipsOnlyChatForEffectiveTools(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   string
		preAdd string
	}{
		{name: "request tools", body: `{"model":"gpt-5","input":"hello","store":false,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`},
		{name: "pre-add tools", body: `{"model":"gpt-5","input":"hello","store":false}`, preAdd: `{"pre_add":true,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := capabilitySelectionChannel(1, config.ChannelTypeAnthropic, "gpt-5")
			first.CompatibleResponse = true
			first.OnlyChat = true
			if test.preAdd != "" {
				first.CustomParameter = &test.preAdd
			}
			second := capabilitySelectionChannel(2, config.ChannelTypeAnthropic, "gpt-5")
			second.CompatibleResponse = true
			relay := capabilitySelectionResponsesRelay(t, test.body, []*model.Channel{first, second})
			if got := relay.getProvider().GetChannel().Id; got != second.Id {
				t.Fatalf("OnlyChat candidate with effective tools was not skipped, selected channel %d", got)
			}
		})
	}
}

func TestResponsesClaudeFallbackMaterializesPreAddWithoutChangingSourceEnvelope(t *testing.T) {
	channel := capabilitySelectionChannel(1, config.ChannelTypeAnthropic, "gpt-5")
	channel.CompatibleResponse = true
	preAdd := `{"pre_add":true,"temperature":0.37}`
	channel.CustomParameter = &preAdd
	relay := capabilitySelectionResponsesRelay(t, `{"model":"gpt-5","input":"hello","store":false}`, []*model.Channel{channel})
	originalEnvelope := string(relay.rawEnvelope.Object.Raw)
	if err := finalizeSelectedProviderRequest(relay); err != nil {
		t.Fatalf("finalize Claude fallback: %v", err)
	}
	if relay.preparedChatRequest == nil || relay.preparedChatRequest.Temperature == nil || *relay.preparedChatRequest.Temperature != 0.37 {
		t.Fatalf("Claude fallback did not materialize pre_add: %#v", relay.preparedChatRequest)
	}
	if string(relay.rawEnvelope.Object.Raw) != originalEnvelope {
		t.Fatalf("Responses source envelope changed: got=%q want=%q", relay.rawEnvelope.Object.Raw, originalEnvelope)
	}
	canonical, ok := common.GetCanonicalRequestBody(relay.c)
	if !ok || !strings.Contains(string(canonical), `"temperature":0.37`) {
		t.Fatalf("published provider body missed pre_add: ok=%t body=%s", ok, canonical)
	}
}

func TestExplicitPinCapabilityFailureStopsBeforeProviderConstruction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	testDB := setupRelayTestDB(t, &model.Channel{})
	pinned := capabilitySelectionChannel(71, config.ChannelTypeJina, "gpt-5")
	pinned.CompatibleResponse = true
	if err := testDB.Create(pinned).Error; err != nil {
		t.Fatalf("create pinned channel: %v", err)
	}

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hello","store":false}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("specific_channel_id", pinned.Id)
	ctx.Set("token_group", "default")
	relay := NewRelayResponses(ctx)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("set request: %v", err)
	}
	err := relay.setProvider(relay.getOriginalModel())
	var gateErr *capabilityGateError
	if !errors.As(err, &gateErr) || relay.getProvider() != nil || ctx.GetInt("channel_id") != 0 {
		t.Fatalf("pinned unsupported request crossed provider construction boundary: err=%v provider=%T channel_id=%d", err, relay.getProvider(), ctx.GetInt("channel_id"))
	}
}

func capabilitySelectionChatRelay(t *testing.T, body string, channels []*model.Channel) *relayChat {
	t.Helper()
	ctx := capabilitySelectionContext(t, "/v1/chat/completions", body, channels)
	relay := NewRelayChat(ctx)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("set Chat request: %v", err)
	}
	ctx.Set("is_stream", relay.IsStream())
	if err := relay.setProvider(relay.getOriginalModel()); err != nil {
		t.Fatalf("select Chat provider: %v", err)
	}
	return relay
}

func capabilitySelectionSpeechRelay(t *testing.T, body string, channels []*model.Channel) *relaySpeech {
	t.Helper()
	ctx := capabilitySelectionContext(t, "/v1/audio/speech", body, channels)
	relay := NewRelaySpeech(ctx)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("set Speech request: %v", err)
	}
	ctx.Set("is_stream", relay.IsStream())
	if err := relay.setProvider(relay.getOriginalModel()); err != nil {
		t.Fatalf("select Speech provider: %v", err)
	}
	return relay
}

func capabilitySelectionResponsesRelay(t *testing.T, body string, channels []*model.Channel) *relayResponses {
	t.Helper()
	ctx := capabilitySelectionContext(t, "/v1/responses", body, channels)
	relay := NewRelayResponses(ctx)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("set Responses request: %v", err)
	}
	ctx.Set("is_stream", relay.IsStream())
	if err := relay.setProvider(relay.getOriginalModel()); err != nil {
		t.Fatalf("select Responses provider: %v", err)
	}
	return relay
}

func capabilitySelectionContext(t *testing.T, path, body string, channels []*model.Channel) *gin.Context {
	t.Helper()
	snapshot := snapshotChannelGroup()
	t.Cleanup(func() { restoreChannelGroup(snapshot) })
	choices := make(map[int]*model.ChannelChoice, len(channels))
	priorities := make([][]int, 0, len(channels))
	for _, channel := range channels {
		choices[channel.Id] = &model.ChannelChoice{Channel: channel}
		priorities = append(priorities, []int{channel.Id})
	}
	modelName := channels[0].Models
	model.ChannelGroup = model.ChannelsChooser{
		Channels:   choices,
		Rule:       map[string]map[string][][]int{"default": {modelName: priorities}},
		ModelGroup: map[string]map[string]bool{modelName: {"default": true}},
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("token_group", "default")
	return ctx
}

func capabilitySelectionChannel(id, channelType int, modelName string) *model.Channel {
	weight := uint(1)
	proxy := ""
	channel := &model.Channel{
		Id: id, Plugin: model.NewCustomEndpointPlugin(), Type: channelType, Status: config.ChannelStatusEnabled,
		Group: "default", Models: modelName, Weight: &weight, Proxy: &proxy,
	}
	if channelType == config.ChannelTypeCodex {
		channel.Key = `{"access_token":"access-token","account_id":"acct-123"}`
	}
	return channel
}
