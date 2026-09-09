package claude

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/common/config"
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func TestConvertFromChatOpenaiPassesRemoteImageURLWithoutFetching(t *testing.T) {
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		gets.Add(1)
		_, _ = w.Write([]byte("unexpected"))
	}))
	t.Cleanup(server.Close)
	mode, policyErr := (ClaudeProviderFactory{}).AssessChatRemoteMedia(nil, &types.ChatCompletionRequest{}, base.ChatRemoteMediaSummary{Items: 1, RemoteURLs: 1})
	if policyErr != nil || mode != base.RemoteMediaPassURL {
		t.Fatalf("Claude remote media policy = %v, %v; want PassURL", mode, policyErr)
	}

	request := &types.ChatCompletionRequest{
		Model: "claude-sonnet",
		Messages: []types.ChatCompletionMessage{{
			Role: types.ChatMessageRoleUser,
			Content: []types.ChatMessagePart{{
				Type:     types.ContentTypeImageURL,
				ImageURL: &types.ChatMessageImageURL{URL: server.URL + "/image.png"},
			}},
		}},
	}
	converted, apiErr := ConvertFromChatOpenai(request)
	if apiErr != nil {
		t.Fatalf("convert request: %v", apiErr)
	}
	if got := gets.Load(); got != 0 {
		t.Fatalf("converter issued %d network requests, want 0", got)
	}
	if len(converted.Messages) != 1 {
		t.Fatalf("unexpected converted messages: %+v", converted.Messages)
	}
	content, ok := converted.Messages[0].Content.([]MessageContent)
	if !ok || len(content) != 1 {
		t.Fatalf("unexpected converted content: %#v", converted.Messages[0].Content)
	}
	part := content[0]
	if part.Type != "image" || part.Source == nil || part.Source.Type != "url" || part.Source.Url != server.URL+"/image.png" {
		t.Fatalf("remote image URL was not represented as a Claude URL source: %+v", part)
	}
}

func TestClaudeNativeRemoteMediaPolicyPassesURLSources(t *testing.T) {
	request := nativeClaudeMediaRequest(map[string]any{"type": "document", "source": map[string]any{"type": "url", "url": "https://example.com/a.pdf"}})
	mode, err := (ClaudeProviderFactory{}).AssessNativeClaudeRemoteMedia(nil, request, SummarizeNativeRemoteMedia(request))
	if err != nil || mode != base.RemoteMediaPassURL {
		t.Fatalf("direct Claude native media policy=%v err=%v, want PassURL", mode, err)
	}
}

func TestConvertFromChatOpenaiKeepsPreparedDataURIAsBase64(t *testing.T) {
	request := &types.ChatCompletionRequest{
		Model: "claude-sonnet",
		Messages: []types.ChatCompletionMessage{{
			Role: types.ChatMessageRoleUser,
			Content: []types.ChatMessagePart{{
				Type:     types.ContentTypeImageURL,
				ImageURL: &types.ChatMessageImageURL{URL: "data:image/png;base64,aW1hZ2U="},
			}},
		}},
	}
	converted, apiErr := ConvertFromChatOpenai(request)
	if apiErr != nil {
		t.Fatalf("convert request: %v", apiErr)
	}
	content, ok := converted.Messages[0].Content.([]MessageContent)
	if !ok || len(content) != 1 {
		t.Fatalf("unexpected converted content: %#v", converted.Messages[0].Content)
	}
	part := content[0]
	if part.Source == nil || part.Source.Type != "base64" || part.Source.MediaType != "image/png" || part.Source.Data != "aW1hZ2U=" {
		t.Fatalf("prepared data URI was not preserved as base64: %+v", part)
	}
}

func TestClaudeProviderFactoryPreservesChannelBaseURL(t *testing.T) {
	baseURL := "https://anthropic-proxy.example.com/api"
	proxy := ""
	channel := &model.Channel{
		Type:    config.ChannelTypeAnthropic,
		BaseURL: &baseURL,
		Proxy:   &proxy,
	}

	provider, ok := ClaudeProviderFactory{}.Create(channel).(*ClaudeProvider)
	if !ok {
		t.Fatalf("expected ClaudeProvider")
	}

	if got := provider.GetBaseURL(); got != baseURL {
		t.Fatalf("expected channel base URL %q, got %q", baseURL, got)
	}
	if got := provider.GetFullRequestURL("/v1/messages"); got != baseURL+"/v1/messages" {
		t.Fatalf("expected full request URL to use channel base URL, got %q", got)
	}
}

func TestCreateClaudeProviderExplicitBaseURLOverride(t *testing.T) {
	channelBaseURL := "https://openai-compatible.example.com"
	overrideBaseURL := "https://claude-relay.example.com/api"
	proxy := ""
	channel := &model.Channel{Plugin: model.NewCustomEndpointPlugin(),
		Type:    config.ChannelTypeCustom,
		BaseURL: &channelBaseURL,
		Proxy:   &proxy,
	}

	provider := CreateClaudeProvider(channel, overrideBaseURL)

	if got := provider.GetBaseURL(); got != overrideBaseURL {
		t.Fatalf("expected explicit base URL override %q, got %q", overrideBaseURL, got)
	}
	if got := provider.GetFullRequestURL("/v1/messages"); got != overrideBaseURL+"/v1/messages" {
		t.Fatalf("expected full request URL to use explicit override, got %q", got)
	}
}

func TestClaudeChatUsageKeepsCacheAccountingOffTheChatWire(t *testing.T) {
	var providerUsage Usage
	if err := json.Unmarshal([]byte(`{
		"input_tokens":11,
		"output_tokens":7,
		"cache_creation_input_tokens":5,
		"cache_read_input_tokens":3,
		"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":0}
	}`), &providerUsage); err != nil {
		t.Fatalf("decode Claude provider usage: %v", err)
	}
	usage := &types.Usage{}
	if ok := ClaudeUsageToOpenaiUsage(&providerUsage, usage); !ok {
		t.Fatal("expected Claude usage conversion to succeed")
	}

	raw, err := json.Marshal(types.ChatCompletionResponse{
		ID:      "chatcmpl_claude",
		Object:  "chat.completion",
		Choices: []types.ChatCompletionChoice{},
		Usage:   usage,
	})
	if err != nil {
		t.Fatalf("marshal Claude Chat response: %v", err)
	}
	for _, field := range []string{"cached_write_tokens", "cached_read_tokens"} {
		if strings.Contains(string(raw), `"`+field+`"`) {
			t.Fatalf("internal Claude accounting field %q escaped onto Chat wire: %s", field, raw)
		}
	}
	if usage.GetExtraTokens()[config.UsageExtraClaudeCacheWrite5m] != 5 || usage.GetExtraTokens()[config.UsageExtraClaudeCacheWrite1h] != 0 || usage.PromptTokensDetails.CachedReadTokens != 3 {
		t.Fatalf("wire projection discarded internal Claude accounting: %+v", usage.PromptTokensDetails)
	}
}

func TestClaudeUsageRequiresCompleteCacheCreationPartition(t *testing.T) {
	var providerUsage Usage
	if err := json.Unmarshal([]byte(`{"input_tokens":0,"output_tokens":4,"cache_creation_input_tokens":8}`), &providerUsage); err != nil {
		t.Fatalf("decode Claude provider usage: %v", err)
	}
	usage := &types.Usage{}
	if !ClaudeUsageToOpenaiUsage(&providerUsage, usage) {
		t.Fatal("provider extractor should publish diagnosable usage")
	}
	if usage.HasProviderUsage() {
		t.Fatalf("unpartitioned cache creation became priceable: %+v", usage)
	}
}

func TestClaudeUsageKeepsZeroOutputAndIndependentWebSearchEvidence(t *testing.T) {
	var providerUsage Usage
	if err := json.Unmarshal([]byte(`{
		"input_tokens":9,"output_tokens":0,
		"server_tool_use":{"web_search_requests":2}
	}`), &providerUsage); err != nil {
		t.Fatalf("decode Claude provider usage: %v", err)
	}
	usage := &types.Usage{}
	if !ClaudeUsageToOpenaiUsage(&providerUsage, usage) || !usage.HasProviderUsage() {
		t.Fatalf("zero-output Claude usage was lost: %+v", usage)
	}
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "")
	if usage.ExtraBilling[key].CallCount != 2 || !usage.HasProviderExtraBilling(key) {
		t.Fatalf("Claude web-search evidence was lost: %+v", usage)
	}
}

func TestConvertFromChatOpenAIUsesOfficialReasoningEffort(t *testing.T) {
	effort := "high"
	request := &types.ChatCompletionRequest{
		Model:               "claude-sonnet",
		MaxCompletionTokens: 1024,
		ReasoningEffort:     &effort,
		Messages: []types.ChatCompletionMessage{
			{Role: types.ChatMessageRoleUser, Content: "hello"},
		},
	}

	converted, apiErr := ConvertFromChatOpenai(request)
	if apiErr != nil {
		t.Fatalf("convert request: %v", apiErr)
	}
	if converted.Thinking == nil || converted.Thinking.Type != "adaptive" {
		t.Fatalf("official reasoning_effort did not enable Claude thinking: %+v", converted.Thinking)
	}
	if converted.OutputConfig == nil || converted.OutputConfig.Effort != effort {
		t.Fatalf("Claude output effort = %+v, want %q", converted.OutputConfig, effort)
	}
}
