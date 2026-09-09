package openrouter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func TestConvertFromChatOpenaiPreservesRemoteMediaWithoutFetching(t *testing.T) {
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		gets.Add(1)
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("pdf"))
	}))
	t.Cleanup(server.Close)

	request := &ChatCompletionRequest{ChatCompletionRequest: types.ChatCompletionRequest{
		Model: "openrouter/auto",
		Messages: []types.ChatCompletionMessage{{
			Role: types.ChatMessageRoleUser,
			Content: []types.ChatMessagePart{{
				Type:     types.ContentTypeImageURL,
				ImageURL: &types.ChatMessageImageURL{URL: server.URL + "/document.pdf", Detail: "high"},
			}},
		}},
	}}
	want, err := json.Marshal(request.Messages)
	if err != nil {
		t.Fatal(err)
	}

	provider := &OpenRouterProvider{}
	provider.Channel = &model.Channel{}
	mode, policyErr := (OpenRouterProviderFactory{}).AssessChatRemoteMedia(nil, &request.ChatCompletionRequest, base.ChatRemoteMediaSummary{Items: 1, RemoteURLs: 1})
	if policyErr != nil || mode != base.RemoteMediaPassURL {
		t.Fatalf("OpenRouter remote media policy = %v, %v; want PassURL", mode, policyErr)
	}
	provider.ConvertFromChatOpenai(request, "openrouter")

	got, err := json.Marshal(request.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("remote media wire changed:\n got %s\nwant %s", got, want)
	}
	if got := gets.Load(); got != 0 {
		t.Fatalf("converter issued %d network requests, want 0", got)
	}
}

func TestCreateChatCompletionReturnsNormalizedReasoningWithoutInventingUsage(t *testing.T) {
	originalDisableTokenEncoders := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() { config.DisableTokenEncoders = originalDisableTokenEncoders })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl_openrouter",
			"object":"chat.completion",
			"model":"provider-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"answer","reasoning":"plan"},"finish_reason":"stop"}]
		}`))
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	proxy := ""
	baseURL := server.URL
	provider := OpenRouterProviderFactory{}.Create(&model.Channel{
		Type:    config.ChannelTypeOpenRouter,
		Key:     "test-key",
		Proxy:   &proxy,
		BaseURL: &baseURL,
	}).(*OpenRouterProvider)
	provider.Usage = &types.Usage{PromptTokens: 3}

	response, apiErr := provider.CreateChatCompletion(&types.ChatCompletionRequest{
		Model:    "gpt-3.5-turbo",
		Messages: []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: "hello"}},
	})
	if apiErr != nil {
		t.Fatalf("CreateChatCompletion returned error: %v", apiErr)
	}
	if len(response.Choices) != 1 || response.Choices[0].Message.ReasoningContent != "plan" || response.Choices[0].Message.Reasoning != "" {
		t.Fatalf("expected OpenRouter reasoning to be normalized, got %+v", response.Choices)
	}
	if response.Usage != nil || provider.Usage.ProviderReported {
		t.Fatalf("missing provider usage became billing evidence: response=%+v settlement=%+v", response.Usage, provider.Usage)
	}

	body, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal normalized response: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode normalized response: %v", err)
	}
	if _, ok := wire["usage"]; ok {
		t.Fatalf("normalized wire invented usage, got %s", body)
	}
	if response.ProviderRawJSON() != nil {
		t.Fatal("normalized adapters must not retain an unbounded raw response buffer")
	}
	if raw := response.ReplayProviderRawJSON(); len(raw) != 0 {
		t.Fatalf("OpenRouter normalization must not opt in to stale raw replay, got %s", raw)
	}
}

func TestCreateChatCompletionKeepsZeroOutputActualModelAndServerSearchEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl_openrouter_usage",
			"object":"chat.completion",
			"model":"actual/provider-model",
			"service_tier":"priority",
			"choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"content_filter"}],
			"usage":{"prompt_tokens":12,"completion_tokens":0,"total_tokens":12,"server_tool_use":{"web_search_requests":2}}
		}`))
	}))
	t.Cleanup(server.Close)

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	proxy := ""
	baseURL := server.URL
	provider := OpenRouterProviderFactory{}.Create(&model.Channel{Type: config.ChannelTypeOpenRouter, Key: "test-key", Proxy: &proxy, BaseURL: &baseURL}).(*OpenRouterProvider)
	provider.Usage = &types.Usage{}
	response, apiErr := provider.CreateChatCompletion(&types.ChatCompletionRequest{Model: "openrouter/auto"})
	if apiErr != nil {
		t.Fatalf("CreateChatCompletion returned error: %v", apiErr)
	}
	if response.Usage == nil || !provider.Usage.HasProviderUsage() || provider.Usage.CompletionTokens != 0 {
		t.Fatalf("zero-output provider usage was lost: %+v", provider.Usage)
	}
	if provider.Usage.ResponseModel != "actual/provider-model" || provider.Usage.ServiceTier != "priority" {
		t.Fatalf("provider attribution was lost: %+v", provider.Usage)
	}
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "")
	if provider.Usage.ExtraBilling[key].CallCount != 2 || !provider.Usage.HasProviderExtraBilling(key) {
		t.Fatalf("server web-search evidence was lost: %+v", provider.Usage)
	}
}
