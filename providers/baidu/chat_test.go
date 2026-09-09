package baidu

import (
	"encoding/json"
	"testing"

	"one-api/types"
)

func TestBaiduStreamUsesTerminalCumulativeSnapshot(t *testing.T) {
	usage := &types.Usage{}
	handler := &baiduStreamHandler{Usage: usage, Request: &types.ChatCompletionRequest{Model: "request-model"}}
	dataChan := make(chan string, 3)
	intermediate := &BaiduChatStreamResponse{BaiduChatResponse: BaiduChatResponse{Model: "actual-model", Usage: providerUsage(t, `{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}`)}}
	handler.convertToOpenaiStream(intermediate, dataChan)
	if usage.ProviderReported {
		t.Fatalf("intermediate Baidu snapshot authorized billing: %+v", usage)
	}
	terminal := &BaiduChatStreamResponse{BaiduChatResponse: BaiduChatResponse{Model: "actual-model", Usage: providerUsage(t, `{"prompt_tokens":5,"completion_tokens":4,"total_tokens":9}`)}, IsEnd: true}
	handler.convertToOpenaiStream(terminal, dataChan)
	if !usage.HasProviderUsage() || usage.CompletionTokens != 4 || usage.TotalTokens != 9 || usage.ResponseModel != "actual-model" {
		t.Fatalf("terminal Baidu snapshot was not used exactly once: %+v", usage)
	}

	partial := &BaiduChatStreamResponse{BaiduChatResponse: BaiduChatResponse{Model: "actual-model", Usage: providerUsage(t, `{"prompt_tokens":5,"total_tokens":5}`)}, IsEnd: true}
	handler.convertToOpenaiStream(partial, dataChan)
	if usage.HasProviderUsage() {
		t.Fatalf("partial terminal Baidu snapshot became provider evidence: %+v", usage)
	}
}

func providerUsage(t *testing.T, raw string) *types.Usage {
	t.Helper()
	var usage types.Usage
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		t.Fatalf("decode provider usage: %v", err)
	}
	return &usage
}
