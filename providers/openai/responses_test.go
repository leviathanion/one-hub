package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/common/responsesws"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestOpenAIResponsesHTTPBridgeURLPreflightUsesOpenContext(t *testing.T) {
	originalResolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	t.Cleanup(func() {
		net.DefaultResolver = originalResolver
	})

	proxy := ""
	channel := &model.Channel{Key: "sk-test", Type: config.ChannelTypeOpenAI, Proxy: &proxy}
	channel.SetProxy()
	provider := CreateOpenAIProvider(channel, "https://api.openai.com")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := provider.validateResponsesWSHTTPBridgeURL(ctx, "https://slow-dns.test/v1/responses")
	if err == nil {
		t.Fatal("expected canceled bridge URL preflight to fail")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expected bridge URL preflight to honor open context, took %s", elapsed)
	}
}

func TestHandlerChatStreamToolCallsFinishReasonFromToolEvent(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "gpt-5",
	}

	dataChan := make(chan string, 4)
	errChan := make(chan error, 1)

	added := []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","status":"in_progress","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SF\"}"}}`)
	handler.HandlerChatStream(&added, dataChan, errChan)

	completed := []byte(`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)
	handler.HandlerChatStream(&completed, dataChan, errChan)

	_ = mustReadChunk(t, dataChan) // tool_call delta chunk
	finalChunk := mustReadChunk(t, dataChan)
	finishReason := mustGetFinishReason(t, finalChunk)

	if finishReason != types.FinishReasonToolCalls {
		t.Fatalf("expected finish_reason=%q, got %q", types.FinishReasonToolCalls, finishReason)
	}

	select {
	case err := <-errChan:
		t.Fatalf("unexpected stream error: %v", err)
	default:
	}
}

func TestHandlerChatStreamToolCallArgumentsAcceptJSONObject(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "gpt-5",
	}

	dataChan := make(chan string, 2)
	errChan := make(chan error, 1)

	added := []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","status":"in_progress","call_id":"call_1","name":"lookup","arguments":{"city":"Paris","days":0}}}`)
	handler.HandlerChatStream(&added, dataChan, errChan)

	chunk := mustReadChunk(t, dataChan)
	if len(chunk.Choices) != 1 || len(chunk.Choices[0].Delta.ToolCalls) != 1 || chunk.Choices[0].Delta.ToolCalls[0].Function == nil {
		t.Fatalf("expected one tool call chunk, got %#v", chunk.Choices)
	}
	if got := chunk.Choices[0].Delta.ToolCalls[0].Function.Arguments; got != `{"city":"Paris","days":0}` {
		t.Fatalf("expected normalized object arguments, got %q", got)
	}

	select {
	case err := <-errChan:
		t.Fatalf("unexpected stream error: %v", err)
	default:
	}
}

func TestHandlerChatStreamToolCallsFinishReasonFromResponseOutput(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "gpt-5",
	}

	dataChan := make(chan string, 2)
	errChan := make(chan error, 1)

	completed := []byte(`data: {"type":"response.completed","response":{"id":"resp_2","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3},"output":[{"type":"function_call","id":"fc_2","status":"completed","call_id":"call_2","name":"lookup","arguments":"{}"}]}}`)
	handler.HandlerChatStream(&completed, dataChan, errChan)

	finalChunk := mustReadChunk(t, dataChan)
	finishReason := mustGetFinishReason(t, finalChunk)

	if finishReason != types.FinishReasonToolCalls {
		t.Fatalf("expected finish_reason=%q, got %q", types.FinishReasonToolCalls, finishReason)
	}

	select {
	case err := <-errChan:
		t.Fatalf("unexpected stream error: %v", err)
	default:
	}
}

func TestHandlerChatStreamStopFinishReasonWithoutToolCall(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "gpt-5",
	}

	dataChan := make(chan string, 2)
	errChan := make(chan error, 1)

	completed := []byte(`data: {"type":"response.completed","response":{"id":"resp_3","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)
	handler.HandlerChatStream(&completed, dataChan, errChan)

	finalChunk := mustReadChunk(t, dataChan)
	finishReason := mustGetFinishReason(t, finalChunk)

	if finishReason != types.FinishReasonStop {
		t.Fatalf("expected finish_reason=%q, got %q", types.FinishReasonStop, finishReason)
	}

	select {
	case err := <-errChan:
		t.Fatalf("unexpected stream error: %v", err)
	default:
	}
}

func TestHandlerResponsesStreamIgnoreNonTrackedEventWithKeyword(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "gpt-5",
	}

	dataChan := make(chan string, 1)
	errChan := make(chan error, 1)

	raw := `data: {"type":"response.reasoning.delta","delta":{"text":"contains response.completed text"}}`
	line := []byte(raw)
	handler.HandlerResponsesStream(&line, dataChan, errChan)

	select {
	case out := <-dataChan:
		if out != raw {
			t.Fatalf("expected passthrough %q, got %q", raw, out)
		}
	default:
		t.Fatal("expected passthrough data, got none")
	}

	select {
	case err := <-errChan:
		t.Fatalf("unexpected stream error: %v", err)
	default:
	}
}

func TestHandlerResponsesStreamTracksResponsesToolBilling(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "gpt-5",
	}

	dataChan := make(chan string, 4)
	errChan := make(chan error, 1)

	created := []byte(`data: {"type":"response.created","response":{"tools":[{"type":"web_search_preview","search_context_size":"high"}]}}`)
	handler.HandlerResponsesStream(&created, dataChan, errChan)

	webSearch := []byte(`data: {"type":"response.output_item.added","item":{"type":"web_search_call"}}`)
	handler.HandlerResponsesStream(&webSearch, dataChan, errChan)

	codeInterpreter := []byte(`data: {"type":"response.output_item.added","item":{"type":"code_interpreter_call"}}`)
	handler.HandlerResponsesStream(&codeInterpreter, dataChan, errChan)

	fileSearch := []byte(`data: {"type":"response.output_item.added","item":{"type":"file_search_call"}}`)
	handler.HandlerResponsesStream(&fileSearch, dataChan, errChan)

	if got := handler.Usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")].CallCount; got != 1 {
		t.Fatalf("expected responses stream handler to track web search billing, got %+v", handler.Usage.ExtraBilling)
	}
	if got := handler.Usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeCodeInterpreter, "")].CallCount; got != 1 {
		t.Fatalf("expected responses stream handler to track code interpreter billing, got %+v", handler.Usage.ExtraBilling)
	}
	if got := handler.Usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeFileSearch, "")].CallCount; got != 1 {
		t.Fatalf("expected responses stream handler to track file search billing, got %+v", handler.Usage.ExtraBilling)
	}

	select {
	case err := <-errChan:
		t.Fatalf("unexpected stream error: %v", err)
	default:
	}
}

func TestHandlerChatStreamTracksAdditionalToolBilling(t *testing.T) {
	handler := OpenAIResponsesStreamHandler{
		Usage:  &types.Usage{},
		Prefix: "data: ",
		Model:  "gpt-5",
	}

	dataChan := make(chan string, 2)
	errChan := make(chan error, 1)

	codeInterpreter := []byte(`data: {"type":"response.output_item.added","item":{"type":"code_interpreter_call"}}`)
	handler.HandlerChatStream(&codeInterpreter, dataChan, errChan)

	fileSearch := []byte(`data: {"type":"response.output_item.added","item":{"type":"file_search_call"}}`)
	handler.HandlerChatStream(&fileSearch, dataChan, errChan)

	if got := handler.Usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeCodeInterpreter, "")].CallCount; got != 1 {
		t.Fatalf("expected chat stream handler to track code interpreter billing, got %+v", handler.Usage.ExtraBilling)
	}
	if got := handler.Usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeFileSearch, "")].CallCount; got != 1 {
		t.Fatalf("expected chat stream handler to track file search billing, got %+v", handler.Usage.ExtraBilling)
	}

	select {
	case err := <-errChan:
		t.Fatalf("unexpected stream error: %v", err)
	default:
	}
}

func mustReadChunk(t *testing.T, dataChan <-chan string) types.ChatCompletionStreamResponse {
	t.Helper()

	select {
	case data := <-dataChan:
		var chunk types.ChatCompletionStreamResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("failed to parse stream chunk %q: %v", data, err)
		}
		return chunk
	default:
		t.Fatal("expected stream chunk, got none")
	}

	return types.ChatCompletionStreamResponse{}
}

func mustGetFinishReason(t *testing.T, chunk types.ChatCompletionStreamResponse) string {
	t.Helper()

	if len(chunk.Choices) == 0 {
		t.Fatal("chunk has no choices")
	}

	finishReason, ok := chunk.Choices[0].FinishReason.(string)
	if !ok {
		t.Fatalf("finish_reason should be string, got %#v", chunk.Choices[0].FinishReason)
	}

	return finishReason
}

func TestCompactResponsesOmitsStructuredInclude(t *testing.T) {
	body, _ := captureCompactRequestBody(t, nil, &types.OpenAIResponsesRequest{
		Model:   "gpt-5",
		Input:   "hello",
		Include: []string{"reasoning.encrypted_content"},
	})

	if _, exists := body["include"]; exists {
		t.Fatalf("expected compact request body to omit structured include, got %#v", body["include"])
	}
}

func TestCompactResponsesOmitsStructuredStoreButKeepsCompactFields(t *testing.T) {
	store := false
	body, _ := captureCompactRequestBody(t, nil, &types.OpenAIResponsesRequest{
		Model:                "gpt-5",
		Input:                "hello",
		Instructions:         "summarize",
		PreviousResponseID:   "resp_prev",
		PromptCacheKey:       "cache-key",
		PromptCacheRetention: "7d",
		Store:                &store,
	})

	if _, exists := body["store"]; exists {
		t.Fatalf("expected compact request body to omit structured store, got %#v", body["store"])
	}
	if body["model"] != "gpt-5" || body["input"] != "hello" {
		t.Fatalf("expected compact request body to keep core compact fields, got %#v", body)
	}
	if body["instructions"] != "summarize" {
		t.Fatalf("expected compact request body to preserve instructions, got %#v", body["instructions"])
	}
	if body["previous_response_id"] != "resp_prev" {
		t.Fatalf("expected compact request body to preserve previous_response_id, got %#v", body["previous_response_id"])
	}
	if body["prompt_cache_key"] != "cache-key" {
		t.Fatalf("expected compact request body to preserve prompt_cache_key, got %#v", body["prompt_cache_key"])
	}
	if body["prompt_cache_retention"] != "7d" {
		t.Fatalf("expected compact request body to preserve prompt_cache_retention, got %#v", body["prompt_cache_retention"])
	}
}

func TestCompactResponsesPreservesUnknownAllowExtraBodyFieldsButNotKnownInclude(t *testing.T) {
	rawBody := []byte(`{"model":"gpt-5","input":"hello","include":["raw-include"],"experimental_feature":"enabled"}`)
	rawMap := make(map[string]interface{})
	if err := json.Unmarshal(rawBody, &rawMap); err != nil {
		t.Fatalf("failed to decode raw body: %v", err)
	}

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(rawBody))
	ctx.Request.Header.Set("Content-Type", "application/json")
	common.SetReusableRequestBodyMap(ctx, rawBody, rawMap)

	body, _ := captureCompactRequestBody(t, func(provider *OpenAIProvider) {
		provider.Channel.AllowExtraBody = true
		provider.SetContext(ctx)
	}, &types.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: "hello",
	})

	if _, exists := body["include"]; exists {
		t.Fatalf("expected compact request body to drop known include from raw extra body, got %#v", body["include"])
	}
	if got := body["experimental_feature"]; got != "enabled" {
		t.Fatalf("expected compact request body to preserve unknown extra field, got %#v", got)
	}
}

func TestResponsesHTTPBridgeRequestDoesNotMergeCachedRawWSFields(t *testing.T) {
	rawBody := []byte(`{"type":"response.create","event_id":"evt_raw","model":"gpt-5","input":"raw","stream":false,"background":true,"stream_options":{"include_usage":true},"raw_extra":"leak"}`)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	common.SetReusableRequestBodyMap(ctx, rawBody, map[string]interface{}{
		"type":           "response.create",
		"event_id":       "evt_raw",
		"model":          "gpt-5",
		"input":          "raw",
		"stream":         false,
		"background":     true,
		"stream_options": map[string]interface{}{"include_usage": true},
		"raw_extra":      "leak",
	})

	proxy := ""
	customParameter := `{"operator_extra":"custom"}`
	provider := CreateOpenAIProvider(&model.Channel{
		Type:            config.ChannelTypeOpenAI,
		Key:             "sk-test",
		Proxy:           &proxy,
		AllowExtraBody:  true,
		CustomParameter: &customParameter,
	}, "https://example.test")
	provider.SetContext(ctx)

	req, errWithCode := provider.buildResponsesHTTPBridgeRequest(map[string]json.RawMessage{
		"model":        json.RawMessage(`"gpt-5"`),
		"input":        json.RawMessage(`"sanitized"`),
		"future_field": json.RawMessage(`{"kept":true}`),
	}, "https://example.test/v1/responses", map[string]string{"Authorization": "Bearer sk-test"}, "gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected bridge request build to succeed, got %v", errWithCode.Message)
	}
	defer req.Body.Close()

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("failed to read bridge request body: %v", err)
	}
	body := make(map[string]interface{})
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("failed to decode bridge request body: %v", err)
	}

	for _, forbidden := range []string{"type", "event_id", "background", "stream_options", "raw_extra"} {
		if _, ok := body[forbidden]; ok {
			t.Fatalf("expected bridge body not to merge raw field %q, got %#v", forbidden, body)
		}
	}
	if body["stream"] != true {
		t.Fatalf("expected bridge body to force stream=true, got %#v", body["stream"])
	}
	if body["input"] != "sanitized" || body["operator_extra"] != "custom" {
		t.Fatalf("expected sanitized/custom fields to be preserved, got %#v", body)
	}
	if future, ok := body["future_field"].(map[string]interface{}); !ok || future["kept"] != true {
		t.Fatalf("expected sanitized unknown create-body field to be preserved, got %#v", body["future_field"])
	}
}

func TestResponsesHTTPBridgeAzureUsesBearerAuthAndFiltersModelAuthHeaders(t *testing.T) {
	proxy := ""
	modelHeaders := `{"Authorization":"Bearer should-not-send","api-key":"evil-api-key","X-Gateway-Auth":"azure-gateway"}`
	provider := CreateOpenAIProvider(&model.Channel{
		Type:         config.ChannelTypeAzure,
		Key:          "azure-key",
		Proxy:        &proxy,
		ModelHeaders: &modelHeaders,
		Other:        `{"api_version":"2024-10-01-preview"}`,
	}, "https://example.openai.azure.com")
	provider.IsAzure = true

	req, errWithCode := provider.buildResponsesHTTPBridgeRequest(map[string]json.RawMessage{
		"model": json.RawMessage(`"gpt-5"`),
		"input": json.RawMessage(`[]`),
	}, "https://example.test/v1/responses", provider.requestHeaders(openAIRequestAuthBearer), "gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected bridge request build to succeed, got %v", errWithCode.Message)
	}
	defer req.Body.Close()

	if got := req.Header.Get("Authorization"); got != "Bearer azure-key" {
		t.Fatalf("expected bridge bearer auth header, got %q", got)
	}
	if got := req.Header.Get("Api-Key"); got != "" {
		t.Fatalf("expected bridge not to send api-key header, got %q", got)
	}
	if got := req.Header.Get("X-Gateway-Auth"); got != "azure-gateway" {
		t.Fatalf("expected non-auth custom header to remain, got %q", got)
	}
}

func TestResponsesHTTPBridgeRequestDeepMergesCustomParameterObjects(t *testing.T) {
	proxy := ""
	customParameter := `{"metadata":{"operator":"custom","nested":{"operator":true}}}`
	provider := CreateOpenAIProvider(&model.Channel{
		Type:            config.ChannelTypeOpenAI,
		Key:             "sk-test",
		Proxy:           &proxy,
		CustomParameter: &customParameter,
	}, "https://api.openai.com")

	req, errWithCode := provider.buildResponsesHTTPBridgeRequest(map[string]json.RawMessage{
		"model":    json.RawMessage(`"gpt-5"`),
		"input":    json.RawMessage(`"hello"`),
		"metadata": json.RawMessage(`{"client":"kept","nested":{"client":true}}`),
	}, "https://example.test/v1/responses", map[string]string{"Authorization": "Bearer sk-test"}, "gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected bridge request build to succeed, got %v", errWithCode.Message)
	}
	defer req.Body.Close()

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("failed to read bridge request body: %v", err)
	}
	body := make(map[string]interface{})
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("failed to decode bridge request body: %v", err)
	}
	metadata, ok := body["metadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected metadata object, got %#v", body["metadata"])
	}
	if metadata["client"] != "kept" || metadata["operator"] != "custom" {
		t.Fatalf("expected metadata to preserve client and merge custom keys, got %#v", metadata)
	}
	nested, ok := metadata["nested"].(map[string]interface{})
	if !ok || nested["client"] != true || nested["operator"] != true {
		t.Fatalf("expected nested metadata to be deep-merged, got %#v", metadata["nested"])
	}
}

func TestResponsesHTTPBridgeRequestCustomParameterCannotRemoveBridgeStream(t *testing.T) {
	proxy := ""
	customParameter := `{"remove_params":["stream"],"background":false}`
	provider := CreateOpenAIProvider(&model.Channel{
		Type:            config.ChannelTypeOpenAI,
		Key:             "sk-test",
		Proxy:           &proxy,
		CustomParameter: &customParameter,
	}, "https://example.test")

	req, errWithCode := provider.buildResponsesHTTPBridgeRequest(map[string]json.RawMessage{
		"model":  json.RawMessage(`"gpt-5"`),
		"input":  json.RawMessage(`"hello"`),
		"stream": json.RawMessage(`true`),
	}, "https://example.test/v1/responses", map[string]string{"Authorization": "Bearer sk-test"}, "gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected bridge request build to succeed, got %v", errWithCode.Message)
	}
	defer req.Body.Close()

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("failed to read bridge request body: %v", err)
	}
	body := make(map[string]interface{})
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("failed to decode bridge request body: %v", err)
	}
	if body["stream"] != true {
		t.Fatalf("expected bridge custom_parameter normalization to restore stream=true, got %#v", body)
	}
	if _, ok := body["background"]; ok {
		t.Fatalf("expected bridge custom_parameter normalization to strip background=false, got %#v", body)
	}
}

func TestResponsesHTTPBridgeRequestRejectsCustomBackgroundTrue(t *testing.T) {
	proxy := ""
	customParameter := `{"background":true}`
	provider := CreateOpenAIProvider(&model.Channel{
		Type:            config.ChannelTypeOpenAI,
		Key:             "sk-test",
		Proxy:           &proxy,
		CustomParameter: &customParameter,
	}, "https://example.test")

	req, errWithCode := provider.buildResponsesHTTPBridgeRequest(map[string]json.RawMessage{
		"model": json.RawMessage(`"gpt-5"`),
		"input": json.RawMessage(`"hello"`),
	}, "https://example.test/v1/responses", map[string]string{"Authorization": "Bearer sk-test"}, "gpt-5")
	if req != nil {
		t.Fatalf("expected rejected bridge request not to return req, got %+v", req)
	}
	if errWithCode == nil || errWithCode.StatusCode != http.StatusBadRequest || errWithCode.Code != "unsupported_responses_ws_bridge_field" {
		t.Fatalf("expected custom background=true to be rejected as unsupported bridge field, got %+v", errWithCode)
	}
}

func TestResponsesWSBridgeOpenerReportsPreSendBuildErrorAsPrepareError(t *testing.T) {
	proxy := ""
	customParameter := `{"background":true}`
	provider := CreateOpenAIProvider(&model.Channel{
		Type:            config.ChannelTypeOpenAI,
		Key:             "sk-test",
		Other:           `{"responses_ws_self_hosted":true}`,
		Proxy:           &proxy,
		CustomParameter: &customParameter,
	}, "https://127.0.0.1:1")
	opener := openAIResponsesWSBridgeOpener{provider: provider, model: "gpt-5"}

	stream, providerErr, prepareErr := opener.OpenBridgeStream(context.Background(), responsesws.BridgeStreamRequest{
		Frame: responsesws.NewTextFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hello"}`)),
	})
	if stream != nil {
		t.Fatalf("expected no stream for pre-send build failure, got %+v", stream)
	}
	if providerErr != nil {
		t.Fatalf("expected pre-send build failure not to be provider rejection evidence, got %+v", providerErr)
	}
	if prepareErr == nil {
		t.Fatal("expected pre-send build failure to be returned as prepare error")
	}
	var apiErr *types.OpenAIErrorWithStatusCode
	if !errors.As(prepareErr, &apiErr) || apiErr.Code != "unsupported_responses_ws_bridge_field" {
		t.Fatalf("expected unsupported bridge field API error in prepare path, got %v", prepareErr)
	}
}

func TestCompactResponsesCustomParameterCanRestoreIncludeWithPreAdd(t *testing.T) {
	customParameter := `{"pre_add":true,"overwrite":true,"include":["from_custom"]}`
	body, _ := captureCompactRequestBody(t, func(provider *OpenAIProvider) {
		provider.Channel.CustomParameter = &customParameter
	}, &types.OpenAIResponsesRequest{
		Model:   "gpt-5",
		Input:   "hello",
		Include: []string{"from_client"},
	})

	includeValues, ok := body["include"].([]interface{})
	if !ok || len(includeValues) != 1 || includeValues[0] != "from_custom" {
		t.Fatalf("expected custom_parameter to restore include after compact cleanup, got %#v", body["include"])
	}
}

func captureCompactRequestBody(t *testing.T, configure func(*OpenAIProvider), request *types.OpenAIResponsesRequest) (map[string]interface{}, *OpenAIProvider) {
	t.Helper()

	var bodyBytes []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{
		Type:  config.ChannelTypeOpenAI,
		Key:   "sk-test",
		Proxy: &proxy,
	}, server.URL)
	provider.Usage = &types.Usage{}

	if configure != nil {
		configure(provider)
	}

	if _, errWithCode := provider.CompactResponses(request); errWithCode != nil {
		t.Fatalf("CompactResponses returned error: %v", errWithCode.Message)
	}

	requestBody := make(map[string]interface{})
	if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
		t.Fatalf("failed to decode compact request body: %v", err)
	}

	return requestBody, provider
}
