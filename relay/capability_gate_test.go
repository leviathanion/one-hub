package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/model"
	"one-api/providers"
	providersBase "one-api/providers/base"
	"one-api/providers/claude"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func boolPointer(value bool) *bool { return &value }

func TestValidateChatSupportedSurfaceRejectsStoredChat(t *testing.T) {
	err := validateChatSupportedSurface(&types.ChatCompletionRequest{Store: boolPointer(true)}, nil)
	assertCapabilityGateError(t, err, "store")
	if err := validateChatSupportedSurface(&types.ChatCompletionRequest{Store: boolPointer(false)}, nil); err != nil {
		t.Fatalf("store=false must retain the existing HTTP path: %v", err)
	}
}

func TestChatInstallsAdapterCompatibilityGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.6","messages":[{"role":"user","content":"hi"}]}`))

	relay := NewRelayChat(c)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("set GPT-5.6 chat request: %v", err)
	}
	capability := currentRequestChannelCapability(c)
	if capability == nil {
		t.Fatal("expected Chat adapter compatibility gate")
	}
	if err := capability(&model.Channel{Type: config.ChannelTypeAzureV1}); err != nil {
		t.Fatalf("Azure's deployment region is not OpenAI Data Residency and must remain eligible: %v", err)
	}
	if err := capability(&model.Channel{Type: config.ChannelTypeOpenAI}); err != nil {
		t.Fatalf("official OpenAI channel should pass Chat adapter gate: %v", err)
	}

	regionalBaseURL := "https://eu.api.openai.com/v1"
	if err := capability(&model.Channel{Type: config.ChannelTypeOpenAI, BaseURL: &regionalBaseURL}); err == nil {
		t.Fatal("OpenAI Data Residency endpoint must be rejected")
	}
}

func TestResponsesOnlyChatModelRequiresNativeResponsesCandidate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"o3-pro","messages":[{"role":"user","content":"hi"}]}`))
	relay := NewRelayChat(c)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("set Responses-only chat request: %v", err)
	}
	capability := currentRequestChannelCapability(c)
	if err := capability(&model.Channel{Type: config.ChannelTypeOpenAI}); err != nil {
		t.Fatalf("official OpenAI Responses adapter should remain eligible: %v", err)
	}
	if err := capability(&model.Channel{Type: config.ChannelTypeAnthropic, CompatibleResponse: true}); err == nil {
		t.Fatal("Responses-to-Chat cross-protocol support cannot execute a Chat-to-Responses model route")
	}
}

func TestChatToResponsesRejectsUnmappedStopAndStreamOptions(t *testing.T) {
	request := &types.ChatCompletionRequest{Model: "o3-pro", Stop: []string{"DONE"}, Stream: true}
	fields := map[string]json.RawMessage{
		"model":          json.RawMessage(`"o3-pro"`),
		"stop":           json.RawMessage(`["DONE"]`),
		"stream":         json.RawMessage(`true`),
		"stream_options": json.RawMessage(`{"future_option":true}`),
	}
	assertCapabilityGateError(t, validateChatToResponsesRepresentability(request, fields), "stop")
	delete(fields, "stop")
	request.Stop = nil
	assertCapabilityGateError(t, validateChatToResponsesRepresentability(request, fields), "stream_options")
	fields["stream_options"] = json.RawMessage(`{"include_obfuscation":false}`)
	assertCapabilityGateError(t, validateChatToResponsesRepresentability(request, fields), "stream_options")
	fields["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	if err := validateChatToResponsesRepresentability(request, fields); err != nil {
		t.Fatalf("include_usage is relay-owned downstream behavior and must remain representable: %v", err)
	}
}

func TestChatAdapterCompatibilityRejectsOnlyLossyConversionBoundaries(t *testing.T) {
	customTool := &types.ChatCompletionTool{Type: types.ToolChoiceTypeCustom}
	customTool.ResponsesTool.Type = types.ToolChoiceTypeCustom
	customTool.ResponsesTool.Name = "shell"
	customRequest := &types.ChatCompletionRequest{Tools: []*types.ChatCompletionTool{customTool}}

	for _, channelType := range []int{config.ChannelTypeAnthropic, config.ChannelTypeGemini, config.ChannelTypeBedrock} {
		modelName := "gpt-5.6"
		if channelType == config.ChannelTypeBedrock {
			modelName = "claude-sonnet-4-20250514"
		}
		err := assessChatCapabilityForTest(&model.Channel{Type: channelType}, modelName, customRequest)
		assertCapabilityGateError(t, err, "tools")
	}
	for _, vertexModel := range []string{"claude-sonnet", "gemini-3"} {
		err := assessChatCapabilityForTest(&model.Channel{Type: config.ChannelTypeVertexAI}, vertexModel, customRequest)
		assertCapabilityGateError(t, err, "tools")
	}
	if err := assessChatCapabilityForTest(&model.Channel{Type: config.ChannelTypeOpenAI}, "gpt-5.6", customRequest); err != nil {
		t.Fatalf("native OpenAI wire must retain custom tools: %v", err)
	}

	functionRequest := &types.ChatCompletionRequest{Tools: []*types.ChatCompletionTool{{
		Type:     types.ToolChoiceTypeFunction,
		Function: types.ChatCompletionFunction{Name: "lookup"},
	}}}
	if err := assessChatCapabilityForTest(&model.Channel{Type: config.ChannelTypeAnthropic}, "claude-sonnet", functionRequest); err != nil {
		t.Fatalf("Claude adapter should retain ordinary function tools: %v", err)
	}
	if err := assessChatCapabilityForTest(&model.Channel{Type: config.ChannelTypeGemini}, "gemini-3", functionRequest); err != nil {
		t.Fatalf("Gemini adapter should retain ordinary function tools: %v", err)
	}
	functionRequest.ToolChoice = types.ToolChoiceTypeAuto
	if err := assessChatCapabilityForTest(&model.Channel{Type: config.ChannelTypeGemini}, "gemini-3", functionRequest); err != nil {
		t.Fatalf("Gemini's default auto tool choice should remain representable: %v", err)
	}
}

func TestChatAdapterCompatibilityRejectsLossyToolHistoryAndControls(t *testing.T) {
	parallel := false
	tests := []struct {
		name    string
		request *types.ChatCompletionRequest
		param   string
	}{
		{
			name: "custom tool history",
			request: &types.ChatCompletionRequest{Messages: []types.ChatCompletionMessage{{ToolCalls: []*types.ChatCompletionToolCalls{{
				Type:   types.ToolChoiceTypeCustom,
				Custom: &types.ChatCompletionToolCallsCustom{Name: "shell", Input: "pwd"},
			}}}}},
			param: "messages",
		},
		{name: "parallel tool calls", request: &types.ChatCompletionRequest{ParallelToolCalls: &parallel}, param: "parallel_tool_calls"},
		{name: "unsupported tool choice", request: &types.ChatCompletionRequest{ToolChoice: types.ToolChoiceTypeNone}, param: "tool_choice"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertCapabilityGateError(t, assessChatCapabilityForTest(
				&model.Channel{Type: config.ChannelTypeAnthropic}, "claude-sonnet", test.request,
			), test.param)
		})
	}
}

func TestChatAdapterCompatibilityRejectsMultipleChoicesAndGeminiAudio(t *testing.T) {
	two := 2
	for _, channel := range []*model.Channel{
		{Type: config.ChannelTypeAnthropic},
		{Type: config.ChannelTypeGemini},
		{Type: config.ChannelTypeVertexAI},
	} {
		modelName := "gemini-3"
		if channel.Type == config.ChannelTypeAnthropic {
			modelName = "claude-sonnet"
		}
		assertCapabilityGateError(t, assessChatCapabilityForTest(channel, modelName, &types.ChatCompletionRequest{N: &two}), "n")
	}
	assertCapabilityGateError(t, assessChatCapabilityForTest(
		&model.Channel{Type: config.ChannelTypeGemini}, "gemini-3",
		&types.ChatCompletionRequest{Modalities: []string{"text", "audio"}},
	), "audio")
}

func assessChatCapabilityForTest(channel *model.Channel, modelName string, request *types.ChatCompletionRequest) error {
	_, err := providers.AssessChatRequest(channel, modelName, request, nil)
	return providerCapabilityGateError(err)
}

func TestChatCapabilityRejectsResourceInjectedByChannelTransform(t *testing.T) {
	fields := map[string]json.RawMessage{
		"model":    json.RawMessage(`"gpt-5"`),
		"messages": json.RawMessage(`[{"role":"user","content":"hello"}]`),
	}
	request := &types.ChatCompletionRequest{Model: "gpt-5"}
	custom := `{"pre_add":true,"tools":[{"type":"file_search","vector_store_ids":["vs_shared"]}]}`
	channel := &model.Channel{Type: config.ChannelTypeOpenAI, CustomParameter: &custom}
	assertCapabilityGateError(t, requireChatChannelCompatibility("gpt-5", request, fields)(channel), "tools")
}

func TestChatRemoteMediaCapabilityUsesMappedProviderModel(t *testing.T) {
	fields := map[string]json.RawMessage{
		"model":    json.RawMessage(`"public-vision"`),
		"messages": json.RawMessage(`[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]`),
	}
	request := &types.ChatCompletionRequest{
		Model: "public-vision",
		Messages: []types.ChatCompletionMessage{{
			Role: types.ChatMessageRoleUser,
			Content: []types.ChatMessagePart{{
				Type:     types.ContentTypeImageURL,
				ImageURL: &types.ChatMessageImageURL{URL: "https://example.com/a.png"},
			}},
		}},
	}

	toClaude := `{"public-vision":"claude-sonnet-4-20250514"}`
	channel := &model.Channel{Type: config.ChannelTypeBedrock, ModelMapping: &toClaude}
	if err := requireChatChannelCompatibility(request.Model, request, fields)(channel); err != nil {
		t.Fatalf("mapped Bedrock Claude model should accept materialized media: %v", err)
	}

	toUnsupported := `{"public-vision":"amazon.nova-pro-v1:0"}`
	channel.ModelMapping = &toUnsupported
	assertCapabilityGateError(t, requireChatChannelCompatibility(request.Model, request, fields)(channel), "model")
}

func TestNativeClaudeRemoteMediaCapabilityUsesMappedProviderModel(t *testing.T) {
	request := &claude.ClaudeRequest{
		Model: "public-claude",
		Messages: []claude.Message{{Role: "user", Content: []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/a.png"}},
		}}},
	}
	toClaude := `{"public-claude":"claude-sonnet-4-20250514"}`
	channel := &model.Channel{Type: config.ChannelTypeBedrock, ModelMapping: &toClaude}
	if err := requireNativeClaudeRemoteMedia(request.Model, request)(channel); err != nil {
		t.Fatalf("mapped Bedrock Claude model should accept native image materialization: %v", err)
	}
	toUnsupported := `{"public-claude":"amazon.nova-pro-v1:0"}`
	channel.ModelMapping = &toUnsupported
	assertCapabilityGateError(t, requireNativeClaudeRemoteMedia(request.Model, request)(channel), "messages")
}

func TestResponsesCrossProtocolCapabilityComposesSelectedAdapterLimits(t *testing.T) {
	fields := map[string]json.RawMessage{
		"model": json.RawMessage(`"gpt-5"`),
		"store": json.RawMessage(`false`),
		"input": json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_file","file_data":"data:application/pdf;base64,AA==","filename":"a.pdf"}]}]`),
	}
	capability := requireResponsesRequestCompatibility(providersBase.OperationResponsesCreate, false, fields, "gpt-5")
	for _, channel := range []*model.Channel{
		{Type: config.ChannelTypeAnthropic, CompatibleResponse: true},
		{Type: config.ChannelTypeGemini, CompatibleResponse: true},
	} {
		assertCapabilityGateError(t, capability(channel), "messages")
	}

	fields["input"] = json.RawMessage(`[{"type":"custom_tool_call","call_id":"call_1","name":"shell","input":"pwd"}]`)
	capability = requireResponsesRequestCompatibility(providersBase.OperationResponsesCreate, false, fields, "gpt-5")
	for _, channel := range []*model.Channel{
		{Type: config.ChannelTypeAnthropic, CompatibleResponse: true},
		{Type: config.ChannelTypeGemini, CompatibleResponse: true},
	} {
		assertCapabilityGateError(t, capability(channel), "messages")
	}

	fields["input"] = json.RawMessage(`"hello"`)
	capability = requireResponsesRequestCompatibility(providersBase.OperationResponsesCreate, false, fields, "gpt-5")
	if err := capability(&model.Channel{Type: config.ChannelTypeAnthropic, CompatibleResponse: true}); err != nil {
		t.Fatalf("ordinary text input should remain representable by Claude: %v", err)
	}
}

func TestResponsesWSCustomParametersCompareOnlyEffectiveCurrentModelTransform(t *testing.T) {
	fields := map[string]json.RawMessage{
		"model": json.RawMessage(`"gpt-5"`),
		"store": json.RawMessage(`false`),
		"input": json.RawMessage(`"hello"`),
	}
	baseURL := "https://api.openai.com"
	other := `{"responses_ws_native":true}`

	perModelMiss := `{"per_model":true,"other-model":{"temperature":0.2}}`
	channel := &model.Channel{Type: config.ChannelTypeOpenAI, BaseURL: &baseURL, Other: other, CustomParameter: &perModelMiss}
	if err := requireResponsesWSAdapterSupport(false, fields, "gpt-5")(channel); err != nil {
		t.Fatalf("per-model miss must not disable native Responses WS: %v", err)
	}

	controlOnly := `{"overwrite":true,"model":"rewritten","stream":true,"remove_params":["model","stream"]}`
	channel.CustomParameter = &controlOnly
	if err := requireResponsesWSAdapterSupport(false, fields, "gpt-5")(channel); err != nil {
		t.Fatalf("relay-owned/control-only parameters must not disable native Responses WS: %v", err)
	}

	bodyChange := `{"temperature":0.2}`
	channel.CustomParameter = &bodyChange
	assertCapabilityGateError(t, requireResponsesWSAdapterSupport(false, fields, "gpt-5")(channel), "")
}

func TestResponsesStoreFalseUsesAdapterSupportForEveryModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, test := range []struct {
		name         string
		requestModel string
	}{
		{name: "OpenAI model name", requestModel: "gpt-5.6"},
		{name: "administrator-defined model name", requestModel: "private-model"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			body := fmt.Sprintf(`{"model":%q,"input":"hi","store":false}`, test.requestModel)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))

			relay := NewRelayResponses(c)
			if err := relay.setRequest(); err != nil {
				t.Fatalf("set responses request: %v", err)
			}
			capability := currentRequestChannelCapability(c)
			if capability == nil {
				t.Fatal("expected model-independent responses capability gate")
			}

			unsupported := &model.Channel{Type: config.ChannelTypeAnthropic}
			err := capability(unsupported)
			var gateErr *capabilityGateError
			if !errors.As(err, &gateErr) {
				t.Fatalf("adapter without responses.create must return a capability error, got %v", err)
			}
			if gateErr.status != http.StatusServiceUnavailable || gateErr.param != "" || gateErr.message != "channel adapter cannot relay operation responses.create" {
				t.Fatalf("unexpected structured capability error: %+v", gateErr)
			}

			baseURL := "https://compatible.example"
			if err := capability(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, BaseURL: &baseURL}); err != nil {
				t.Fatalf("OpenAI-compatible adapter should relay responses.create: %v", err)
			}
		})
	}
}

func TestResponsesCapabilityGateDoesNotFilterCodexParameterSemantics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi","store":false,"truncation":"auto"}`))

	relay := NewRelayResponses(c)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("set responses request: %v", err)
	}
	capability := currentRequestChannelCapability(c)
	if err := capability(&model.Channel{Type: config.ChannelTypeCodex}); err != nil {
		t.Fatalf("Codex truncation semantics belong to upstream: %v", err)
	}
	if err := capability(&model.Channel{Type: config.ChannelTypeOpenAI}); err != nil {
		t.Fatalf("OpenAI channel should remain selectable: %v", err)
	}
}

func TestValidateResponsesSupportedSurfaceRejectsUnsupportedResourcesBeforeProviderWork(t *testing.T) {
	tests := []struct {
		name      string
		request   types.OpenAIResponsesRequest
		raw       map[string]json.RawMessage
		operation responsesOperation
		param     string
	}{
		{name: "background", request: types.OpenAIResponsesRequest{Background: boolPointer(true)}, param: "background"},
		{name: "conversation", request: types.OpenAIResponsesRequest{Conversation: "conv_1"}, param: "conversation"},
		{name: "saved prompt", request: types.OpenAIResponsesRequest{Prompt: map[string]any{"id": "pmpt_1"}}, param: "prompt"},
		{name: "file input", raw: map[string]json.RawMessage{"input": json.RawMessage(`[{"type":"input_file","file_id":"file_1"}]`)}, param: "input"},
		{name: "file search", raw: map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"file_search","vector_store_ids":["vs_1"]}]`)}, param: "tools"},
		{name: "hosted shell auto", raw: map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"shell","environment":{"type":"container_auto"}}]`)}, param: "tools"},
		{name: "hosted shell reference", raw: map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"shell","environment":{"type":"container_reference","container_id":"cntr_1"}}]`)}, param: "tools"},
		{name: "code interpreter auto", raw: map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"code_interpreter","container":{"type":"auto"}}]`)}, param: "tools"},
		{name: "code interpreter reference", raw: map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"code_interpreter","container":"cntr_1"}]`)}, param: "tools"},
		{name: "code interpreter implicit container", raw: map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"code_interpreter"}]`)}, param: "tools"},
		{name: "image mask file", raw: map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"image_generation","input_image_mask":{"file_id":"file_1"}}]`)}, param: "tools"},
		{name: "uploaded skill", raw: map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"shell","environment":{"type":"local","skills":[{"type":"skill_reference","skill_id":"skill_1"}]}}]`)}, param: "tools"},
		{name: "future object resource", raw: map[string]json.RawMessage{"tools": json.RawMessage(`{"type":"provider_future","vector_store_ids":["vs_1"]}`)}, param: "tools"},
		{name: "compact multi agent", raw: map[string]json.RawMessage{"multi_agent": json.RawMessage(`{"enabled":true}`)}, operation: responsesOperationCompact, param: "multi_agent"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateResponsesSupportedSurface(&test.request, test.raw, test.operation)
			assertCapabilityGateError(t, err, test.param)
		})
	}
}

func TestValidateResponsesSupportedSurfaceSavedPromptErrorIsActionable(t *testing.T) {
	err := validateResponsesSupportedSurface(&types.OpenAIResponsesRequest{
		Prompt: map[string]any{"id": "pmpt_1"},
	}, nil, responsesOperationCreate)
	apiErr := capabilityGateAPIError(err)
	if apiErr == nil || apiErr.Param != "prompt" || openAIErrorCodeString(apiErr.Code, "") != unsupportedCapabilityCode {
		t.Fatalf("expected stable saved prompt capability error, got err=%v api=%+v", err, apiErr)
	}
	if !strings.Contains(apiErr.Message, "2026-11-30") || !strings.Contains(apiErr.Message, "instructions or input") {
		t.Fatalf("expected close date and migration guidance, got %q", apiErr.Message)
	}
}

func TestValidateResponsesSupportedSurfaceKeepsInlineAndLocalTools(t *testing.T) {
	raw := map[string]json.RawMessage{
		"input": json.RawMessage(`[{"type":"message","content":[{"type":"input_file","file_data":"data:application/pdf;base64,AA==","filename":"a.pdf"}]}]`),
		"tools": json.RawMessage(`[
			{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"file_id":{"type":"string"}}}},
			{"type":"shell","environment":{"type":"local"}},
			{"type":"local_shell"},
			{"type":"image_generation","input_image_mask":{"image_url":"data:image/png;base64,AA=="}}
		]`),
	}
	if err := validateResponsesSupportedSurface(&types.OpenAIResponsesRequest{}, raw, responsesOperationCreate); err != nil {
		t.Fatalf("inline file data, function schema, local shell and image generation should remain supported: %v", err)
	}
}

func TestValidateResponsesSupportedSurfaceLeavesProviderToolSchemaToUpstream(t *testing.T) {
	for name, tools := range map[string]json.RawMessage{
		"future union":             json.RawMessage(`[{"type":"future_tool","environment":{"type":"provider_future","future":{"keep":true}}}]`),
		"future hosted-like union": json.RawMessage(`[{"type":"future_tool","environment":{"type":"container_auto","future":{"keep":true}}}]`),
		"future type shape":        json.RawMessage(`[{"type":{"kind":"future"},"future":{"keep":true}}]`),
		"provider object":          json.RawMessage(`{"type":"provider_future","future":{"keep":true}}`),
		"implicit shell":           json.RawMessage(`[{"type":"shell"}]`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateResponsesSupportedSurface(&types.OpenAIResponsesRequest{}, map[string]json.RawMessage{"tools": tools}, responsesOperationCreate); err != nil {
				t.Fatalf("provider-owned tools schema was rejected before channel selection: %v", err)
			}
		})
	}
}

func TestResponsesWSAdapterSupportOnlyRejectsMappingForCurrentModel(t *testing.T) {
	unrelatedMapping := `{"gpt-4":"provider-gpt-4"}`
	channel := &model.Channel{Type: config.ChannelTypeOpenAI, Models: "gpt-5", ModelMapping: &unrelatedMapping}
	if err := requireResponsesWSAdapterSupport(false, nil, "gpt-5")(channel); err != nil {
		t.Fatalf("an unrelated model mapping must not disable native Responses WebSocket: %v", err)
	}

	currentMapping := `{"gpt-5":"provider-gpt-5"}`
	channel.ModelMapping = &currentMapping
	assertCapabilityGateError(t, requireResponsesWSAdapterSupport(false, nil, "gpt-5")(channel), "")

	identityMapping := `{"gpt-5":"gpt-5","gpt-4":"provider-gpt-4"}`
	channel.ModelMapping = &identityMapping
	if err := requireResponsesWSAdapterSupport(false, nil, "gpt-5")(channel); err != nil {
		t.Fatalf("an identity mapping for the current model must preserve native Responses WebSocket: %v", err)
	}
}

func TestSupportedSurfacePreservesUnknownServiceTier(t *testing.T) {
	for _, serviceTier := range []string{"", "auto", "default", "flex", "fast", "priority", "scale", "provider_future_tier"} {
		if err := validateChatSupportedSurface(&types.ChatCompletionRequest{ServiceTier: serviceTier}, nil); err != nil {
			t.Fatalf("Chat service tier %q belongs to the upstream: %v", serviceTier, err)
		}
		if err := validateResponsesSupportedSurface(&types.OpenAIResponsesRequest{ServiceTier: serviceTier}, nil, responsesOperationCreate); err != nil {
			t.Fatalf("Responses service tier %q belongs to the upstream: %v", serviceTier, err)
		}
	}
}

func TestValidateChatToResponsesRepresentabilityPreservesCustomToolHistory(t *testing.T) {
	request := &types.ChatCompletionRequest{
		Model: "gpt-5",
		Messages: []types.ChatCompletionMessage{
			{
				Role: types.ChatMessageRoleAssistant,
				ToolCalls: []*types.ChatCompletionToolCalls{{
					Id:   "call_1",
					Type: types.ToolChoiceTypeCustom,
					Custom: &types.ChatCompletionToolCallsCustom{
						Name:  "shell",
						Input: "echo ok",
					},
				}},
			},
			{Role: types.ChatMessageRoleTool, ToolCallID: "call_1", Content: []any{map[string]any{"type": types.ContentTypeText, "text": "ok"}}},
		},
		ToolChoice: map[string]any{"type": types.ToolChoiceTypeFunction, "function": map[string]any{"name": "lookup"}},
	}
	fields := map[string]json.RawMessage{
		"model":       json.RawMessage(`"gpt-5"`),
		"messages":    json.RawMessage(`[]`),
		"tool_choice": json.RawMessage(`{"type":"function","function":{"name":"lookup"}}`),
	}
	if err := validateChatToResponsesRepresentability(request, fields); err != nil {
		t.Fatalf("custom Chat tool history should be representable by Responses: %v", err)
	}

	request.Messages[0].ToolCalls[0].Custom = nil
	assertCapabilityGateError(t, validateChatToResponsesRepresentability(request, fields), "messages")
}

func TestValidateChatToResponsesRepresentabilityAllowsAssistantContentWithToolCalls(t *testing.T) {
	request := &types.ChatCompletionRequest{
		Model: "gpt-5",
		Messages: []types.ChatCompletionMessage{{
			Role:    types.ChatMessageRoleAssistant,
			Content: "I will check that now.",
			ToolCalls: []*types.ChatCompletionToolCalls{{
				Id:   "call_1",
				Type: types.ToolChoiceTypeFunction,
				Function: &types.ChatCompletionToolCallsFunction{
					Name:      "lookup",
					Arguments: `{"q":"ok"}`,
				},
			}},
		}},
	}
	fields := map[string]json.RawMessage{
		"model":    json.RawMessage(`"gpt-5"`),
		"messages": json.RawMessage(`[{"role":"assistant","content":"I will check that now.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"ok\"}"}}]}]`),
	}

	if err := validateChatToResponsesRepresentability(request, fields); err != nil {
		t.Fatalf("assistant content with tool calls should be representable: %v", err)
	}
	converted := request.ToResponsesRequest()
	inputs, ok := converted.Input.([]types.InputResponses)
	if !ok || len(inputs) != 2 || inputs[0].Type != types.InputTypeMessage || inputs[1].Type != types.InputTypeFunctionCall {
		t.Fatalf("representability gate allowed a lossy conversion: %#v", converted.Input)
	}
}

func TestValidateChatToResponsesRepresentabilityRejectsMultipleChoices(t *testing.T) {
	for _, value := range []int{2, 128} {
		value := value
		t.Run(fmt.Sprintf("n=%d", value), func(t *testing.T) {
			request := &types.ChatCompletionRequest{N: &value}
			fields := map[string]json.RawMessage{"n": json.RawMessage(fmt.Sprintf("%d", value))}
			assertCapabilityGateError(t, validateChatToResponsesRepresentability(request, fields), "n")
		})
	}

	one := 1
	if err := validateChatToResponsesRepresentability(
		&types.ChatCompletionRequest{N: &one},
		map[string]json.RawMessage{"n": json.RawMessage(`1`)},
	); err != nil {
		t.Fatalf("n=1 is losslessly representable by the Responses adapter: %v", err)
	}
}

func TestValidateChatToResponsesRepresentabilityRejectsLossyNestedFields(t *testing.T) {
	name := "worker"
	tests := []struct {
		name    string
		request *types.ChatCompletionRequest
		fields  map[string]json.RawMessage
		param   string
	}{
		{
			name:    "named participant",
			request: &types.ChatCompletionRequest{Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hi", Name: &name}}},
			fields:  map[string]json.RawMessage{"messages": json.RawMessage(`[{"role":"user","content":"hi","name":"worker"}]`)},
			param:   "messages",
		},
		{
			name:    "reasoning max tokens",
			request: &types.ChatCompletionRequest{Reasoning: &types.ChatReasoning{MaxTokens: 128}},
			fields:  map[string]json.RawMessage{"reasoning": json.RawMessage(`{"max_tokens":128}`)},
			param:   "reasoning",
		},
		{
			name:    "message extension",
			request: &types.ChatCompletionRequest{Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hi"}}},
			fields:  map[string]json.RawMessage{"messages": json.RawMessage(`[{"role":"user","content":"hi","future":true}]`)},
			param:   "messages",
		},
		{
			name:    "content part extension",
			request: &types.ChatCompletionRequest{Messages: []types.ChatCompletionMessage{{Role: "user", Content: []any{map[string]any{"type": "text", "text": "hi"}}}}},
			fields:  map[string]json.RawMessage{"messages": json.RawMessage(`[{"role":"user","content":[{"type":"text","text":"hi","future":true}]}]`)},
			param:   "messages",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertCapabilityGateError(t, validateChatToResponsesRepresentability(test.request, test.fields), test.param)
		})
	}
}

func TestValidateResponsesToChatRepresentabilityRejectsLossySurface(t *testing.T) {
	storeFalse := false
	maxToolCalls := 2
	parallelTrue := true
	functionTools := []types.ResponsesTools{{Type: "function", Name: "lookup"}}
	tests := []struct {
		name    string
		request types.OpenAIResponsesRequest
		raw     map[string]json.RawMessage
		param   string
	}{
		{name: "max tool calls", request: types.OpenAIResponsesRequest{Store: &storeFalse, MaxToolCalls: &maxToolCalls}, raw: map[string]json.RawMessage{"max_tool_calls": json.RawMessage(`2`)}, param: "max_tool_calls"},
		{name: "parallel function tools in stream", request: types.OpenAIResponsesRequest{Store: &storeFalse, Stream: true, Tools: functionTools, ParallelToolCalls: &parallelTrue}, raw: map[string]json.RawMessage{"stream": json.RawMessage(`true`), "tools": json.RawMessage(`[{"type":"function","name":"lookup"}]`), "parallel_tool_calls": json.RawMessage(`true`)}, param: "parallel_tool_calls"},
		{name: "stream function tools without serial mode", request: types.OpenAIResponsesRequest{Store: &storeFalse, Stream: true, Tools: functionTools}, raw: map[string]json.RawMessage{"stream": json.RawMessage(`true`), "tools": json.RawMessage(`[{"type":"function","name":"lookup"}]`)}, param: "parallel_tool_calls"},
		{name: "typed max tool calls without raw envelope", request: types.OpenAIResponsesRequest{Store: &storeFalse, MaxToolCalls: &maxToolCalls}, param: "max_tool_calls"},
		{name: "include", request: types.OpenAIResponsesRequest{Store: &storeFalse}, raw: map[string]json.RawMessage{"include": json.RawMessage(`["reasoning.encrypted_content"]`)}, param: "include"},
		{name: "truncation", request: types.OpenAIResponsesRequest{Store: &storeFalse, Truncation: "auto"}, raw: map[string]json.RawMessage{"truncation": json.RawMessage(`"auto"`)}, param: "truncation"},
		{name: "multi agent", request: types.OpenAIResponsesRequest{Store: &storeFalse}, raw: map[string]json.RawMessage{"multi_agent": json.RawMessage(`{"enabled":true}`)}, param: "multi_agent"},
		{name: "reasoning mode", request: types.OpenAIResponsesRequest{Store: &storeFalse}, raw: map[string]json.RawMessage{"reasoning": json.RawMessage(`{"effort":"high","mode":"pro"}`)}, param: "reasoning"},
		{name: "reasoning context", request: types.OpenAIResponsesRequest{Store: &storeFalse}, raw: map[string]json.RawMessage{"reasoning": json.RawMessage(`{"context":"all_turns"}`)}, param: "reasoning"},
		{name: "reasoning summary", request: types.OpenAIResponsesRequest{Store: &storeFalse}, raw: map[string]json.RawMessage{"reasoning": json.RawMessage(`{"summary":"auto"}`)}, param: "reasoning"},
		{name: "deprecated reasoning summary", request: types.OpenAIResponsesRequest{Store: &storeFalse}, raw: map[string]json.RawMessage{"reasoning": json.RawMessage(`{"generate_summary":"auto"}`)}, param: "reasoning"},
		{name: "custom tool", request: types.OpenAIResponsesRequest{Store: &storeFalse, Tools: []types.ResponsesTools{{Type: "custom", Name: "shell"}}}, raw: map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"custom","name":"shell"}]`)}, param: "tools"},
		{name: "programmatic caller", request: types.OpenAIResponsesRequest{Store: &storeFalse, Tools: []types.ResponsesTools{{Type: "function", Name: "lookup"}}}, raw: map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"function","name":"lookup","allowed_callers":["code_interpreter"]}]`)}, param: "tools"},
		{name: "hosted tool", request: types.OpenAIResponsesRequest{Store: &storeFalse, Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearch}}}, raw: map[string]json.RawMessage{"tools": json.RawMessage(`[{"type":"web_search"}]`)}, param: "tools"},
		{name: "unsupported tool choice", request: types.OpenAIResponsesRequest{Store: &storeFalse, ToolChoice: map[string]any{"type": "custom", "name": "shell"}}, raw: map[string]json.RawMessage{"tool_choice": json.RawMessage(`{"type":"custom","name":"shell"}`)}, param: "tool_choice"},
		{name: "reasoning input item", request: types.OpenAIResponsesRequest{Store: &storeFalse, Input: []any{map[string]any{"type": types.InputTypeReasoning, "summary": []any{}}}}, raw: map[string]json.RawMessage{"input": json.RawMessage(`[{"type":"reasoning","summary":[]}]`)}, param: "input"},
		{name: "message item extension", request: types.OpenAIResponsesRequest{Store: &storeFalse, Input: []any{map[string]any{"type": types.InputTypeMessage, "role": "user", "content": "hi", "future_metadata": true}}}, raw: map[string]json.RawMessage{"input": json.RawMessage(`[{"type":"message","role":"user","content":"hi","future_metadata":true}]`)}, param: "input"},
		{name: "content part extension", request: types.OpenAIResponsesRequest{Store: &storeFalse, Input: []any{map[string]any{"type": types.InputTypeMessage, "role": "user", "content": []any{map[string]any{"type": types.ContentTypeInputText, "text": "hi", "future": true}}}}}, raw: map[string]json.RawMessage{"input": json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi","future":true}]}]`)}, param: "input"},
		{name: "file id content", request: types.OpenAIResponsesRequest{Store: &storeFalse, Input: []any{map[string]any{"type": types.InputTypeMessage, "role": "user", "content": []any{map[string]any{"type": types.ContentTypeInputFile, "file_id": "file_1"}}}}}, raw: map[string]json.RawMessage{"input": json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file_1"}]}]`)}, param: "input"},
		{name: "tool output image", request: types.OpenAIResponsesRequest{Store: &storeFalse, Input: []any{map[string]any{"type": types.InputTypeFunctionCallOutput, "call_id": "call_1", "output": []any{map[string]any{"type": types.ContentTypeInputImage, "image_url": "https://example.com/a.png"}}}}}, raw: map[string]json.RawMessage{"input": json.RawMessage(`[{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_image","image_url":"https://example.com/a.png"}]}]`)}, param: "input"},
		{name: "tool output text extension", request: types.OpenAIResponsesRequest{Store: &storeFalse, Input: []any{map[string]any{"type": types.InputTypeFunctionCallOutput, "call_id": "call_1", "output": []any{map[string]any{"type": types.ContentTypeInputText, "text": "ok", "future": true}}}}}, raw: map[string]json.RawMessage{"input": json.RawMessage(`[{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"ok","future":true}]}]`)}, param: "input"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateResponsesToChatRepresentability(&test.request, test.raw)
			assertCapabilityGateError(t, err, test.param)
		})
	}
}

func TestValidateResponsesToChatRepresentabilityKeepsSharedSurface(t *testing.T) {
	storeFalse := false
	parallel := true
	raw := map[string]json.RawMessage{
		"model":               json.RawMessage(`"gpt-5"`),
		"input":               json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_file","file_data":"data:application/pdf;base64,AA==","filename":"a.pdf"}]},{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"ok"}]},{"type":"custom_tool_call","call_id":"call_2","name":"shell","input":"echo ok"},{"type":"custom_tool_call_output","call_id":"call_2","output":"ok"}]`),
		"store":               json.RawMessage(`false`),
		"service_tier":        json.RawMessage(`"priority"`),
		"processing_class":    json.RawMessage(`"flex"`),
		"safety_identifier":   json.RawMessage(`"user_123"`),
		"parallel_tool_calls": json.RawMessage(`true`),
		"reasoning":           json.RawMessage(`{"effort":"high"}`),
		"text":                json.RawMessage(`{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"},"strict":true},"verbosity":"low"}`),
		"tools":               json.RawMessage(`[{"type":"function","name":"lookup","parameters":{"type":"object"}}]`),
		"tool_choice":         json.RawMessage(`{"type":"function","name":"lookup"}`),
	}
	effort := "high"
	request := types.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: []any{
			map[string]any{"type": types.InputTypeMessage, "role": "user", "content": []any{map[string]any{"type": types.ContentTypeInputText, "text": "hi"}, map[string]any{"type": types.ContentTypeInputFile, "file_data": "data:application/pdf;base64,AA==", "filename": "a.pdf"}}},
			map[string]any{"type": types.InputTypeFunctionCallOutput, "call_id": "call_1", "output": []any{map[string]any{"type": types.ContentTypeInputText, "text": "ok"}}},
			map[string]any{"type": types.InputTypeCustomToolCall, "call_id": "call_2", "name": "shell", "input": "echo ok"},
			map[string]any{"type": types.InputTypeCustomToolCallOutput, "call_id": "call_2", "output": "ok"},
		},
		Store:             &storeFalse,
		ServiceTier:       "priority",
		ProcessingClass:   "flex",
		SafetyIdentifier:  "user_123",
		ParallelToolCalls: &parallel,
		Reasoning:         &types.ReasoningEffort{Effort: &effort},
		Text: &types.ResponsesText{
			Format:    &types.ResponsesTextFormat{Type: "json_schema", Name: "answer", Schema: map[string]any{"type": "object"}, Strict: true},
			Verbosity: "low",
		},
		ToolChoice: map[string]any{"type": "function", "name": "lookup"},
		Tools: []types.ResponsesTools{{
			Type:       "function",
			Name:       "lookup",
			Parameters: map[string]any{"type": "object"},
		}},
	}
	if err := validateResponsesToChatRepresentability(&request, raw); err != nil {
		t.Fatalf("expected shared Responses/Chat surface to remain representable: %v", err)
	}
}

func TestValidateResponsesToChatRepresentabilityKeepsSerialFunctionToolStream(t *testing.T) {
	storeFalse := false
	parallelFalse := false
	request := types.OpenAIResponsesRequest{
		Model:             "gpt-5",
		Input:             "hello",
		Store:             &storeFalse,
		Stream:            true,
		ParallelToolCalls: &parallelFalse,
		Tools: []types.ResponsesTools{{
			Type:       "function",
			Name:       "lookup",
			Parameters: map[string]any{"type": "object"},
		}},
	}
	raw := map[string]json.RawMessage{
		"model":               json.RawMessage(`"gpt-5"`),
		"input":               json.RawMessage(`"hello"`),
		"store":               json.RawMessage(`false`),
		"stream":              json.RawMessage(`true`),
		"parallel_tool_calls": json.RawMessage(`false`),
		"tools":               json.RawMessage(`[{"type":"function","name":"lookup","parameters":{"type":"object"}}]`),
	}
	if err := validateResponsesToChatRepresentability(&request, raw); err != nil {
		t.Fatalf("explicit serial function-tool stream should remain representable: %v", err)
	}
}

func TestValidateResponsesWSClientEnvelopeRejectsStreamIDLanes(t *testing.T) {
	err := validateResponsesWSClientEnvelope(map[string]json.RawMessage{"stream_id": json.RawMessage(`"lane-a"`)})
	assertCapabilityGateError(t, err, "stream_id")
	if err := validateResponsesWSClientEnvelope(map[string]json.RawMessage{}); err != nil {
		t.Fatalf("default websocket lane must remain supported: %v", err)
	}
}

func assertCapabilityGateError(t *testing.T, err error, param string) {
	t.Helper()
	apiErr := capabilityGateAPIError(err)
	if apiErr == nil || apiErr.Param != param || openAIErrorCodeString(apiErr.Code, "") != unsupportedCapabilityCode {
		t.Fatalf("expected unsupported capability for %q, got err=%v api=%+v", param, err, apiErr)
	}
}

func TestUnsupportedCapabilityHandlerUsesStableOpenAIError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/resp_1/cancel", nil)

	UnsupportedCapability("operation", "response cancellation is not supported")(c)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", recorder.Code)
	}
	var envelope types.OpenAIErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if openAIErrorCodeString(envelope.Error.Code, "") != unsupportedCapabilityCode || envelope.Error.Param != "operation" {
		t.Fatalf("expected stable unsupported_capability envelope, got %+v", envelope.Error)
	}
}
