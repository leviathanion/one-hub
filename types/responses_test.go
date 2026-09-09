package types

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestSummaryResponsesListUnmarshalSupportsArrayAndObject(t *testing.T) {
	testCases := []struct {
		name string
		data string
	}{
		{
			name: "array",
			data: `{"type":"reasoning","summary":[{"type":"summary_text","text":"alpha"}]}`,
		},
		{
			name: "object",
			data: `{"type":"reasoning","summary":{"type":"summary_text","text":"beta"}}`,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var input InputResponses
			if err := json.Unmarshal([]byte(testCase.data), &input); err != nil {
				t.Fatalf("unexpected unmarshal error: %v", err)
			}

			if len(input.Summary) != 1 {
				t.Fatalf("expected one summary item, got %d", len(input.Summary))
			}

			if input.Summary[0].Type != ContentTypeSummaryText {
				t.Fatalf("expected summary type %q, got %q", ContentTypeSummaryText, input.Summary[0].Type)
			}
		})
	}
}

func TestResponsesToChatAllowsTerminalWithoutUsage(t *testing.T) {
	response := &OpenAIResponsesResponses{
		ID:     "resp_failed",
		Model:  "gpt-5",
		Status: ResponseStatusFailed,
	}
	chat := response.ToChat()
	if chat == nil || chat.Usage != nil || len(chat.Choices) != 1 {
		t.Fatalf("expected a Chat response without fabricated usage, got %+v", chat)
	}
}

func TestResponsesRefusalUsesFlatWireStringAndConvertsBothWays(t *testing.T) {
	var event OpenAIResponsesStreamResponses
	if err := json.Unmarshal([]byte(`{"type":"response.content_part.added","sequence_number":1,"output_index":0,"content_index":0,"item_id":"msg_1","part":{"type":"refusal","refusal":"cannot comply"}}`), &event); err != nil {
		t.Fatalf("decode refusal content-part event: %v", err)
	}
	if event.Part == nil || event.Part.Type != ContentTypeRefusal || event.Part.Refusal != "cannot comply" {
		t.Fatalf("unexpected refusal content-part: %+v", event.Part)
	}

	chatResponse := &ChatCompletionResponse{
		ID: "chatcmpl_1", Model: "gpt-5", Usage: &Usage{},
		Choices: []ChatCompletionChoice{{
			FinishReason: FinishReasonStop,
			Message: ChatCompletionMessage{
				Role:    ChatMessageRoleAssistant,
				Refusal: "cannot comply",
			},
		}},
	}
	responses := chatResponse.ToResponses(&OpenAIResponsesRequest{Model: "gpt-5"})
	encoded, err := json.Marshal(responses)
	if err != nil {
		t.Fatalf("encode converted Responses refusal: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"refusal":"cannot comply"`)) || !bytes.Contains(encoded, []byte(`"type":"refusal"`)) {
		t.Fatalf("Chat to Responses emitted a non-flat refusal: %s", encoded)
	}
	if bytes.Contains(encoded, []byte(`"refusal":{"`)) {
		t.Fatalf("Chat to Responses emitted the old nested refusal shape: %s", encoded)
	}

	var providerResponse OpenAIResponsesResponses
	if err := json.Unmarshal([]byte(`{"id":"resp_1","model":"gpt-5","status":"completed","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"refusal","refusal":"cannot comply"}]}]}`), &providerResponse); err != nil {
		t.Fatalf("decode provider refusal response: %v", err)
	}
	chat := providerResponse.ToChat()
	if len(chat.Choices) != 1 || chat.Choices[0].Message.Refusal != "cannot comply" {
		t.Fatalf("Responses refusal was not mapped to Chat: %+v", chat.Choices)
	}
}

func TestInputResponsesMarshalKeepsEmptySummaryForReasoning(t *testing.T) {
	input := InputResponses{
		Type:    InputTypeReasoning,
		ID:      "rs_1",
		Status:  ResponseStatusCompleted,
		Summary: SummaryResponsesList{},
	}

	data, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unexpected payload unmarshal error: %v", err)
	}

	summary, ok := payload["summary"].([]any)
	if !ok {
		t.Fatalf("expected summary array to be preserved, got %#v", payload["summary"])
	}
	if len(summary) != 0 {
		t.Fatalf("expected empty summary array, got %#v", summary)
	}
}

func TestResponsesOutputMarshalKeepsEmptySummaryForReasoning(t *testing.T) {
	output := ResponsesOutput{
		Type:    InputTypeReasoning,
		ID:      "rs_1",
		Status:  ResponseStatusCompleted,
		Summary: SummaryResponsesList{},
	}

	data, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unexpected payload unmarshal error: %v", err)
	}

	summary, ok := payload["summary"].([]any)
	if !ok {
		t.Fatalf("expected summary array to be preserved, got %#v", payload["summary"])
	}
	if len(summary) != 0 {
		t.Fatalf("expected empty summary array, got %#v", summary)
	}
}

func TestResponsesResponseMarshalPreservesExplicitEmptyKnownFields(t *testing.T) {
	raw := []byte(`{"id":"resp_1","model":"gpt-5","object":"response","status":"completed","output":[],"error":null,"previous_response_id":null,"metadata":{},"parallel_tool_calls":false,"max_output_tokens":0}`)
	var response OpenAIResponsesResponses
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode marshaled response: %v", err)
	}
	for field, want := range map[string]string{
		"output": "[]", "error": "null", "previous_response_id": "null",
		"metadata": "{}", "parallel_tool_calls": "false", "max_output_tokens": "0",
	} {
		if got := string(fields[field]); got != want {
			t.Fatalf("explicit empty field %s changed: got %q want %q; body=%s", field, got, want, encoded)
		}
	}
}

func TestResponsesResponseMarshalPreservesMissingModelAndStatus(t *testing.T) {
	raw := []byte(`{"id":"resp_compact","object":"response.compaction","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	var response OpenAIResponsesResponses
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("unmarshal compact response: %v", err)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal compact response: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode marshaled compact response: %v", err)
	}
	for _, field := range []string{"model", "status"} {
		if value, ok := fields[field]; ok {
			t.Fatalf("missing provider field %q was injected as %s: %s", field, value, encoded)
		}
	}
	if string(fields["object"]) != `"response.compaction"` || string(fields["output"]) != "[]" {
		t.Fatalf("compact response fields changed: %s", encoded)
	}
}

func TestResponsesResponseMarshalPreservesExplicitEmptyModelAndStatus(t *testing.T) {
	raw := []byte(`{"id":"resp_explicit_empty","model":"","status":""}`)
	var response OpenAIResponsesResponses
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("unmarshal explicit empty response: %v", err)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal explicit empty response: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode explicit empty response: %v", err)
	}
	if string(fields["model"]) != `""` || string(fields["status"]) != `""` {
		t.Fatalf("explicit empty model/status were not preserved: %s", encoded)
	}
}

func TestDecodeCapturedProviderJSONAllowsFutureOutputUnion(t *testing.T) {
	raw := []byte(`{"id":"resp_future","model":"gpt-5","object":"response","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5},"output":[{"type":"future_output","quality":{"future":true}}]}`)
	var response OpenAIResponsesResponses
	if err := response.DecodeCapturedProviderJSON(raw); err != nil {
		t.Fatalf("decode captured provider JSON: %v", err)
	}
	if response.ID != "resp_future" || response.Model != "gpt-5" || response.Usage == nil || response.Usage.TotalTokens != 5 {
		t.Fatalf("expected stable evidence from future response, got %+v", response)
	}
	response.SetProviderRawJSON(raw)
	response.EnableProviderRawJSONReplay()
	if got := response.ReplayProviderRawJSON(); string(got) != string(raw) {
		t.Fatalf("expected raw future response replay, got %s", got)
	}
}

func TestDecodeCapturedProviderJSONKeepsRawDeliveryWhenUsageShapeIsUnknown(t *testing.T) {
	raw := []byte(`{"id":"resp_future_usage","model":"gpt-5","object":"response","status":"completed","usage":"future-shape","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[]}]}`)
	var response OpenAIResponsesResponses
	if err := response.DecodeCapturedProviderJSON(raw); err != nil {
		t.Fatalf("decode stable fields independently: %v", err)
	}
	if response.ID != "resp_future_usage" || response.Status != "completed" || response.Usage != nil || len(response.Output) != 1 {
		t.Fatalf("unexpected independent evidence: %+v", response)
	}
	response.SetProviderRawJSON(raw)
	response.EnableProviderRawJSONReplay()
	if got := response.ReplayProviderRawJSON(); string(got) != string(raw) {
		t.Fatalf("raw response changed: %s", got)
	}
}

func TestResponsesOutputMarshalPreservesExplicitNullKnownField(t *testing.T) {
	var output ResponsesOutput
	if err := json.Unmarshal([]byte(`{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[],"error":null}`), &output); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if string(fields["content"]) != "[]" || string(fields["error"]) != "null" {
		t.Fatalf("explicit output empties were lost: %s", encoded)
	}
}

func TestResponsesOutputQualityFutureUnionPreservesRawField(t *testing.T) {
	raw := []byte(`{"type":"image_generation_call","id":"img_future","status":"completed","quality":{"future":"quality"},"size":"1024x1024"}`)
	var output ResponsesOutput
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatalf("unmarshal future quality union: %v", err)
	}
	if output.Quality != "" {
		t.Fatalf("future quality union must not be treated as a local string, got %q", output.Quality)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("marshal future quality union: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode future quality output: %v", err)
	}
	if got := string(fields["quality"]); got != `{"future":"quality"}` {
		t.Fatalf("future quality union was not preserved: %s", encoded)
	}
}

func TestResponsesResponseRoundTripPreservesNestedUnknownAndNullFields(t *testing.T) {
	raw := []byte(`{"id":"resp_nested","model":"gpt-5","object":"response","status":"completed","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","refusal":null,"future_nested":{"keep":true}}]}]}`)
	var response OpenAIResponsesResponses
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("unmarshal nested response: %v", err)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal nested response: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode response fields: %v", err)
	}
	var output []map[string]json.RawMessage
	if err := json.Unmarshal(fields["output"], &output); err != nil || len(output) != 1 {
		t.Fatalf("decode output: %v body=%s", err, encoded)
	}
	var content []map[string]json.RawMessage
	if err := json.Unmarshal(output[0]["content"], &content); err != nil || len(content) != 1 {
		t.Fatalf("decode content: %v body=%s", err, encoded)
	}
	if string(content[0]["refusal"]) != "null" || string(content[0]["future_nested"]) != `{"keep":true}` {
		t.Fatalf("nested fields changed during same-dialect round trip: %s", encoded)
	}
}

func TestResponsesResponseRoundTripPreservesFutureFieldsInsideKnownContainers(t *testing.T) {
	raw := []byte(`{
		"id":"resp_future_nested",
		"model":"gpt-5.6",
		"object":"response",
		"status":"completed",
		"usage":{
			"input_tokens":2,
			"output_tokens":3,
			"total_tokens":5,
			"future_usage":{"large_integer":12345678901234567890},
			"output_tokens_details":{"reasoning_tokens":1,"future_detail":{"keep":true}}
		},
		"output":[{
			"type":"message",
			"id":"msg_1",
			"status":"completed",
			"role":"assistant",
			"content":[{
				"type":"output_text",
				"text":"ok",
				"logprobs":{"future_number":12345678901234567890},
				"annotations":[{"type":"future_citation","future":{"keep":true}}]
			}]
		}]
	}`)
	var response OpenAIResponsesResponses
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}

	var original, roundTripped any
	for name, input := range map[string][]byte{"original": raw, "round-tripped": encoded} {
		decoder := json.NewDecoder(bytes.NewReader(input))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			t.Fatalf("decode %s response: %v", name, err)
		}
		if name == "original" {
			original = decoded
		} else {
			roundTripped = decoded
		}
	}
	if !reflect.DeepEqual(roundTripped, original) {
		t.Fatalf("future fields inside known response containers changed:\noriginal=%s\nround-tripped=%s", raw, encoded)
	}
}

func TestResponsesResponseMarshalLetsChangedKnownFieldOverrideOriginal(t *testing.T) {
	var response OpenAIResponsesResponses
	if err := json.Unmarshal([]byte(`{"id":"resp_original","model":"gpt-5","object":"response","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3,"future":{"keep":true}}}`), &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	response.Usage.TotalTokens = 9
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(fields["usage"], &usage); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	if string(usage["total_tokens"]) != "9" {
		t.Fatalf("changed known usage must override original provider field: %s", encoded)
	}
	if _, retained := usage["future"]; retained {
		t.Fatalf("a modified known container must not replay stale extension state: %s", encoded)
	}
}

func TestResponsesResponseRoundTripPreservesUnknownReasoningFields(t *testing.T) {
	raw := []byte(`{"id":"resp_reasoning","model":"gpt-5.6","object":"response","status":"completed","output":[],"reasoning":{"effort":"medium","mode":"pro","context":"all_turns","future":{"keep":true}}}`)
	var response OpenAIResponsesResponses
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if response.Reasoning == nil || response.Reasoning.Effort == nil || *response.Reasoning.Effort != "medium" {
		t.Fatalf("known reasoning fields were not decoded: %+v", response.Reasoning)
	}

	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var reasoning map[string]json.RawMessage
	if err := json.Unmarshal(fields["reasoning"], &reasoning); err != nil {
		t.Fatalf("decode reasoning: %v body=%s", err, encoded)
	}
	for field, want := range map[string]string{
		"effort": `"medium"`, "mode": `"pro"`, "context": `"all_turns"`, "future": `{"keep":true}`,
	} {
		if got := string(reasoning[field]); got != want {
			t.Fatalf("reasoning field %s changed: got %q want %q; body=%s", field, got, want, encoded)
		}
	}
}

func TestReasoningEffortMarshalLetsKnownFieldsOverrideRawAndPreservesExplicitNull(t *testing.T) {
	var reasoning ReasoningEffort
	if err := json.Unmarshal([]byte(`{"effort":null,"mode":"pro"}`), &reasoning); err != nil {
		t.Fatalf("unmarshal reasoning: %v", err)
	}
	encoded, err := json.Marshal(reasoning)
	if err != nil {
		t.Fatalf("marshal reasoning: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode reasoning: %v", err)
	}
	if got := string(fields["effort"]); got != "null" {
		t.Fatalf("explicit null effort was not preserved: %s", encoded)
	}

	high := "high"
	reasoning.Effort = &high
	encoded, err = json.Marshal(reasoning)
	if err != nil {
		t.Fatalf("marshal updated reasoning: %v", err)
	}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode updated reasoning: %v", err)
	}
	if got := string(fields["effort"]); got != `"high"` || string(fields["mode"]) != `"pro"` {
		t.Fatalf("known update or unknown field preservation failed: %s", encoded)
	}
}

func TestResponsesOutputStringContentSupportsTypedSlices(t *testing.T) {
	output := ResponsesOutput{
		Type: InputTypeMessage,
		Content: []ContentResponses{
			{
				Type: ContentTypeOutputText,
				Text: "hello",
			},
			{
				Type: ContentTypeOutputText,
				Text: " world",
			},
		},
	}

	if got := output.StringContent(); got != "hello world" {
		t.Fatalf("expected concatenated content, got %q", got)
	}
}

func TestResponsesInputFunctionArgumentsAcceptJSONValues(t *testing.T) {
	data := []byte(`{
		"model":"gpt-5",
		"input":[
			{"type":"function_call","call_id":"call_object","name":"lookup","arguments":{"city":"Paris","days":0,"strict":false}},
			{"type":"function_call","call_id":"call_array","name":"batch","arguments":[{"id":1}]},
			{"type":"function_call","call_id":"call_null","name":"empty","arguments":null}
		]
	}`)

	var request OpenAIResponsesRequest
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}

	chat, err := request.ToChatCompletionRequest()
	if err != nil {
		t.Fatalf("unexpected conversion error: %v", err)
	}

	if len(chat.Messages) != 3 {
		t.Fatalf("expected three converted tool call messages, got %d", len(chat.Messages))
	}
	testCases := []struct {
		index int
		want  string
	}{
		{index: 0, want: `{"city":"Paris","days":0,"strict":false}`},
		{index: 1, want: `[{"id":1}]`},
		{index: 2, want: ""},
	}
	for _, testCase := range testCases {
		message := chat.Messages[testCase.index]
		if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function == nil {
			t.Fatalf("expected tool call at message %d, got %#v", testCase.index, message.ToolCalls)
		}
		if got := message.ToolCalls[0].Function.Arguments; got != testCase.want {
			t.Fatalf("expected arguments %q at message %d, got %q", testCase.want, testCase.index, got)
		}
	}
}

func TestResponsesOutputFunctionArgumentsAcceptJSONValues(t *testing.T) {
	var response OpenAIResponsesResponses
	data := []byte(`{
		"id":"resp_1",
		"model":"gpt-5",
		"service_tier":"flex",
		"status":"completed",
		"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0},
		"output":[
			{"type":"function_call","id":"fc_object","status":"completed","call_id":"call_object","name":"lookup","arguments":{"city":"Paris","days":0,"strict":false}},
			{"type":"function_call","id":"fc_string","status":"completed","call_id":"call_string","name":"weather","arguments":"{\"city\":\"Berlin\"}"},
			{"type":"function_call","id":"fc_missing","status":"completed","call_id":"call_missing","name":"missing"}
		]
	}`)

	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}

	if response.Output[0].Arguments == nil {
		t.Fatal("expected object arguments to be present")
	}
	if got := *response.Output[0].Arguments; got != `{"city":"Paris","days":0,"strict":false}` {
		t.Fatalf("expected object arguments to be normalized, got %q", got)
	}
	if response.Output[1].Arguments == nil {
		t.Fatal("expected string arguments to be present")
	}
	if got := *response.Output[1].Arguments; got != `{"city":"Berlin"}` {
		t.Fatalf("expected string arguments to be decoded once, got %q", got)
	}
	if response.Output[2].Arguments != nil {
		t.Fatalf("expected missing arguments to stay nil, got %q", *response.Output[2].Arguments)
	}

	chat := response.ToChat()
	if chat.ServiceTier != "flex" {
		t.Fatalf("expected actual service tier in chat conversion, got %q", chat.ServiceTier)
	}
	if len(chat.Choices) != 1 || len(chat.Choices[0].Message.ToolCalls) != 3 {
		t.Fatalf("expected three chat tool calls, got %#v", chat.Choices)
	}
	if got := chat.Choices[0].Message.ToolCalls[0].Function.Arguments; got != `{"city":"Paris","days":0,"strict":false}` {
		t.Fatalf("expected object arguments in chat conversion, got %q", got)
	}
	if got := chat.Choices[0].Message.ToolCalls[1].Function.Arguments; got != `{"city":"Berlin"}` {
		t.Fatalf("expected string arguments in chat conversion, got %q", got)
	}
	if got := chat.Choices[0].Message.ToolCalls[2].Function.Arguments; got != "" {
		t.Fatalf("expected missing arguments to convert to empty string, got %q", got)
	}
}

func TestChatToolCallFunctionArgumentsAcceptJSONValues(t *testing.T) {
	var message ChatCompletionMessage
	data := []byte(`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":{"city":"Paris","days":0}}}]}`)

	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}

	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function == nil {
		t.Fatalf("expected one tool call, got %#v", message.ToolCalls)
	}
	if got := message.ToolCalls[0].Function.Arguments; got != `{"city":"Paris","days":0}` {
		t.Fatalf("expected object arguments to be normalized, got %q", got)
	}
}

func TestChatCompletionToolUnmarshalPreservesFunctionDefinition(t *testing.T) {
	var tool ChatCompletionTool
	data := []byte(`{"type":"function","function":{"name":"lookup","description":"resolve a record","parameters":{"type":"object","properties":{"id":{"type":"string"}}}}}`)

	if err := json.Unmarshal(data, &tool); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}

	if tool.Type != "function" {
		t.Fatalf("expected type %q, got %q", "function", tool.Type)
	}
	if tool.Function.Name != "lookup" {
		t.Fatalf("expected function name %q, got %q", "lookup", tool.Function.Name)
	}

	parameters, ok := tool.Function.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("expected parameters to unmarshal into a map, got %T", tool.Function.Parameters)
	}
	if parameters["type"] != "object" {
		t.Fatalf("expected schema type %q, got %#v", "object", parameters["type"])
	}
}

func TestChatCompletionToolMarshalPreservesFunctionDefinition(t *testing.T) {
	tool := ChatCompletionTool{
		Type: "function",
		Function: ChatCompletionFunction{
			Name:        "lookup",
			Description: "resolve a record",
			Parameters: map[string]any{
				"type": "object",
			},
		},
	}

	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unexpected payload unmarshal error: %v", err)
	}

	if payload["type"] != "function" {
		t.Fatalf("expected type %q, got %#v", "function", payload["type"])
	}

	functionPayload, ok := payload["function"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested function payload, got %T", payload["function"])
	}
	if functionPayload["name"] != "lookup" {
		t.Fatalf("expected function name %q, got %#v", "lookup", functionPayload["name"])
	}
}

func TestChatCompletionToolRoundTripPreservesResponsesTool(t *testing.T) {
	var tool ChatCompletionTool
	data := []byte(`{"type":"web_search_preview","search_context_size":"medium","vendor_extension":{"enabled":true}}`)

	if err := json.Unmarshal(data, &tool); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}

	if tool.Type != APIToolTypeWebSearchPreview {
		t.Fatalf("expected type %q, got %q", APIToolTypeWebSearchPreview, tool.Type)
	}
	if tool.ResponsesTool.SearchContextSize != "medium" {
		t.Fatalf("expected search_context_size %q, got %q", "medium", tool.ResponsesTool.SearchContextSize)
	}

	tool.ResponsesTool.SearchContextSize = "high"

	encoded, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("unexpected payload unmarshal error: %v", err)
	}

	if payload["type"] != APIToolTypeWebSearchPreview {
		t.Fatalf("expected type %q, got %#v", APIToolTypeWebSearchPreview, payload["type"])
	}
	if payload["search_context_size"] != "high" {
		t.Fatalf("expected updated search_context_size, got %#v", payload["search_context_size"])
	}
	if _, ok := payload["vendor_extension"]; !ok {
		t.Fatalf("expected vendor_extension to be preserved, got %#v", payload)
	}
}

func TestChatCompletionRequestToResponsesRequestFlattensCustomTool(t *testing.T) {
	var request ChatCompletionRequest
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-5",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"type":"custom","custom":{"name":"shell","description":"Run shell commands","format":{"type":"text"}}}]
	}`), &request); err != nil {
		t.Fatalf("unmarshal chat request: %v", err)
	}

	responses := request.ToResponsesRequest()
	if len(responses.Tools) != 1 {
		t.Fatalf("expected one converted custom tool, got %#v", responses.Tools)
	}
	encoded, err := json.Marshal(responses.Tools[0])
	if err != nil {
		t.Fatalf("marshal converted custom tool: %v", err)
	}
	var tool map[string]any
	if err := json.Unmarshal(encoded, &tool); err != nil {
		t.Fatalf("decode converted custom tool: %v", err)
	}
	if tool["type"] != "custom" || tool["name"] != "shell" || tool["description"] != "Run shell commands" {
		t.Fatalf("expected required custom tool fields to be flattened, got %#v", tool)
	}
	if _, exists := tool["custom"]; exists {
		t.Fatalf("expected Responses custom tool to be flat, got %#v", tool)
	}
	if _, exists := tool["format"]; !exists {
		t.Fatalf("expected custom tool format to survive flattening, got %#v", tool)
	}
}

func TestChatCompletionRequestToResponsesRequestDoesNotLeakChatStreamOptions(t *testing.T) {
	falseValue := false
	trueValue := true
	for _, test := range []struct {
		name          string
		streamOptions *StreamOptions
	}{
		{name: "usage only is omitted", streamOptions: &StreamOptions{IncludeUsage: true}},
		{name: "explicit false is omitted", streamOptions: &StreamOptions{IncludeUsage: true, IncludeObfuscation: &falseValue}},
		{name: "explicit true is omitted", streamOptions: &StreamOptions{IncludeObfuscation: &trueValue}},
	} {
		t.Run(test.name, func(t *testing.T) {
			converted := (&ChatCompletionRequest{Stream: true, StreamOptions: test.streamOptions}).ToResponsesRequest()
			body, err := json.Marshal(converted)
			if err != nil {
				t.Fatalf("marshal converted request: %v", err)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(body, &object); err != nil {
				t.Fatalf("decode converted request: %v", err)
			}
			if streamOptions, exists := object["stream_options"]; exists {
				t.Fatalf("Chat stream options leaked to Responses: %s", streamOptions)
			}
		})
	}
}

func TestResponsesToolsMarshalJSONPreservesUnknownFieldsAndReturnsErrors(t *testing.T) {
	var tool ResponsesTools
	if err := json.Unmarshal([]byte(`{"type":"web_search_preview","search_context_size":"medium","vendor_extension":{"enabled":true}}`), &tool); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}

	tool.SearchContextSize = "high"

	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unexpected payload unmarshal error: %v", err)
	}

	if payload["search_context_size"] != "high" {
		t.Fatalf("expected updated search_context_size, got %#v", payload["search_context_size"])
	}
	if _, ok := payload["vendor_extension"]; !ok {
		t.Fatalf("expected vendor_extension to be preserved, got %#v", payload)
	}

	tool.Parameters = func() {}
	_, err = json.Marshal(tool)
	if err == nil {
		t.Fatal("expected marshal error for unsupported function parameter type")
	}
}

func TestResponsesToolsMarshalJSONPreservesDescriptionForUnknownToolTypes(t *testing.T) {
	var tool ResponsesTools
	if err := json.Unmarshal([]byte(`{"type":"vendor_tool","description":"vendor-defined tool","vendor_extension":{"enabled":true}}`), &tool); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}

	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unexpected payload unmarshal error: %v", err)
	}

	if payload["description"] != "vendor-defined tool" {
		t.Fatalf("expected unknown tool description to be preserved, got %#v", payload)
	}
	if _, ok := payload["vendor_extension"]; !ok {
		t.Fatalf("expected vendor_extension to be preserved, got %#v", payload)
	}
}

func TestResponsesToolsMarshalJSONStripsDescriptionForServerTools(t *testing.T) {
	clientExecution := "client"
	testCases := []struct {
		name            string
		tool            ResponsesTools
		wantDescription bool
	}{
		{
			name: "function keeps description",
			tool: ResponsesTools{
				Type:        "function",
				Name:        "lookup",
				Description: "resolve a record",
			},
			wantDescription: true,
		},
		{
			name: "namespace keeps description",
			tool: ResponsesTools{
				Type:        "namespace",
				Name:        "browser",
				Description: "browser namespace",
			},
			wantDescription: true,
		},
		{
			name: "client tool search keeps description",
			tool: ResponsesTools{
				Type:        "tool_search",
				Description: "client-side search",
				Execution:   clientExecution,
			},
			wantDescription: true,
		},
		{
			name: "server tool search strips description",
			tool: ResponsesTools{
				Type:        "tool_search",
				Description: "hosted search",
			},
			wantDescription: false,
		},
		{
			name: "web search strips description",
			tool: ResponsesTools{
				Type:              APIToolTypeWebSearchPreview,
				Description:       "hosted search",
				SearchContextSize: "medium",
			},
			wantDescription: false,
		},
		{
			name: "unknown tool keeps description",
			tool: ResponsesTools{
				Type:        "vendor_tool",
				Description: "vendor-defined tool",
			},
			wantDescription: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			data, err := json.Marshal(testCase.tool)
			if err != nil {
				t.Fatalf("unexpected marshal error: %v", err)
			}

			var payload map[string]any
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Fatalf("unexpected payload unmarshal error: %v", err)
			}

			_, hasDescription := payload["description"]
			if hasDescription != testCase.wantDescription {
				t.Fatalf("expected description presence %v, got payload %#v", testCase.wantDescription, payload)
			}
		})
	}
}

func TestResponsesToolsMarshalJSONStripsNestedServerToolDescription(t *testing.T) {
	tool := ResponsesTools{
		Type:        "namespace",
		Name:        "browser",
		Description: "browser namespace",
		Tools: []ResponsesTools{
			{
				Type:        APIToolTypeFileSearch,
				Description: "hosted file search",
				Filters:     map[string]any{"type": "eq"},
			},
		},
	}

	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unexpected payload unmarshal error: %v", err)
	}

	if _, ok := payload["description"]; !ok {
		t.Fatalf("expected namespace description to be preserved, got %#v", payload)
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("expected nested tools, got %#v", payload["tools"])
	}
	nested, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("expected nested tool payload, got %T", tools[0])
	}
	if _, ok := nested["description"]; ok {
		t.Fatalf("expected nested server tool description to be stripped, got %#v", nested)
	}
	if _, ok := nested["filters"]; !ok {
		t.Fatalf("expected nested known fields to be preserved, got %#v", nested)
	}
}

func TestGetResponsesExtraBillingRecognizesNormalizedWebSearchAlias(t *testing.T) {
	billing := GetResponsesExtraBilling(&OpenAIResponsesResponses{
		Tools: []ResponsesTools{
			{Type: APIToolTypeWebSearch, SearchContextSize: "high"},
		},
		Output: []ResponsesOutput{
			{Type: InputTypeWebSearchCall, ID: "ws_1", Status: "completed", Action: map[string]any{"type": "search"}},
		},
	})

	entry, ok := billing[APIToolTypeWebSearch]
	if !ok {
		t.Fatalf("expected GA web_search to retain its product billing key, got %+v", billing)
	}
	if entry.Type != "high" || entry.CallCount != 1 {
		t.Fatalf("expected a single high web search charge, got %+v", entry)
	}
}

func TestGetResponsesExtraBillingAccumulatesMultipleWebSearchCalls(t *testing.T) {
	billing := GetResponsesExtraBilling(&OpenAIResponsesResponses{
		Tools: []ResponsesTools{
			{Type: APIToolTypeWebSearchPreview, SearchContextSize: "medium"},
		},
		Output: []ResponsesOutput{
			{Type: InputTypeWebSearchCall, ID: "ws_1", Status: "completed", Action: map[string]any{"type": "search"}},
			{Type: InputTypeWebSearchCall, ID: "ws_2", Status: "completed", Action: map[string]any{"type": "search"}},
			{Type: InputTypeWebSearchCall, ID: "ws_3", Status: "completed", Action: map[string]any{"type": "search"}},
		},
	})

	entry, ok := billing[APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected web search billing entry, got %+v", billing)
	}
	if entry.Type != "medium" || entry.CallCount != 3 {
		t.Fatalf("expected web search calls to accumulate to 3, got %+v", entry)
	}
}

func TestResponsesWebSearchBillingPreviewDominatesWithItsOwnVariant(t *testing.T) {
	serviceType, billingType := ResponsesWebSearchBilling(&OpenAIResponsesResponses{Tools: []ResponsesTools{
		{Type: APIToolTypeWebSearch, SearchContextSize: "high"},
		{Type: APIToolTypeWebSearchPreview, SearchContextSize: "low"},
	}})
	if serviceType != APIToolTypeWebSearchPreview || billingType != "low" {
		t.Fatalf("ambiguous search declaration mixed product and variant: service=%q type=%q", serviceType, billingType)
	}
}

func TestResponsesWebSearchBillingUsesCompletedActionEvidence(t *testing.T) {
	response := &OpenAIResponsesResponses{
		Output: []ResponsesOutput{
			{Type: InputTypeWebSearchCall, ID: "search", Status: "completed", Action: map[string]any{"type": "search"}},
			{Type: InputTypeWebSearchCall, ID: "open", Status: "completed", Action: map[string]any{"type": "open_page"}},
			{Type: InputTypeWebSearchCall, ID: "find", Status: "completed", Action: map[string]any{"type": "find_in_page"}},
			{Type: InputTypeWebSearchCall, ID: "pending", Status: "in_progress", Action: map[string]any{"type": "search"}},
			{Type: InputTypeWebSearchCall, ID: "unknown", Status: "completed"},
		},
	}
	billing := GetResponsesExtraBilling(response)
	if got := billing[APIToolTypeWebSearchPreview].CallCount; got != 2 {
		t.Fatalf("expected search plus one conservative unknown action charge, got %d in %+v", got, billing)
	}
	diagnostics := GetResponsesBillingDiagnostics(response)
	if !diagnostics["web_search_action_unknown"] {
		t.Fatalf("expected unknown action diagnostic, got %+v", diagnostics)
	}
}

func TestGetResponsesExtraBillingSeparatesImageGenerationVariants(t *testing.T) {
	billing := GetResponsesExtraBilling(&OpenAIResponsesResponses{
		Output: []ResponsesOutput{
			{Type: InputTypeImageGenerationCall, ID: "img_1", Status: "completed", Quality: "low", Size: "1024x1024"},
			{Type: InputTypeImageGenerationCall, ID: "img_2", Status: "completed", Quality: "high", Size: "1536x1024"},
			{Type: InputTypeImageGenerationCall, ID: "img_failed", Status: "failed", Quality: "high", Size: "1536x1024"},
		},
	})

	lowKey := BuildExtraBillingKey(APIToolTypeImageGeneration, "low-1024x1024")
	highKey := BuildExtraBillingKey(APIToolTypeImageGeneration, "high-1536x1024")
	if len(billing) != 2 {
		t.Fatalf("expected completed image variants to be tracked without the failed call, got %+v", billing)
	}
	if entry := billing[lowKey]; entry.ServiceType != APIToolTypeImageGeneration || entry.Type != "low-1024x1024" || entry.CallCount != 1 {
		t.Fatalf("expected low image generation variant billing entry, got %+v", entry)
	}
	if entry := billing[highKey]; entry.ServiceType != APIToolTypeImageGeneration || entry.Type != "high-1536x1024" || entry.CallCount != 1 {
		t.Fatalf("expected high image generation variant billing entry, got %+v", entry)
	}
}

func TestResponsesImageGenerationBillingCarriesToolPricingEvidence(t *testing.T) {
	response := &OpenAIResponsesResponses{
		Tools: []ResponsesTools{{
			Type:          APIToolTypeImageGeneration,
			Model:         "gpt-image-2",
			Quality:       "auto",
			Size:          "auto",
			PartialImages: float64(2),
		}},
		Output: []ResponsesOutput{{
			Type:    InputTypeImageGenerationCall,
			Status:  "completed",
			Quality: "high",
			Size:    "2048x2048",
		}},
	}
	billing := GetResponsesExtraBilling(response)
	key := BuildExtraBillingKey(APIToolTypeImageGeneration, "gpt-image-2|high|2048x2048|0")
	if entry := billing[key]; entry.CallCount != 1 || entry.Type != "gpt-image-2|high|2048x2048|0" {
		t.Fatalf("expected request partial image limit not to become billing evidence, got %+v", billing)
	}
}

func TestResponsesToolTypeHelpersAndAdditionalBillingBranches(t *testing.T) {
	if IsResponsesWebSearchToolType("custom-search") {
		t.Fatal("expected unknown tool types not to classify as responses web search tools")
	}
	if got := NormalizeResponsesWebSearchToolType(""); got != "" {
		t.Fatalf("expected empty responses web search tool type to stay empty, got %q", got)
	}
	if billing := GetResponsesExtraBilling(nil); billing != nil {
		t.Fatalf("expected nil response billing lookup to stay nil, got %+v", billing)
	}

	billing := GetResponsesExtraBilling(&OpenAIResponsesResponses{
		Output: []ResponsesOutput{
			{Type: InputTypeCodeInterpreterCall, ID: "code_1"},
			{Type: InputTypeFileSearchCall, ID: "file_1"},
		},
	})
	if len(billing) != 0 {
		t.Fatalf("unsupported hosted tools must not produce dormant billing evidence, got %+v", billing)
	}
}

func TestChatCompletionResponseToResponsesCopiesResponseObjectFields(t *testing.T) {
	background := true
	store := false
	maxToolCalls := 3
	temperature := 0.4
	topP := 0.9
	parallelToolCalls := true
	effort := "medium"
	summary := "auto"

	request := &OpenAIResponsesRequest{
		Model:              "gpt-5",
		Background:         &background,
		Instructions:       "Answer briefly.",
		MaxOutputTokens:    128,
		MaxToolCalls:       &maxToolCalls,
		Metadata:           map[string]string{"trace_id": "abc"},
		ParallelToolCalls:  &parallelToolCalls,
		PreviousResponseID: "resp_prev",
		Prompt:             map[string]any{"id": "pmpt_123"},
		Reasoning: &ReasoningEffort{
			Effort:  &effort,
			Summary: &summary,
		},
		Store:            &store,
		Temperature:      &temperature,
		ServiceTier:      "priority",
		ProcessingClass:  "flex",
		SafetyIdentifier: "user_123",
		Text: &ResponsesText{
			Verbosity: "low",
		},
		TopP: &topP,
	}

	response := (&ChatCompletionResponse{
		ID:          "resp_123",
		Model:       "gpt-5",
		Created:     1,
		ServiceTier: "flex",
		Usage:       &Usage{},
		Choices: []ChatCompletionChoice{
			{
				Message: ChatCompletionMessage{
					Role:    ChatMessageRoleAssistant,
					Content: "hello",
				},
				FinishReason: FinishReasonStop,
			},
		},
	}).ToResponses(request)

	if response.Instructions != request.Instructions {
		t.Fatalf("expected instructions %q, got %#v", request.Instructions, response.Instructions)
	}
	if response.Reasoning != request.Reasoning {
		t.Fatalf("expected reasoning pointer to be preserved")
	}
	if response.PreviousResponseID != request.PreviousResponseID {
		t.Fatalf("expected previous_response_id %q, got %q", request.PreviousResponseID, response.PreviousResponseID)
	}
	if response.MaxToolCalls != request.MaxToolCalls {
		t.Fatalf("expected max_tool_calls pointer to be preserved")
	}
	if response.Store != request.Store {
		t.Fatalf("expected store pointer to be preserved")
	}
	if response.Text != request.Text {
		t.Fatalf("expected response text to use request text config")
	}
	if response.ServiceTier != "flex" || response.ProcessingClass != request.ProcessingClass || response.SafetyIdentifier != request.SafetyIdentifier {
		t.Fatalf("expected shared Responses fields to survive Chat fallback response conversion, got service_tier=%q processing_class=%q safety_identifier=%q", response.ServiceTier, response.ProcessingClass, response.SafetyIdentifier)
	}
}

func TestResponsesUsageMarshalKeepsProviderCacheEvidenceInternal(t *testing.T) {
	usage := ResponsesUsage{
		InputTokens: 1,
		InputTokensDetails: &ResponsesUsageInputTokensDetails{
			CachedTokens:      2,
			CachedReadTokens:  3,
			CacheWriteTokens:  5,
			CachedWriteTokens: 7,
		},
	}
	raw, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	wire := string(raw)
	if !strings.Contains(wire, `"cached_tokens":2`) {
		t.Fatalf("official cached_tokens missing from wire: %s", wire)
	}
	if !strings.Contains(wire, `"cache_write_tokens":5`) {
		t.Fatalf("official cache_write_tokens missing from wire: %s", wire)
	}
	for _, internalField := range []string{"cached_read_tokens", "cached_write_tokens"} {
		if strings.Contains(wire, internalField) {
			t.Fatalf("provider billing evidence %q leaked onto Responses wire: %s", internalField, wire)
		}
	}
	if usage.InputTokensDetails.CachedReadTokens != 3 || usage.InputTokensDetails.CacheWriteTokens != 5 || usage.InputTokensDetails.CachedWriteTokens != 7 {
		t.Fatalf("marshalling mutated billing evidence: %+v", usage.InputTokensDetails)
	}
}

func TestChatToResponsesDoesNotInventServiceTierFromRequest(t *testing.T) {
	response := (&ChatCompletionResponse{
		ID:      "resp_no_tier",
		Model:   "gpt-5",
		Usage:   &Usage{},
		Choices: []ChatCompletionChoice{{FinishReason: FinishReasonStop}},
	}).ToResponses(&OpenAIResponsesRequest{Model: "gpt-5", ServiceTier: "priority"})
	if response.ServiceTier != "" {
		t.Fatalf("request tier is not final billing evidence, got %q", response.ServiceTier)
	}
}

func TestOpenAIResponsesRequestToChatCompletionRequestCopiesTextVerbosity(t *testing.T) {
	request := &OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: "hello",
		Text: &ResponsesText{
			Verbosity: "low",
		},
	}

	chat, err := request.ToChatCompletionRequest()
	if err != nil {
		t.Fatalf("unexpected conversion error: %v", err)
	}

	if chat.Verbosity != "low" {
		t.Fatalf("expected chat verbosity %q, got %q", "low", chat.Verbosity)
	}
}

func TestOpenAIResponsesRequestToChatCompletionRequestCopiesSharedRequestFields(t *testing.T) {
	store := false
	effort := "high"
	request := &OpenAIResponsesRequest{
		Model:            "gpt-5",
		Input:            "hello",
		Store:            &store,
		ServiceTier:      "priority",
		ProcessingClass:  "flex",
		SafetyIdentifier: "user_123",
		Reasoning:        &ReasoningEffort{Effort: &effort},
		ToolChoice:       map[string]any{"type": "function", "name": "lookup"},
	}

	chat, err := request.ToChatCompletionRequest()
	if err != nil {
		t.Fatalf("unexpected conversion error: %v", err)
	}
	if chat.Store == nil || *chat.Store {
		t.Fatalf("expected explicit store=false to be preserved, got %#v", chat.Store)
	}
	if chat.ServiceTier != request.ServiceTier || chat.ProcessingClass != request.ProcessingClass || chat.SafetyIdentifier != request.SafetyIdentifier {
		t.Fatalf("expected shared request fields to be copied, got service_tier=%q processing_class=%q safety_identifier=%q", chat.ServiceTier, chat.ProcessingClass, chat.SafetyIdentifier)
	}
	if chat.Reasoning != nil || chat.ReasoningEffort == nil || *chat.ReasoningEffort != effort {
		t.Fatalf("expected only official reasoning_effort to be emitted, got reasoning=%#v reasoning_effort=%#v", chat.Reasoning, chat.ReasoningEffort)
	}
	if effective := chat.EffectiveReasoning(); effective == nil || effective.Effort != effort {
		t.Fatalf("expected adapter reasoning effort %q, got %#v", effort, effective)
	}
	choice, ok := chat.ToolChoice.(map[string]any)
	if !ok || choice["type"] != "function" {
		t.Fatalf("expected named function tool choice to be converted, got %#v", chat.ToolChoice)
	}
	function, ok := choice["function"].(map[string]any)
	if !ok || function["name"] != "lookup" {
		t.Fatalf("expected Chat function tool choice shape, got %#v", chat.ToolChoice)
	}
	encoded, err := json.Marshal(chat)
	if err != nil {
		t.Fatalf("marshal converted Chat request: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode converted Chat request: %v", err)
	}
	if _, ok := fields["reasoning"]; ok {
		t.Fatalf("converted Chat request must not contain non-standard reasoning: %s", encoded)
	}
	if _, ok := fields["reasoning_effort"]; !ok {
		t.Fatalf("converted Chat request must contain reasoning_effort: %s", encoded)
	}
}

func TestOpenAIResponsesRequestToChatCompletionRequestRejectsReasoningSummary(t *testing.T) {
	value := "auto"
	for _, test := range []struct {
		name      string
		reasoning ReasoningEffort
	}{
		{name: "summary", reasoning: ReasoningEffort{Summary: &value}},
		{name: "generate_summary", reasoning: ReasoningEffort{GenerateSummary: &value}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := &OpenAIResponsesRequest{Model: "gpt-5", Input: "hello", Reasoning: &test.reasoning}
			if _, err := request.ToChatCompletionRequest(); err == nil {
				t.Fatal("expected lossy reasoning summary conversion to fail")
			}
		})
	}
}

func TestOpenAIResponsesRequestToChatCompletionRequestMapsTextFormat(t *testing.T) {
	request := &OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: "hello",
		Text: &ResponsesText{
			Verbosity: "low",
			Format: &ResponsesTextFormat{
				Type:        "json_schema",
				Name:        "person",
				Description: "Extract a person record.",
				Schema: map[string]any{
					"type": "object",
				},
				Strict: true,
			},
		},
	}

	chat, err := request.ToChatCompletionRequest()
	if err != nil {
		t.Fatalf("unexpected conversion error: %v", err)
	}

	if chat.Verbosity != "low" {
		t.Fatalf("expected chat verbosity %q, got %q", "low", chat.Verbosity)
	}
	if chat.ResponseFormat == nil {
		t.Fatal("expected chat response_format to be populated")
	}
	if chat.ResponseFormat.Type != "json_schema" {
		t.Fatalf("expected response_format type %q, got %q", "json_schema", chat.ResponseFormat.Type)
	}
	if chat.ResponseFormat.JsonSchema == nil {
		t.Fatal("expected json_schema payload to be populated")
	}
	if chat.ResponseFormat.JsonSchema.Name != "person" {
		t.Fatalf("expected schema name %q, got %q", "person", chat.ResponseFormat.JsonSchema.Name)
	}
	if chat.ResponseFormat.JsonSchema.Description != "Extract a person record." {
		t.Fatalf("expected schema description to be preserved, got %q", chat.ResponseFormat.JsonSchema.Description)
	}
	if chat.ResponseFormat.JsonSchema.Strict != true {
		t.Fatalf("expected strict=true, got %#v", chat.ResponseFormat.JsonSchema.Strict)
	}
}

func TestChatCompletionRequestToResponsesRequestMapsResponseFormatAndVerbosity(t *testing.T) {
	request := &ChatCompletionRequest{
		Model: "gpt-5",
		Messages: []ChatCompletionMessage{
			{
				Role:    ChatMessageRoleUser,
				Content: "hello",
			},
		},
		Verbosity: "high",
		ResponseFormat: &ChatCompletionResponseFormat{
			Type: "json_schema",
			JsonSchema: &FormatJsonSchema{
				Name:        "person",
				Description: "Extract a person record.",
				Schema: map[string]any{
					"type": "object",
				},
				Strict: true,
			},
		},
	}

	responses := request.ToResponsesRequest()
	if responses.Text == nil {
		t.Fatal("expected responses text config to be populated")
	}
	if responses.Text.Verbosity != "high" {
		t.Fatalf("expected verbosity %q, got %q", "high", responses.Text.Verbosity)
	}
	if responses.Text.Format == nil {
		t.Fatal("expected responses text format to be populated")
	}
	if responses.Text.Format.Type != "json_schema" {
		t.Fatalf("expected text.format type %q, got %q", "json_schema", responses.Text.Format.Type)
	}
	if responses.Text.Format.Name != "person" {
		t.Fatalf("expected schema name %q, got %q", "person", responses.Text.Format.Name)
	}
	if responses.Text.Format.Description != "Extract a person record." {
		t.Fatalf("expected schema description to be preserved, got %q", responses.Text.Format.Description)
	}
	if responses.Text.Format.Strict != true {
		t.Fatalf("expected strict=true, got %#v", responses.Text.Format.Strict)
	}
}

func TestChatCompletionRequestToResponsesRequestPreservesToolChoiceAndCustomHistory(t *testing.T) {
	var request ChatCompletionRequest
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-5",
		"tool_choice":{"type":"function","function":{"name":"lookup"}},
		"messages":[
			{"role":"assistant","tool_calls":[
				{"id":"call_custom","type":"custom","custom":{"name":"shell","input":"echo ok"}},
				{"id":"call_function","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"ok\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_custom","content":[{"type":"text","text":"custom result"}]},
			{"role":"tool","tool_call_id":"call_function","content":"function result"}
		]
	}`), &request); err != nil {
		t.Fatalf("unmarshal Chat request: %v", err)
	}

	responses := request.ToResponsesRequest()
	choice, ok := responses.ToolChoice.(map[string]any)
	if !ok || choice["type"] != ToolChoiceTypeFunction || choice["name"] != "lookup" {
		t.Fatalf("expected flat Responses function tool choice, got %#v", responses.ToolChoice)
	}
	if _, nested := choice["function"]; nested {
		t.Fatalf("Responses tool choice must not retain Chat nesting: %#v", choice)
	}
	inputs, ok := responses.Input.([]InputResponses)
	if !ok || len(inputs) != 4 {
		t.Fatalf("expected four converted tool history items, got %#v", responses.Input)
	}
	if inputs[0].Type != InputTypeCustomToolCall || inputs[0].CallID != "call_custom" || inputs[0].Name != "shell" || inputs[0].Input != "echo ok" {
		t.Fatalf("unexpected custom tool call conversion: %#v", inputs[0])
	}
	customOutput, ok := inputs[2].Output.([]ContentResponses)
	if inputs[2].Type != InputTypeCustomToolCallOutput || !ok || len(customOutput) != 1 || customOutput[0].Type != ContentTypeInputText || customOutput[0].Text != "custom result" {
		t.Fatalf("unexpected custom tool output conversion: %#v", inputs[2])
	}
	if inputs[1].Type != InputTypeFunctionCall || inputs[3].Type != InputTypeFunctionCallOutput || inputs[3].Output != "function result" {
		t.Fatalf("unexpected function tool history conversion: %#v", inputs)
	}

	encoded, err := json.Marshal(request.Messages[0].ToolCalls[0])
	if err != nil {
		t.Fatalf("marshal custom Chat tool call: %v", err)
	}
	var customCall map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &customCall); err != nil {
		t.Fatalf("decode custom Chat tool call: %v", err)
	}
	if _, ok := customCall["custom"]; !ok {
		t.Fatalf("custom payload missing after round trip: %s", encoded)
	}
	if _, ok := customCall["function"]; ok {
		t.Fatalf("custom tool call must not emit function:null: %s", encoded)
	}
}

func TestChatCompletionRequestToResponsesRequestPreservesAssistantContentWithToolCalls(t *testing.T) {
	var request ChatCompletionRequest
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-5",
		"messages":[{
			"role":"assistant",
			"content":"I will check that now.",
			"tool_calls":[{
				"id":"call_1",
				"type":"function",
				"function":{"name":"lookup","arguments":"{\"q\":\"ok\"}"}
			}]
		}]
	}`), &request); err != nil {
		t.Fatalf("unmarshal Chat request: %v", err)
	}

	responses := request.ToResponsesRequest()
	inputs, ok := responses.Input.([]InputResponses)
	if !ok || len(inputs) != 2 {
		t.Fatalf("expected assistant message and tool call, got %#v", responses.Input)
	}
	if inputs[0].Type != InputTypeMessage || inputs[0].Role != ChatMessageRoleAssistant {
		t.Fatalf("expected assistant message before tool call, got %#v", inputs[0])
	}
	content, ok := inputs[0].Content.([]ContentResponses)
	if !ok || len(content) != 1 || content[0].Type != ContentTypeOutputText || content[0].Text != "I will check that now." {
		t.Fatalf("assistant content was not preserved: %#v", inputs[0].Content)
	}
	if inputs[1].Type != InputTypeFunctionCall || inputs[1].CallID != "call_1" || inputs[1].Name != "lookup" || inputs[1].Arguments != `{"q":"ok"}` {
		t.Fatalf("tool call was not preserved: %#v", inputs[1])
	}
}

func TestChatCompletionResponseToResponsesPreservesContentWithToolCalls(t *testing.T) {
	chatResponse := &ChatCompletionResponse{
		ID: "chatcmpl_mixed", Model: "gpt-5", Usage: &Usage{},
		Choices: []ChatCompletionChoice{{
			FinishReason: FinishReasonToolCalls,
			Message: ChatCompletionMessage{
				Role:    ChatMessageRoleAssistant,
				Content: "I will check that now.",
				ToolCalls: []*ChatCompletionToolCalls{{
					Id: "call_1", Type: ToolChoiceTypeFunction,
					Function: &ChatCompletionToolCallsFunction{Name: "lookup", Arguments: `{"q":"ok"}`},
				}},
			},
		}},
	}

	responses := chatResponse.ToResponses(&OpenAIResponsesRequest{Model: "gpt-5"})
	if len(responses.Output) != 2 {
		t.Fatalf("expected message and tool call outputs, got %#v", responses.Output)
	}
	message := responses.Output[0]
	content, ok := message.Content.([]ContentResponses)
	if message.Type != InputTypeMessage || !ok || len(content) != 1 || content[0].Type != ContentTypeOutputText || content[0].Text != "I will check that now." {
		t.Fatalf("assistant content was not preserved before tool call: %#v", message)
	}
	tool := responses.Output[1]
	if tool.Type != InputTypeFunctionCall || tool.CallID != "call_1" || tool.Name != "lookup" || tool.Arguments == nil || *tool.Arguments != `{"q":"ok"}` {
		t.Fatalf("tool call was not preserved: %#v", tool)
	}
}

func TestOpenAIResponsesRequestToChatCompletionRequestPreservesInlineFileAndToolHistory(t *testing.T) {
	var request OpenAIResponsesRequest
	if err := json.Unmarshal([]byte(`{
		"model":"gpt-5",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_file","file_data":"data:application/pdf;base64,AA==","filename":"a.pdf"}]},
			{"type":"function_call","call_id":"call_function","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_function","output":[{"type":"input_text","text":"function result"}]},
			{"type":"custom_tool_call","call_id":"call_custom","name":"shell","input":"echo ok"},
			{"type":"custom_tool_call_output","call_id":"call_custom","output":"custom result"}
		]
	}`), &request); err != nil {
		t.Fatalf("unmarshal Responses request: %v", err)
	}

	chat, err := request.ToChatCompletionRequest()
	if err != nil {
		t.Fatalf("convert Responses request: %v", err)
	}
	if len(chat.Messages) != 5 {
		t.Fatalf("expected five Chat history messages, got %#v", chat.Messages)
	}
	parts, ok := chat.Messages[0].Content.([]ChatMessagePart)
	if !ok || len(parts) != 1 || parts[0].File == nil || parts[0].File.Filename != "a.pdf" {
		t.Fatalf("official filename was not preserved in Chat file content: %#v", chat.Messages[0].Content)
	}
	outputParts, ok := chat.Messages[2].Content.([]ChatMessagePart)
	if !ok || len(outputParts) != 1 || outputParts[0].Type != ContentTypeText || outputParts[0].Text != "function result" {
		t.Fatalf("Responses input_text tool output was not converted to Chat text: %#v", chat.Messages[2].Content)
	}
	custom := chat.Messages[3].ToolCalls
	if len(custom) != 1 || custom[0].Type != ToolChoiceTypeCustom || custom[0].Custom == nil || custom[0].Custom.Name != "shell" || custom[0].Custom.Input != "echo ok" {
		t.Fatalf("unexpected custom Chat assistant history: %#v", custom)
	}
	if chat.Messages[4].ToolCallID != "call_custom" || chat.Messages[4].Content != "custom result" {
		t.Fatalf("unexpected custom Chat tool result: %#v", chat.Messages[4])
	}
}

func TestOpenAIResponsesRequestToChatCompletionRequestRejectsNonTextToolOutput(t *testing.T) {
	for _, test := range []struct {
		name   string
		output any
	}{
		{name: "image", output: []any{map[string]any{"type": ContentTypeInputImage, "image_url": "https://example.com/a.png"}}},
		{name: "text extension", output: []any{map[string]any{"type": ContentTypeInputText, "text": "ok", "future": true}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := &OpenAIResponsesRequest{
				Model: "gpt-5",
				Input: []any{map[string]any{
					"type":    InputTypeFunctionCallOutput,
					"call_id": "call_1",
					"output":  test.output,
				}},
			}
			if _, err := request.ToChatCompletionRequest(); err == nil {
				t.Fatal("expected lossy tool output conversion to fail")
			}
		})
	}
}

func TestNonStreamToolCallConversionPreservesCustomUnion(t *testing.T) {
	chatResponse := &ChatCompletionResponse{
		ID: "chatcmpl_1", Model: "gpt-5", Usage: &Usage{},
		Choices: []ChatCompletionChoice{{
			FinishReason: FinishReasonToolCalls,
			Message: ChatCompletionMessage{Role: ChatMessageRoleAssistant, ToolCalls: []*ChatCompletionToolCalls{
				{Id: "call_custom", Type: ToolChoiceTypeCustom, Custom: &ChatCompletionToolCallsCustom{Name: "shell", Input: "echo ok"}},
				{Id: "call_function", Type: ToolChoiceTypeFunction, Function: &ChatCompletionToolCallsFunction{Name: "lookup", Arguments: "{}"}},
			}},
		}},
	}
	responses := chatResponse.ToResponses(&OpenAIResponsesRequest{Model: "gpt-5"})
	if len(responses.Output) != 2 || responses.Output[0].Type != InputTypeCustomToolCall || responses.Output[0].Input != "echo ok" || responses.Output[1].Type != InputTypeFunctionCall {
		t.Fatalf("unexpected Chat to Responses custom union conversion: %#v", responses.Output)
	}

	arguments := "{}"
	responsesResponse := &OpenAIResponsesResponses{
		ID: "resp_1", Model: "gpt-5", Usage: &ResponsesUsage{},
		Output: []ResponsesOutput{
			{Type: InputTypeCustomToolCall, CallID: "call_custom", Name: "shell", Input: "echo ok", Status: ResponseStatusCompleted},
			{Type: InputTypeFunctionCall, CallID: "call_function", Name: "lookup", Arguments: &arguments, Status: ResponseStatusCompleted},
		},
	}
	chat := responsesResponse.ToChat()
	if len(chat.Choices) != 1 || len(chat.Choices[0].Message.ToolCalls) != 2 || chat.Choices[0].FinishReason != FinishReasonToolCalls {
		t.Fatalf("unexpected Responses to Chat tool call conversion: %#v", chat.Choices)
	}
	if custom := chat.Choices[0].Message.ToolCalls[0]; custom.Type != ToolChoiceTypeCustom || custom.Custom == nil || custom.Custom.Input != "echo ok" || custom.Function != nil {
		t.Fatalf("custom Responses output was not preserved as Chat custom union: %#v", custom)
	}

	emptyInput, err := json.Marshal(ResponsesOutput{Type: InputTypeCustomToolCall, CallID: "call_empty", Name: "shell"})
	if err != nil {
		t.Fatalf("marshal empty custom tool input: %v", err)
	}
	var emptyFields map[string]json.RawMessage
	if err := json.Unmarshal(emptyInput, &emptyFields); err != nil {
		t.Fatalf("decode empty custom tool input: %v", err)
	}
	if input, ok := emptyFields["input"]; !ok || string(input) != `""` {
		t.Fatalf("custom tool call must preserve an explicitly empty input: %s", emptyInput)
	}
}
