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

	if tool.Type != APITollTypeWebSearchPreview {
		t.Fatalf("expected type %q, got %q", APITollTypeWebSearchPreview, tool.Type)
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

	if payload["type"] != APITollTypeWebSearchPreview {
		t.Fatalf("expected type %q, got %#v", APITollTypeWebSearchPreview, payload["type"])
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
