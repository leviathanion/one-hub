package relay_util

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	commonresponses "one-api/common/responses"
	"strings"
	"testing"

	"one-api/common/requester"
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
		Store:           &store,
		ServiceTier:     "priority",
		ProcessingClass: "flex",
		Text: &types.ResponsesText{
			Verbosity: "low",
		},
	}, &types.Usage{})

	converter.ProcessStreamData(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5.6-terra","service_tier":"flex","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"plan step"},"finish_reason":null}]}`)
	converter.ProcessStreamData(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5.6-terra","service_tier":"flex","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
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
	if completed.Response.Model != "gpt-5.6-terra" || completed.Response.ServiceTier != "flex" || completed.Response.ProcessingClass != "flex" {
		t.Fatalf("expected actual model/tier and request processing class, got model=%q tier=%q class=%q", completed.Response.Model, completed.Response.ServiceTier, completed.Response.ProcessingClass)
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
	if output.Role != "" {
		t.Fatalf("reasoning output must not contain message role, got %q", output.Role)
	}
	outputAdded := mustUnmarshalEvent(t, events, "response.output_item.added")
	if outputAdded.Item == nil || outputAdded.Item.Type != types.InputTypeReasoning || outputAdded.Item.Role != "" {
		t.Fatalf("reasoning output_item.added must not contain message role: %+v", outputAdded.Item)
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
	if converter.item == nil || converter.item.Arguments != nil {
		t.Fatalf("partial function arguments must live only in the bounded builder, item=%+v", converter.item)
	}
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

func TestResponsesStreamConverterPreservesEmptyFunctionArguments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/", nil)
	converter := NewOpenAIResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{Model: "gpt-5"}, &types.Usage{})

	roleOnly := `{"id":"chatcmpl_empty_args","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`
	if err := converter.ProcessStreamData(roleOnly); err != nil {
		t.Fatalf("role-only chunk: %v", err)
	}
	chunk := `{"id":"chatcmpl_empty_args","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_empty","function":{"name":"lookup","arguments":""}}]},"finish_reason":"tool_calls"}]}`
	if err := converter.ProcessStreamData(chunk); err != nil {
		t.Fatalf("empty arguments chunk: %v", err)
	}
	if err := converter.ProcessStreamData("[DONE]"); err != nil {
		t.Fatalf("finalize empty arguments stream: %v", err)
	}

	events := parseSSEEvents(t, recorder.Body.String())
	if strings.Contains(recorder.Body.String(), "response.content_part") || strings.Contains(recorder.Body.String(), `"type":"message"`) {
		t.Fatalf("role-only prefix must not create a spurious message item: %q", recorder.Body.String())
	}
	done := mustUnmarshalEvent(t, events, "response.function_call_arguments.done")
	arguments, ok := done.Arguments.(string)
	if !ok || arguments != "" {
		t.Fatalf("function_call_arguments.done must contain an empty string, got %#v", done.Arguments)
	}
	completed := mustUnmarshalEvent(t, events, "response.completed")
	if completed.Response == nil || len(completed.Response.Output) != 1 || completed.Response.Output[0].Arguments == nil || *completed.Response.Output[0].Arguments != "" {
		t.Fatalf("completed function_call must preserve empty arguments: %+v", completed.Response)
	}
}

func TestResponsesStreamConverterAcceptsExactCumulativePayloadLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/", nil)
	converter := NewOpenAIResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{Model: "gpt-5"}, &types.Usage{})
	first := `{"id":"chatcmpl_limit","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"content":"accepted-prefix"},"finish_reason":null}]}`
	second := `{"id":"chatcmpl_limit","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"content":"-at-limit"},"finish_reason":"stop"}]}`
	converter.maxProcessedBytes = int64(len(first) + len(second))

	if err := converter.ProcessStreamData(first); err != nil {
		t.Fatalf("first payload: %v", err)
	}
	if err := converter.ProcessStreamData(second); err != nil {
		t.Fatalf("payload ending at exact cumulative limit: %v", err)
	}
	if err := converter.ProcessStreamData("[DONE]"); err != nil {
		t.Fatalf("finalize at exact cumulative limit: %v", err)
	}
	final := converter.FinalResponse()
	if final == nil || len(final.Output) != 1 {
		t.Fatalf("unexpected exact-limit final response: %+v", final)
	}
	content, ok := final.Output[0].Content.([]types.ContentResponses)
	if !ok || len(content) != 1 || content[0].Text != "accepted-prefix-at-limit" {
		t.Fatalf("unexpected exact-limit output content: %#v", final.Output[0].Content)
	}
}

func TestResponsesStreamConverterRejectsCumulativePayloadBeforeMutation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/", nil)
	converter := NewOpenAIResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{Model: "gpt-5"}, &types.Usage{})
	first := `{"id":"chatcmpl_limit","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"content":"accepted-prefix"},"finish_reason":null}]}`
	overflow := `{"id":"chatcmpl_limit","object":"chat.completion.chunk","created":1,"model":"model-offending","choices":[{"index":0,"delta":{"content":"offending-delta"},"finish_reason":"stop"}]}`
	converter.maxProcessedBytes = int64(len(first) + len(overflow) - 1)

	if err := converter.ProcessStreamData(first); err != nil {
		t.Fatalf("first payload: %v", err)
	}
	err := converter.ProcessStreamData(overflow)
	if !errors.Is(err, errResponsesStreamConverterStateLimit) {
		t.Fatalf("overflow error=%v, want converter state limit", err)
	}
	if converter.processedBytes != int64(len(first)) {
		t.Fatalf("overflow payload changed cumulative state: %d", converter.processedBytes)
	}
	if converter.responses.Model != "gpt-5" || converter.partTextBuilder.String() != "accepted-prefix" {
		t.Fatalf("overflow payload mutated accepted aggregate: model=%q text=%q", converter.responses.Model, converter.partTextBuilder.String())
	}
	if err := converter.ProcessStreamData("[DONE]"); !errors.Is(err, errResponsesStreamConverterStateLimit) {
		t.Fatalf("terminal converter must preserve state-limit error, got %v", err)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "offending-delta") || strings.Contains(body, "model-offending") || strings.Contains(body, "response.completed") {
		t.Fatalf("overflow payload leaked into output: %q", body)
	}
	if strings.Count(body, "event: error") != 1 || !strings.Contains(body, `"code":"provider_usage_state_limit"`) {
		t.Fatalf("expected one provider state-limit event, got %q", body)
	}
	if final := converter.FinalResponse(); final != nil {
		t.Fatalf("overflowing stream must not expose a successful final response: %+v", final)
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

func TestResponsesStreamConverterMalformedChunkEmitsSingleSequencedTerminalError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/", nil)
	converter := NewOpenAIResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{Model: "gpt-5"}, &types.Usage{})

	if err := converter.ProcessStreamData(`{"id":"chatcmpl_error","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[]}`); err != nil {
		t.Fatalf("expected valid chat chunk, got %v", err)
	}
	if err := converter.ProcessStreamData(`{"id":`); err == nil {
		t.Fatal("expected malformed chat chunk to terminate the converter")
	}
	converter.ProcessStreamData("[DONE]")
	converter.ProcessStreamError()

	events := parseSSEEvents(t, recorder.Body.String())
	if len(events) != 3 {
		t.Fatalf("expected created, in_progress and one error event, got %#v", events)
	}
	if events[2].Event != "error" {
		t.Fatalf("expected final event to be error, got %#v", events[2])
	}
	var streamErr struct {
		Type           string  `json:"type"`
		Code           string  `json:"code"`
		Message        string  `json:"message"`
		Param          *string `json:"param"`
		SequenceNumber int64   `json:"sequence_number"`
	}
	if err := json.Unmarshal([]byte(events[2].Data), &streamErr); err != nil {
		t.Fatalf("expected valid Responses error payload, got %q: %v", events[2].Data, err)
	}
	if streamErr.Type != "error" || streamErr.Code != "invalid_provider_response" || streamErr.Message != "stream interrupted" || streamErr.Param != nil || streamErr.SequenceNumber != 2 {
		t.Fatalf("unexpected Responses error payload: %+v", streamErr)
	}
	if strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatalf("malformed stream must not append response.completed: %q", recorder.Body.String())
	}
	if finalResponse := converter.FinalResponse(); finalResponse != nil {
		t.Fatalf("terminal error must not expose a successful final response, got %+v", finalResponse)
	}
}

func TestResponsesStreamConverterRejectsUnsupportedChoiceShapesWithoutPartialOutput(t *testing.T) {
	tests := []struct {
		name  string
		chunk string
	}{
		{
			name:  "null tool call",
			chunk: `{"id":"chatcmpl_bad","choices":[{"index":0,"delta":{"tool_calls":[null]}}]}`,
		},
		{
			name:  "missing function",
			chunk: `{"id":"chatcmpl_bad","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_1","function":null}]}}]}`,
		},
		{
			name:  "multiple tool calls",
			chunk: `{"id":"chatcmpl_bad","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"a","arguments":""}},{"index":1,"function":{"name":"b","arguments":""}}]}}]}`,
		},
		{
			name:  "unsupported image delta",
			chunk: `{"id":"chatcmpl_bad","choices":[{"index":0,"delta":{"images":[{"type":"image_url","image_url":{"url":"https://example.test/image.png"}}]}}]}`,
		},
		{
			name:  "mixed content and refusal",
			chunk: `{"id":"chatcmpl_bad","choices":[{"index":0,"delta":{"content":"answer","refusal":"refuse"}}]}`,
		},
		{
			name:  "mixed reasoning and content",
			chunk: `{"id":"chatcmpl_bad","choices":[{"index":0,"delta":{"reasoning_content":"plan","content":"answer"}}]}`,
		},
		{
			name:  "unsupported choice index",
			chunk: `{"id":"chatcmpl_bad","choices":[{"index":1,"delta":{"content":"alternative"}}]}`,
		},
		{
			name:  "tool call missing identity",
			chunk: `{"id":"chatcmpl_bad","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest("GET", "/", nil)
			converter := NewOpenAIResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{Model: "gpt-5"}, &types.Usage{})

			if err := converter.ProcessStreamData(test.chunk); err == nil {
				t.Fatal("unsupported provider delta must fail")
			}
			if err := converter.ProcessStreamData("[DONE]"); err == nil {
				t.Fatal("terminal converter must preserve the provider-shape error")
			}
			body := recorder.Body.String()
			if strings.Count(body, "event: error") != 1 || strings.Contains(body, "response.created") || strings.Contains(body, "response.completed") {
				t.Fatalf("unsupported chunk produced partial success output: %q", body)
			}
			if converter.FinalResponse() != nil {
				t.Fatal("unsupported chunk must not expose a successful final response")
			}
		})
	}
}

func TestResponsesStreamConverterRejectsToolCallIndexAndIdentityChangesBeforeClosingItem(t *testing.T) {
	tests := []struct {
		name      string
		offending string
	}{
		{
			name:      "different index",
			offending: `{"id":"chatcmpl_tools","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_1","function":{"name":"lookup","arguments":"2}"}}]}}]}`,
		},
		{
			name:      "different call id",
			offending: `{"id":"chatcmpl_tools","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_changed","function":{"arguments":"2}"}}]}}]}`,
		},
		{
			name:      "different function name",
			offending: `{"id":"chatcmpl_tools","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"changed","arguments":"2}"}}]}}]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest("GET", "/", nil)
			converter := NewOpenAIResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{Model: "gpt-5"}, &types.Usage{})
			prefix := `{"id":"chatcmpl_tools","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_0","function":{"name":"lookup","arguments":"{\"a\":"}}]}}]}`

			if err := converter.ProcessStreamData(prefix); err != nil {
				t.Fatalf("accepted prefix: %v", err)
			}
			if err := converter.ProcessStreamData(test.offending); err == nil {
				t.Fatal("tool identity/index change must fail")
			}
			body := recorder.Body.String()
			if strings.Count(body, "event: error") != 1 || strings.Contains(body, "response.function_call_arguments.done") || strings.Contains(body, "response.output_item.done") || strings.Contains(body, "response.completed") {
				t.Fatalf("offending tool chunk closed or completed a forged item: %q", body)
			}
			if converter.argsBuilder.String() != `{"a":` || converter.item == nil || converter.item.CallID != "call_0" || converter.item.Name != "lookup" {
				t.Fatalf("offending tool chunk mutated accepted prefix: args=%q item=%+v", converter.argsBuilder.String(), converter.item)
			}
		})
	}
}

func TestResponsesStreamFailureCodePreservesStateLimits(t *testing.T) {
	if got := ResponsesStreamFailureCode(errResponsesStreamConverterStateLimit); got != "provider_usage_state_limit" {
		t.Fatalf("state limit code=%q", got)
	}
	if got := ResponsesStreamFailureCode(requester.ErrSSEEventTooLarge); got != "provider_usage_state_limit" {
		t.Fatalf("observer state limit code=%q", got)
	}
	for _, code := range []string{"provider_usage_state_limit", "provider_protocol_error"} {
		err := &types.OpenAIErrorWithStatusCode{OpenAIError: types.OpenAIError{Code: code}}
		if got := ResponsesStreamFailureCode(err); got != code {
			t.Fatalf("typed provider tracking code=%q, want %q", got, code)
		}
	}
	if got := ResponsesStreamFailureCode(errors.New("malformed")); got != "invalid_provider_response" {
		t.Fatalf("generic converter error code=%q", got)
	}
}

func TestResponsesStreamObserverTracksTerminalResponse(t *testing.T) {
	observer := commonresponses.NewStreamObserver()

	observer.ObserveRawEvent("event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_created\",\"object\":\"response\",\"prompt_cache_key\":\"pc-created\",\"status\":\"in_progress\"}}\n\n")
	observer.ObserveRawEvent("event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_created\",\"object\":\"response\",\"prompt_cache_key\":\"pc-final\",\"status\":\"completed\"}}\n\n")

	finalResponse := observer.FinalResponse()
	if finalResponse == nil {
		t.Fatal("expected observer to keep terminal response")
	}
	if finalResponse.ID != "resp_created" {
		t.Fatalf("expected terminal response id %q, got %q", "resp_created", finalResponse.ID)
	}
	if finalResponse.PromptCacheKey != "pc-final" {
		t.Fatalf("expected terminal prompt_cache_key %q, got %q", "pc-final", finalResponse.PromptCacheKey)
	}
	if err := observer.LifecycleError(); err != nil {
		t.Fatalf("expected valid terminal lifecycle, got %v", err)
	}
}

func TestResponsesStreamObserverAcceptsMultiLineAndNoSpaceData(t *testing.T) {
	observer := commonresponses.NewStreamObserver()
	observer.ObserveRawEvent("event: response.completed\ndata:{\"type\":\"response.completed\",\ndata:\"sequence_number\":0,\"response\":{\"id\":\"resp_multiline\",\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n\n")

	if err := observer.LifecycleError(); err != nil {
		t.Fatalf("expected legal multi-line SSE event, got %v", err)
	}
	final := observer.FinalResponse()
	if final == nil || final.ID != "resp_multiline" || final.Usage == nil || final.Usage.TotalTokens != 5 {
		t.Fatalf("expected terminal usage from multi-line SSE event, got %+v", final)
	}
}

func TestResponsesSSEDataPayloadPreservesMultiLineAndColonlessFields(t *testing.T) {
	payload, ok := commonresponses.SSEDataPayload("event: response.completed\r\ndata:{\"type\":\"response.completed\",\r\ndata\r\ndata:\"response\":{\"id\":\"resp_1\"}}\r\n\r\n")
	if !ok || payload != "{\"type\":\"response.completed\",\n\n\"response\":{\"id\":\"resp_1\"}}" {
		t.Fatalf("unexpected SSE data payload: ok=%v payload=%q", ok, payload)
	}
	if payload, ok := commonresponses.SSEDataPayload("event\n: comment\n\n"); ok || payload != "" {
		t.Fatalf("event without data must not produce a payload: ok=%v payload=%q", ok, payload)
	}
}

func TestResponsesStreamObserverFreezesFirstTerminal(t *testing.T) {
	tests := []struct {
		name  string
		first string
		late  string
		kind  commonresponses.StreamTerminalKind
		id    string
	}{
		{
			name:  "response before conflicting response",
			first: "data: {\"type\":\"response.completed\",\"sequence_number\":0,\"response\":{\"id\":\"resp_first\",\"status\":\"completed\"}}\n\n",
			late:  "data: {\"type\":\"response.failed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_second\",\"status\":\"failed\"}}\n\n",
			kind:  commonresponses.StreamTerminalResponse,
			id:    "resp_first",
		},
		{
			name:  "response before error",
			first: "data: {\"type\":\"response.completed\",\"sequence_number\":0,\"response\":{\"id\":\"resp_first\",\"status\":\"completed\"}}\n\n",
			late:  "data: {\"type\":\"error\",\"code\":\"late\"}\n\n",
			kind:  commonresponses.StreamTerminalResponse,
			id:    "resp_first",
		},
		{
			name:  "error before response",
			first: "data: {\"type\":\"error\",\"code\":\"first\"}\n\n",
			late:  "data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_late\",\"status\":\"completed\"}}\n\n",
			kind:  commonresponses.StreamTerminalResponse,
			id:    "resp_late",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observer := commonresponses.NewStreamObserver()
			observer.ObserveRawEvent(test.first)
			observer.ObserveRawEvent(test.late)
			if observer.TerminalKind() != test.kind {
				t.Fatalf("terminal kind=%v, want %v", observer.TerminalKind(), test.kind)
			}
			final := observer.FinalResponse()
			if test.id == "" {
				if final != nil {
					t.Fatalf("later response overwrote first error terminal: %+v", final)
				}
			} else if final == nil || final.ID != test.id {
				t.Fatalf("first response terminal was not frozen: %+v", final)
			}
		})
	}
}

func TestResponsesStreamObserverRejectsConflictingIdentity(t *testing.T) {
	observer := commonresponses.NewStreamObserver()
	observer.ObserveRawEvent("data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_a\"}}\n\n")
	observer.ObserveRawEvent("data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_b\",\"status\":\"completed\"}}\n\n")

	if observer.LifecycleError() == nil || observer.TerminalSeen() {
		t.Fatalf("conflicting identity must fail before terminal commit: err=%v terminal=%v", observer.LifecycleError(), observer.TerminalSeen())
	}
	if final := observer.FinalResponse(); final == nil || final.ID != "resp_a" {
		t.Fatalf("conflicting terminal mutated accepted response facts: %+v", final)
	}
}

func TestResponsesStreamObserverDoesNotIncrementMaxSequence(t *testing.T) {
	observer := commonresponses.NewStreamObserver()
	observer.ObserveRawEvent("data: {\"type\":\"response.created\",\"sequence_number\":9223372036854775807,\"response\":{\"id\":\"resp_max_sequence\"}}\n\n")
}

func TestResponsesStreamObserverAcceptsFutureOutputQualityUnionOnTerminal(t *testing.T) {
	observer := commonresponses.NewStreamObserver()
	observer.ObserveRawEvent(`data: {"type":"response.completed","sequence_number":0,"response":{"id":"resp_future_quality","status":"completed","output":[{"type":"image_generation_call","id":"img_future","status":"completed","quality":{"future":"quality"}}]}}` + "\n\n")

	if err := observer.LifecycleError(); err != nil {
		t.Fatalf("future output quality union must not invalidate terminal lifecycle: %v", err)
	}
	final := observer.FinalResponse()
	if final == nil || len(final.Output) != 1 || final.Output[0].ID != "img_future" || final.Output[0].Quality != "" {
		t.Fatalf("expected stable terminal evidence with ignored local quality union, got %+v", final)
	}
}

func TestResponsesStreamObserverAcceptsNormalizedCodexEvents(t *testing.T) {
	observer := commonresponses.NewStreamObserver()
	observer.ObserveRawEvent("event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_unterminated\",\"status\":\"in_progress\"}}\n\n")
	observer.ObserveRawEvent("event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_unterminated\",\"status\":\"completed\"}}\n\n")

	if err := observer.LifecycleError(); err != nil {
		t.Fatalf("expected Codex-style unterminated event boundary to stay valid, got %v", err)
	}
	if final := observer.FinalResponse(); final == nil || final.ID != "resp_unterminated" || final.Status != "completed" {
		t.Fatalf("expected terminal event after unterminated predecessor, got %+v", final)
	}
}

func TestResponsesStreamObserverAcceptsRefusalContentPart(t *testing.T) {
	observer := commonresponses.NewStreamObserver()
	observer.ObserveRawEvent(`data: {"type":"response.content_part.added","sequence_number":0,"output_index":0,"content_index":0,"item_id":"msg_1","part":{"type":"refusal","refusal":"cannot comply"}}` + "\n\n")
	observer.ObserveRawEvent(`data: {"type":"response.completed","sequence_number":1,"response":{"id":"resp_1","status":"completed"}}` + "\n\n")

	if err := observer.LifecycleError(); err != nil {
		t.Fatalf("expected flat refusal content-part to be a valid provider event: %v", err)
	}
}

func TestResponsesStreamObserverOnlyRejectsResourceConflicts(t *testing.T) {
	tests := []struct {
		name      string
		events    []string
		wantError bool
	}{
		{
			name:   "missing terminal",
			events: []string{`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1"}}`},
		},
		{
			name:   "realtime terminal",
			events: []string{`data: {"type":"response.done","sequence_number":0,"response":{"id":"resp_1"}}`},
		},
		{
			name:   "malformed data",
			events: []string{`data: {not-json}`},
		},
		{
			name:      "terminal missing response id",
			wantError: true,
			events:    []string{`data: {"type":"response.completed","sequence_number":0,"response":{"status":"completed"}}`},
		},
		{
			name:      "response id changes",
			wantError: true,
			events: []string{
				`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_a"}}`,
				`data: {"type":"response.completed","sequence_number":1,"response":{"id":"resp_b","status":"completed"}}`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observer := commonresponses.NewStreamObserver()
			for _, event := range tt.events {
				observer.ObserveRawEvent(event + "\n\n")
			}
			if err := observer.LifecycleError(); (err != nil) != tt.wantError {
				t.Fatal("expected invalid lifecycle to fail")
			}
		})
	}
}

func TestResponsesStreamObserverTreatsProviderSequenceAndTailAsBestEffort(t *testing.T) {
	observer := commonresponses.NewStreamObserver()
	for _, event := range []string{
		`data: {"type":"response.created","sequence_number":1,"response":{"id":"resp_best_effort"}}`,
		`data: {"type":"response.output_text.delta","sequence_number":null,"delta":"ok"}`,
		`data: {"type":"response.completed","response":{"id":"resp_best_effort","status":"completed"}}`,
		`data: {"type":"response.output_text.done","sequence_number":2,"text":"late"}`,
	} {
		observer.ObserveRawEvent(event + "\n\n")
	}
	if err := observer.LifecycleError(); err != nil {
		t.Fatalf("provider-owned sequence and trailing-event details must not invalidate exact-wire delivery: %v", err)
	}
}

func TestResponsesStreamObserverAcceptsIncompleteTopLevelErrorEnvelope(t *testing.T) {
	observer := commonresponses.NewStreamObserver()
	observer.ObserveRawEvent("data: {\"type\":\"error\",\"future_detail\":true}\n\n")
	if err := observer.LifecycleError(); err != nil {
		t.Fatalf("top-level provider error must remain terminal without local field validation: %v", err)
	}
	if observer.TerminalKind() != commonresponses.StreamTerminalError {
		t.Fatalf("expected top-level error terminal, got %v", observer.TerminalKind())
	}
}

func TestResponsesStreamConverterDoesNotEchoUnconfirmedRequestTier(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	converter := NewOpenAIResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{
		Model:       "gpt-5",
		ServiceTier: "priority",
	}, &types.Usage{})
	if err := converter.ProcessStreamData(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`); err != nil {
		t.Fatal(err)
	}
	if err := converter.ProcessStreamData("[DONE]"); err != nil {
		t.Fatal(err)
	}
	completed := mustUnmarshalEvent(t, parseSSEEvents(t, recorder.Body.String()), "response.completed")
	if completed.Response == nil || completed.Response.ServiceTier != "" {
		t.Fatalf("request tier must not be emitted as provider evidence: %+v", completed.Response)
	}
}

func TestResponsesStreamConverterPreservesChatRefusalDelta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	converter := NewOpenAIResponsesStreamConverter(ctx, &types.OpenAIResponsesRequest{Model: "gpt-5"}, &types.Usage{})
	if err := converter.ProcessStreamData(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{"refusal":"cannot comply"}}]}`); err != nil {
		t.Fatalf("convert refusal delta: %v", err)
	}
	if err := converter.ProcessStreamData(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"gpt-5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`); err != nil {
		t.Fatalf("convert refusal terminal: %v", err)
	}
	if err := converter.ProcessStreamData("[DONE]"); err != nil {
		t.Fatalf("complete refusal stream: %v", err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"response.refusal.delta"`) || !strings.Contains(body, `"delta":"cannot comply"`) || !strings.Contains(body, `"type":"response.refusal.done"`) || !strings.Contains(body, `"refusal":"cannot comply"`) {
		t.Fatalf("Chat refusal was lost during Responses conversion: %q", body)
	}
}

func TestResponsesStreamObserverAcceptsTopLevelErrorTerminal(t *testing.T) {
	observer := commonresponses.NewStreamObserver()
	observer.ObserveRawEvent(`data: {"type":"error","sequence_number":7,"code":"server_error","message":"failed","param":"model"}` + "\n\n")
	if err := observer.LifecycleError(); err != nil {
		t.Fatalf("expected valid error terminal, got %v", err)
	}
	streamError := observer.TerminalError()
	if streamError == nil || streamError.Code != "server_error" || streamError.Message != "failed" || streamError.Param == nil || *streamError.Param != "model" {
		t.Fatalf("expected provider terminal error evidence, got %+v", streamError)
	}
}

func TestResponsesStreamHelperGuardBranches(t *testing.T) {
	var nilObserver *commonresponses.StreamObserver
	nilObserver.ObserveRawEvent("event: response.created\n\n")
	if finalResponse := nilObserver.FinalResponse(); finalResponse != nil {
		t.Fatalf("expected nil observer final response to stay nil, got %+v", finalResponse)
	}

	observer := commonresponses.NewStreamObserver()
	observer.ObserveRawEvent("data: {not-json}\n\n")
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

	if !commonresponses.IsTerminalEventType("response.failed") {
		t.Fatal("expected failed responses events to classify as terminal")
	}
	if commonresponses.IsTerminalEventType("response.done") {
		t.Fatal("expected Realtime response.done not to classify as a Responses terminal")
	}
	if commonresponses.IsTerminalEventType("response.updated") {
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

func TestResponsesStreamObserverKeepsTerminalIndependentFromUsageEvidence(t *testing.T) {
	observer := commonresponses.NewStreamObserver()
	raw := "data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":\"future-shape\"}}\n\n"
	observer.ObserveRawEvent(raw)

	if err := observer.LifecycleError(); err != nil {
		t.Fatalf("valid terminal was rejected because usage was malformed: %v", err)
	}
	if !observer.TerminalSeen() || observer.TerminalKind() != commonresponses.StreamTerminalResponse {
		t.Fatalf("terminal facts missing: kind=%v", observer.TerminalKind())
	}
	final := observer.FinalResponse()
	if final == nil || final.ID != "resp_1" || final.Status != "completed" || final.Usage != nil {
		t.Fatalf("unexpected stable response facts: %+v", final)
	}
}
