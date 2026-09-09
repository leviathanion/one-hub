package siliconflow

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"
)

func TestIssue005SiliconflowInheritedOpenAIChatUsageRemainsProviderEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","model":"Qwen/Qwen3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":14,"completion_tokens":45,"total_tokens":59}}`)
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	proxy := ""
	baseURL := server.URL
	channel := &model.Channel{Type: config.ChannelTypeSiliconflow, Key: "sk-test", Proxy: &proxy, BaseURL: &baseURL}
	provider := (SiliconflowProviderFactory{}).Create(channel).(*SiliconflowProvider)
	provider.SetUsage(&types.Usage{})
	response, apiErr := provider.CreateChatCompletion(&types.ChatCompletionRequest{
		Model:    "Qwen/Qwen3",
		Messages: []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: "hello"}},
	})
	if apiErr != nil || response == nil {
		t.Fatalf("inherited OpenAI chat failed: response=%+v err=%+v", response, apiErr)
	}
	if !provider.Usage.HasProviderBaseUsage() || provider.Usage.PromptTokens != 14 || provider.Usage.CompletionTokens != 45 || provider.Usage.TotalTokens != 59 {
		t.Fatalf("inherited OpenAI usage was not preserved: %+v", provider.Usage)
	}
}
