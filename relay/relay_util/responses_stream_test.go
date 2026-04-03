package relay_util

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestResponsesStreamConverterStoresReasoningSummaryOnSummaryField(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/", nil)

	background := true
	store := false
	maxToolCalls := 2
	effort := "medium"
	summary := "auto"
	converter := NewOpenAIResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{
		Model:              "gpt-5",
		Background:         &background,
		Instructions:       "Answer briefly.",
		MaxToolCalls:       &maxToolCalls,
		PreviousResponseID: "resp_prev",
		Reasoning: &types.ReasoningEffort{
			Effort:  &effort,
			Summary: &summary,
		},
		Store: &store,
		Text: &types.ResponsesText{
			Verbosity: "low",
		},
	}, &types.Usage{})

	converter.ProcessStreamData(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"plan step"},"finish_reason":null}]}`)
	converter.ProcessStreamData(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	converter.ProcessStreamData("[DONE]")

	events := parseSSEEvents(t, recorder.Body.String())

	added := mustUnmarshalEvent(t, events, "response.reasoning_summary_part.added")
	if added.SummaryIndex == nil || *added.SummaryIndex != 0 {
		t.Fatalf("expected summary_index=0 on reasoning_summary_part.added, got %#v", added.SummaryIndex)
	}
	if added.ContentIndex != nil {
		t.Fatalf("expected content_index to be nil on reasoning_summary_part.added, got %#v", added.ContentIndex)
	}

	delta := mustUnmarshalEvent(t, events, "response.reasoning_summary_text.delta")
	if delta.SummaryIndex == nil || *delta.SummaryIndex != 0 {
		t.Fatalf("expected summary_index=0 on reasoning_summary_text.delta, got %#v", delta.SummaryIndex)
	}
	if delta.ContentIndex != nil {
		t.Fatalf("expected content_index to be nil on reasoning_summary_text.delta, got %#v", delta.ContentIndex)
	}

	completed := mustUnmarshalEvent(t, events, "response.completed")
	if completed.Response == nil {
		t.Fatal("expected response.completed payload to include response")
	}

	if got := completed.Response.Status; got != types.ResponseStatusCompleted {
		t.Fatalf("expected response status %q, got %q", types.ResponseStatusCompleted, got)
	}
	if completed.Response.Instructions != "Answer briefly." {
		t.Fatalf("expected instructions to be copied, got %#v", completed.Response.Instructions)
	}
	if completed.Response.Reasoning == nil || completed.Response.Reasoning.Effort == nil || *completed.Response.Reasoning.Effort != "medium" {
		t.Fatalf("expected reasoning effort to be copied, got %#v", completed.Response.Reasoning)
	}
	if completed.Response.MaxToolCalls == nil || *completed.Response.MaxToolCalls != maxToolCalls {
		t.Fatalf("expected max_tool_calls to be copied, got %#v", completed.Response.MaxToolCalls)
	}
	if completed.Response.PreviousResponseID != "resp_prev" {
		t.Fatalf("expected previous_response_id to be copied, got %q", completed.Response.PreviousResponseID)
	}
	if completed.Response.Store == nil || *completed.Response.Store != store {
		t.Fatalf("expected store to be copied, got %#v", completed.Response.Store)
	}

	textConfig, ok := completed.Response.Text.(map[string]any)
	if !ok {
		t.Fatalf("expected text config to unmarshal into a map, got %T", completed.Response.Text)
	}
	if textConfig["verbosity"] != "low" {
		t.Fatalf("expected text verbosity %q, got %#v", "low", textConfig["verbosity"])
	}

	if len(completed.Response.Output) != 1 {
		t.Fatalf("expected exactly one output item, got %d", len(completed.Response.Output))
	}

	output := completed.Response.Output[0]
	if output.Type != types.InputTypeReasoning {
		t.Fatalf("expected reasoning output, got %q", output.Type)
	}

	if len(output.Summary) != 1 || output.Summary[0].Text != "plan step" {
		t.Fatalf("unexpected reasoning summary: %#v", output.Summary)
	}

	if output.Content != nil {
		t.Fatalf("expected reasoning content to remain nil, got %#v", output.Content)
	}
}

func TestResponsesStreamConverterResetsPartIndexesPerOutputItem(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/", nil)

	converter := NewOpenAIResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{Model: "gpt-5"}, &types.Usage{})

	converter.ProcessStreamData(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"plan one"},"finish_reason":null}]}`)
	converter.ProcessStreamData(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"content":"answer one"},"finish_reason":null}]}`)
	converter.ProcessStreamData(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"reasoning_content":"plan two"},"finish_reason":null}]}`)
	converter.ProcessStreamData(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"content":"answer two"},"finish_reason":null}]}`)
	converter.ProcessStreamData(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	converter.ProcessStreamData("[DONE]")

	events := parseSSEEvents(t, recorder.Body.String())

	reasoningAdded := mustUnmarshalAllEvents(t, events, "response.reasoning_summary_part.added")
	if len(reasoningAdded) != 2 {
		t.Fatalf("expected two reasoning_summary_part.added events, got %d", len(reasoningAdded))
	}
	for i, event := range reasoningAdded {
		if event.SummaryIndex == nil || *event.SummaryIndex != 0 {
			t.Fatalf("expected reasoning summary index 0 for item %d, got %#v", i, event.SummaryIndex)
		}
	}

	contentAdded := mustUnmarshalAllEvents(t, events, "response.content_part.added")
	if len(contentAdded) != 2 {
		t.Fatalf("expected two content_part.added events, got %d", len(contentAdded))
	}
	for i, event := range contentAdded {
		if event.ContentIndex == nil || *event.ContentIndex != 0 {
			t.Fatalf("expected content index 0 for item %d, got %#v", i, event.ContentIndex)
		}
	}
}

func TestResponsesStreamConverterFunctionArgumentsDoNotDuplicate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/", nil)

	converter := NewOpenAIResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{Model: "gpt-5"}, &types.Usage{})

	converter.ProcessStreamData(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"{\"a\":"}}]},"finish_reason":null}]}`)
	converter.ProcessStreamData(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`)
	converter.ProcessStreamData("[DONE]")

	events := parseSSEEvents(t, recorder.Body.String())
	done := mustUnmarshalEvent(t, events, "response.function_call_arguments.done")
	if done.Arguments == nil {
		t.Fatal("expected arguments in function_call_arguments.done")
	}
	arguments, ok := done.Arguments.(string)
	if !ok {
		t.Fatalf("expected arguments to be a string, got %T", done.Arguments)
	}
	if arguments != `{"a":1}` {
		t.Fatalf("expected merged arguments without duplication, got %q", arguments)
	}
}

func TestResponsesStreamConverterFinalResponseAvailableAfterDone(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/", nil)

	converter := NewOpenAIResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "pc-stream-final",
	}, &types.Usage{})

	converter.ProcessStreamData(`{"id":"chatcmpl_final","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}`)
	converter.ProcessStreamData(`{"id":"chatcmpl_final","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	converter.ProcessStreamData("[DONE]")

	finalResponse := converter.FinalResponse()
	if finalResponse == nil {
		t.Fatal("expected final response to be available after stream completion")
	}
	if finalResponse.ID != "chatcmpl_final" {
		t.Fatalf("expected final response id %q, got %q", "chatcmpl_final", finalResponse.ID)
	}
	if finalResponse.PromptCacheKey != "pc-stream-final" {
		t.Fatalf("expected prompt_cache_key %q, got %q", "pc-stream-final", finalResponse.PromptCacheKey)
	}
	if finalResponse.Status != types.ResponseStatusCompleted {
		t.Fatalf("expected final response status %q, got %q", types.ResponseStatusCompleted, finalResponse.Status)
	}
}

func TestResponsesStreamObserverTracksTerminalResponse(t *testing.T) {
	observer := NewOpenAIResponsesStreamObserver()

	observer.ObserveRawLine("event: response.created\n")
	observer.ObserveRawLine("data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_created\",\"object\":\"response\",\"prompt_cache_key\":\"pc-created\",\"status\":\"in_progress\"}}\n")
	observer.ObserveRawLine("\n")
	observer.ObserveRawLine("event: response.completed\n")
	observer.ObserveRawLine("data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_final\",\"object\":\"response\",\"prompt_cache_key\":\"pc-final\",\"status\":\"completed\"}}\n")
	observer.ObserveRawLine("\n")
	observer.ObserveRawLine("data: [DONE]\n")

	finalResponse := observer.FinalResponse()
	if finalResponse == nil {
		t.Fatal("expected observer to keep terminal response")
	}
	if finalResponse.ID != "resp_final" {
		t.Fatalf("expected terminal response id %q, got %q", "resp_final", finalResponse.ID)
	}
	if finalResponse.PromptCacheKey != "pc-final" {
		t.Fatalf("expected terminal prompt_cache_key %q, got %q", "pc-final", finalResponse.PromptCacheKey)
	}
}

func TestResponsesStreamHelperGuardBranches(t *testing.T) {
	var nilObserver *OpenAIResponsesStreamObserver
	nilObserver.ObserveRawLine("event: response.created\n")
	if finalResponse := nilObserver.FinalResponse(); finalResponse != nil {
		t.Fatalf("expected nil observer final response to stay nil, got %+v", finalResponse)
	}

	observer := NewOpenAIResponsesStreamObserver()
	observer.ObserveRawLine("data: {not-json}\n")
	if finalResponse := observer.FinalResponse(); finalResponse != nil {
		t.Fatalf("expected invalid observer payloads to be ignored, got %+v", finalResponse)
	}

	var nilConverter *OpenAIResponsesStreamConverter
	if finalResponse := nilConverter.FinalResponse(); finalResponse != nil {
		t.Fatalf("expected nil converter final response to stay nil, got %+v", finalResponse)
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/", nil)
	converter := NewOpenAIResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{Model: "gpt-5"}, &types.Usage{})
	if finalResponse := converter.FinalResponse(); finalResponse != nil {
		t.Fatalf("expected incomplete converter not to expose a final response, got %+v", finalResponse)
	}

	if !isResponsesTerminalEventType("response.failed") {
		t.Fatal("expected failed responses events to classify as terminal")
	}
	if isResponsesTerminalEventType("response.updated") {
		t.Fatal("expected non-terminal responses events not to classify as terminal")
	}
}

type sseEvent struct {
	Event string
	Data  string
}

func parseSSEEvents(t *testing.T, raw string) []sseEvent {
	t.Helper()

	blocks := strings.Split(strings.TrimSpace(raw), "\n\n")
	events := make([]sseEvent, 0, len(blocks))

	for _, block := range blocks {
		if strings.TrimSpace(block) == "" {
			continue
		}

		var event sseEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				event.Data = strings.TrimPrefix(line, "data: ")
			}
		}

		if event.Event == "" || event.Data == "" {
			t.Fatalf("invalid SSE block: %q", block)
		}

		events = append(events, event)
	}

	return events
}

func mustUnmarshalEvent(t *testing.T, events []sseEvent, eventName string) *types.OpenAIResponsesStreamResponses {
	t.Helper()

	for _, event := range events {
		if event.Event != eventName {
			continue
		}

		var payload types.OpenAIResponsesStreamResponses
		if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
			t.Fatalf("failed to unmarshal %s payload: %v", eventName, err)
		}

		return &payload
	}

	t.Fatalf("event %q not found", eventName)
	return nil
}

func mustUnmarshalAllEvents(t *testing.T, events []sseEvent, eventName string) []*types.OpenAIResponsesStreamResponses {
	t.Helper()

	matches := make([]*types.OpenAIResponsesStreamResponses, 0)
	for _, event := range events {
		if event.Event != eventName {
			continue
		}

		var payload types.OpenAIResponsesStreamResponses
		if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
			t.Fatalf("failed to unmarshal %s payload: %v", eventName, err)
		}

		matches = append(matches, &payload)
	}

	if len(matches) == 0 {
		t.Fatalf("event %q not found", eventName)
	}

	return matches
}
