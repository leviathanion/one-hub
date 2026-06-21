package openai

import (
	"testing"

	"one-api/common/config"
	"one-api/model"
)

func TestOpenAIGetRequestHeadersUsesSingleAuthHeader(t *testing.T) {
	t.Run("azure classic uses api key only", func(t *testing.T) {
		proxy := ""
		provider := CreateOpenAIProvider(&model.Channel{Key: "azure-key", Type: config.ChannelTypeAzure, Proxy: &proxy}, "https://example.openai.azure.com")
		provider.IsAzure = true

		headers := provider.GetRequestHeaders()
		if got := headers["api-key"]; got != "azure-key" {
			t.Fatalf("expected api-key header, got %q", got)
		}
		if got := headers["Authorization"]; got != "" {
			t.Fatalf("expected no bearer auth header, got %q", got)
		}
	})

	t.Run("azure v1 uses bearer only", func(t *testing.T) {
		proxy := ""
		provider := CreateOpenAIProvider(&model.Channel{Key: "azure-v1-key", Type: config.ChannelTypeAzureV1, Proxy: &proxy}, "https://example.openai.azure.com")
		provider.IsAzure = true

		headers := provider.GetRequestHeaders()
		if got := headers["Authorization"]; got != "Bearer azure-v1-key" {
			t.Fatalf("expected bearer auth header, got %q", got)
		}
		if got := headers["api-key"]; got != "" {
			t.Fatalf("expected no api-key header, got %q", got)
		}
	})

	t.Run("non azure uses bearer only and filters auth model headers", func(t *testing.T) {
		proxy := ""
		modelHeaders := `{"Authorization":"Bearer should-not-send","api-key":"evil-api-key","X-Gateway-Auth":"gateway-token"}`
		provider := CreateOpenAIProvider(&model.Channel{
			Key:          "sk-test",
			Type:         config.ChannelTypeOpenAI,
			Proxy:        &proxy,
			ModelHeaders: &modelHeaders,
		}, "https://api.openai.com")

		headers := provider.GetRequestHeaders()
		if got := headers["Authorization"]; got != "Bearer sk-test" {
			t.Fatalf("expected channel bearer auth header, got %q", got)
		}
		if got := headers["api-key"]; got != "" {
			t.Fatalf("expected no api-key header, got %q", got)
		}
		if got := headers["X-Gateway-Auth"]; got != "gateway-token" {
			t.Fatalf("expected non-auth custom header to remain, got %q", got)
		}
	})
}
