package types

import (
	"encoding/json"
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
			{Type: InputTypeWebSearchCall, ID: "ws_1"},
		},
	})

	entry, ok := billing[APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected normalized web_search alias to map to web search billing, got %+v", billing)
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
			{Type: InputTypeWebSearchCall, ID: "ws_1"},
			{Type: InputTypeWebSearchCall, ID: "ws_2"},
			{Type: InputTypeWebSearchCall, ID: "ws_3"},
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

func TestGetResponsesExtraBillingSeparatesImageGenerationVariants(t *testing.T) {
	billing := GetResponsesExtraBilling(&OpenAIResponsesResponses{
		Output: []ResponsesOutput{
			{Type: InputTypeImageGenerationCall, ID: "img_1", Quality: "low", Size: "1024x1024"},
			{Type: InputTypeImageGenerationCall, ID: "img_2", Quality: "high", Size: "1536x1024"},
		},
	})

	lowKey := BuildExtraBillingKey(APIToolTypeImageGeneration, "low-1024x1024")
	highKey := BuildExtraBillingKey(APIToolTypeImageGeneration, "high-1536x1024")
	if len(billing) != 2 {
		t.Fatalf("expected image generation variants to be tracked separately, got %+v", billing)
	}
	if entry := billing[lowKey]; entry.ServiceType != APIToolTypeImageGeneration || entry.Type != "low-1024x1024" || entry.CallCount != 1 {
		t.Fatalf("expected low image generation variant billing entry, got %+v", entry)
	}
	if entry := billing[highKey]; entry.ServiceType != APIToolTypeImageGeneration || entry.Type != "high-1536x1024" || entry.CallCount != 1 {
		t.Fatalf("expected high image generation variant billing entry, got %+v", entry)
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
	if got := billing[APIToolTypeCodeInterpreter].CallCount; got != 1 {
		t.Fatalf("expected code interpreter usage billing, got %+v", billing[APIToolTypeCodeInterpreter])
	}
	if got := billing[APIToolTypeFileSearch].CallCount; got != 1 {
		t.Fatalf("expected file search usage billing, got %+v", billing[APIToolTypeFileSearch])
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
		Store:       &store,
		Temperature: &temperature,
		Text: &ResponsesText{
			Verbosity: "low",
		},
		TopP: &topP,
	}

	response := (&ChatCompletionResponse{
		ID:      "resp_123",
		Model:   "gpt-5",
		Created: 1,
		Usage:   &Usage{},
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
