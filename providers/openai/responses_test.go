package openai

import (
	"encoding/json"
	"testing"

	"one-api/types"
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
