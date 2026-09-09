package tencent

import (
	"encoding/json"
	"testing"

	"one-api/types"
)

func TestTencentStreamAuthorizesUsageOnlyOnTerminalChoice(t *testing.T) {
	usage := &types.Usage{}
	handler := &tencentStreamHandler{Usage: usage, Request: &types.ChatCompletionRequest{Model: "hunyuan"}}
	data := make(chan string, 3)
	handler.convertToOpenaiStream(&TencentChatResponse{
		Model:   "hunyuan",
		Choices: []TencentResponseChoices{{Delta: TencentMessage{Content: "a"}}},
		Usage:   &types.Usage{PromptTokens: 3, CompletionTokens: 0, TotalTokens: 3},
	}, data)
	if usage.HasProviderUsage() {
		t.Fatalf("nonterminal Tencent frame authorized usage: %+v", usage)
	}
	handler.convertToOpenaiStream(&TencentChatResponse{
		Model:   "hunyuan",
		Choices: []TencentResponseChoices{{FinishReason: "stop"}},
		Usage:   &types.Usage{PromptTokens: 3, CompletionTokens: 0, TotalTokens: 3},
	}, data)
	if !usage.HasProviderUsage() || usage.CompletionTokens != 0 {
		t.Fatalf("terminal Tencent zero-output usage was not authorized: %+v", usage)
	}

	var partial TencentChatResponse
	if err := json.Unmarshal([]byte(`{"model":"hunyuan","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"total_tokens":3}}`), &partial); err != nil {
		t.Fatal(err)
	}
	handler.convertToOpenaiStream(&partial, data)
	if usage.HasProviderUsage() {
		t.Fatalf("partial terminal Tencent usage became provider evidence: %+v", usage)
	}
}
