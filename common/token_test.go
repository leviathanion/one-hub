package common

import (
	"encoding/json"
	"testing"

	"one-api/common/config"
	"one-api/types"
)

func legacyCountTokenInputMessagesFallback(t *testing.T, input any, model string, preCostType int) int {
	t.Helper()

	jsonStr, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	var messages []types.ChatCompletionMessage
	if err := json.Unmarshal(jsonStr, &messages); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}

	return CountTokenMessages(messages, model, preCostType)
}

func TestCountTokenInputMessagesFastPathMatchesChatMessages(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	originalApproximate := config.ApproximateTokenEnabled
	config.DisableTokenEncoders = true
	config.ApproximateTokenEnabled = false
	t.Cleanup(func() {
		config.DisableTokenEncoders = originalDisable
		config.ApproximateTokenEnabled = originalApproximate
	})

	input := []any{
		map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{
					"type": "input_text",
					"text": "hello from responses",
				},
			},
		},
		map[string]any{
			"type":      "function_call",
			"call_id":   "call_1",
			"name":      "lookup_weather",
			"arguments": `{"city":"shanghai"}`,
		},
		map[string]any{
			"type":    "function_call_output",
			"call_id": "call_1",
			"output":  "sunny",
		},
	}

	expectedMessages := []types.ChatCompletionMessage{
		{
			Role: "user",
			Content: []any{
				map[string]any{
					"type": "text",
					"text": "hello from responses",
				},
			},
		},
		{
			Role: types.ChatMessageRoleAssistant,
			ToolCalls: []*types.ChatCompletionToolCalls{
				{
					Id:   "call_1",
					Type: "function",
					Function: &types.ChatCompletionToolCallsFunction{
						Name:      "lookup_weather",
						Arguments: `{"city":"shanghai"}`,
					},
				},
			},
		},
		{
			Role:       types.ChatMessageRoleTool,
			ToolCallID: "call_1",
			Content:    "sunny",
		},
	}

	got := CountTokenInputMessages(input, "gpt-4o", config.PreCostDefault)
	want := CountTokenMessages(expectedMessages, "gpt-4o", config.PreCostDefault)

	if got != want {
		t.Fatalf("expected fast-path token count %d, got %d", want, got)
	}
}

func TestCountTokenInputMessagesFallsBackForUnsupportedResponsesItems(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	originalApproximate := config.ApproximateTokenEnabled
	config.DisableTokenEncoders = true
	config.ApproximateTokenEnabled = false
	t.Cleanup(func() {
		config.DisableTokenEncoders = originalDisable
		config.ApproximateTokenEnabled = originalApproximate
	})

	input := []any{
		map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{
					"type": "input_text",
					"text": "hello from responses",
				},
			},
		},
		map[string]any{
			"type": types.InputTypeReasoning,
			"summary": []any{
				map[string]any{
					"type": types.ContentTypeSummaryText,
					"text": "internal reasoning summary",
				},
			},
		},
	}

	if _, ok := responsesInputToMessagesFast(input); ok {
		t.Fatal("expected mixed responses input to bypass fast path")
	}

	got := CountTokenInputMessages(input, "gpt-4o", config.PreCostDefault)
	want := legacyCountTokenInputMessagesFallback(t, input, "gpt-4o", config.PreCostDefault)

	if got != want {
		t.Fatalf("expected fallback token count %d, got %d", want, got)
	}
}
