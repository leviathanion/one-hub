package claude

import (
	"testing"

	"one-api/common/config"
	"one-api/model"
)

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
	channel := &model.Channel{
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
