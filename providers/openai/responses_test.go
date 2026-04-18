package openai

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

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
