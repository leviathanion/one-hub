package openai

import (
	"one-api/common/config"
	"one-api/model"
	"testing"
)

func TestNativeResourceURLCapability(t *testing.T) {
	for _, channelType := range []int{config.ChannelTypeOpenAI, config.ChannelTypeAzureV1, config.ChannelTypeAzure, config.ChannelTypeGemini} {
		proxy := ""
		baseURL := "https://provider.example"
		provider := CreateOpenAIProvider(&model.Channel{Type: channelType, Proxy: &proxy, BaseURL: &baseURL}, baseURL)
		got, err := provider.BuildResourceRelayURL("/v1/files/file%3Aid/content", "future=a%2Bb")
		supported := channelType == config.ChannelTypeOpenAI || channelType == config.ChannelTypeAzureV1
		if supported && (err != nil || got != "https://provider.example/v1/files/file%3Aid/content?future=a%2Bb") {
			t.Fatalf("type=%d url=%s err=%v", channelType, got, err)
		}
		if !supported && err == nil {
			t.Fatalf("unsupported type=%d url=%s", channelType, got)
		}
	}
}
