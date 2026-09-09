package types

import (
	"encoding/json"
	"testing"
)

func TestChatCompletionResponseToResponsesKeepsContentWhenUsageIsMissing(t *testing.T) {
	response := (&ChatCompletionResponse{
		ID:    "chatcmpl_i024_missing",
		Model: "gpt-5",
		Choices: []ChatCompletionChoice{{
			FinishReason: FinishReasonToolCalls,
			Message: ChatCompletionMessage{
				Role:    ChatMessageRoleAssistant,
				Content: "hello",
				ToolCalls: []*ChatCompletionToolCalls{{
					Id:   "call_i024",
					Type: ToolChoiceTypeFunction,
					Function: &ChatCompletionToolCallsFunction{
						Name:      "lookup",
						Arguments: `{"q":"ok"}`,
					},
				}},
			},
		}},
	}).ToResponses(&OpenAIResponsesRequest{Model: "gpt-5"})

	if response == nil {
		t.Fatal("expected a converted Responses response")
	}
	if response.Usage != nil {
		t.Fatalf("missing Chat usage must remain absent, got %+v", response.Usage)
	}
	if len(response.Output) != 2 {
		t.Fatalf("expected assistant text and tool output, got %#v", response.Output)
	}
	message := response.Output[0]
	content, ok := message.Content.([]ContentResponses)
	if !ok || len(content) != 1 || content[0].Type != ContentTypeOutputText || content[0].Text != "hello" {
		t.Fatalf("assistant text was not preserved: %#v", message)
	}
	tool := response.Output[1]
	if tool.Type != InputTypeFunctionCall || tool.CallID != "call_i024" || tool.Name != "lookup" || tool.Arguments == nil || *tool.Arguments != `{"q":"ok"}` {
		t.Fatalf("tool call was not preserved: %#v", tool)
	}

	wire, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal converted Responses response: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("parse converted Responses JSON: %v", err)
	}
	if _, present := decoded["usage"]; present {
		t.Fatalf("missing usage must be omitted from public Responses JSON: %s", wire)
	}
	var clientResponse OpenAIResponsesResponses
	if err := json.Unmarshal(wire, &clientResponse); err != nil {
		t.Fatalf("client could not parse converted Responses JSON: %v", err)
	}
	if clientResponse.Usage != nil || len(clientResponse.Output) != 2 {
		t.Fatalf("parsed Responses JSON changed content or usage semantics: %+v", clientResponse)
	}
}

func TestChatCompletionResponseToResponsesPreservesCompleteUsage(t *testing.T) {
	response := (&ChatCompletionResponse{
		ID: "chatcmpl_i024_usage",
		Usage: &Usage{
			PromptTokens:     11,
			CompletionTokens: 7,
			TotalTokens:      18,
			PromptTokensDetails: PromptTokensDetails{
				AudioTokens:  2,
				CachedTokens: 3,
			},
			CompletionTokensDetails: CompletionTokensDetails{ReasoningTokens: 5},
		},
		Choices: []ChatCompletionChoice{{
			FinishReason: FinishReasonStop,
			Message:      ChatCompletionMessage{Role: ChatMessageRoleAssistant, Content: "hello"},
		}},
	}).ToResponses(&OpenAIResponsesRequest{Model: "gpt-5"})

	if response.Usage == nil {
		t.Fatal("complete Chat usage must be converted")
	}
	if response.Usage.InputTokens != 11 || response.Usage.OutputTokens != 7 || response.Usage.TotalTokens != 18 {
		t.Fatalf("basic usage fields changed during conversion: %+v", response.Usage)
	}
	if response.Usage.InputTokensDetails == nil || response.Usage.InputTokensDetails.AudioTokens != 2 || response.Usage.InputTokensDetails.CachedTokens != 3 {
		t.Fatalf("input usage details changed during conversion: %+v", response.Usage.InputTokensDetails)
	}
	if response.Usage.OutputTokensDetails == nil || response.Usage.OutputTokensDetails.ReasoningTokens != 5 {
		t.Fatalf("output usage details changed during conversion: %+v", response.Usage.OutputTokensDetails)
	}
}
