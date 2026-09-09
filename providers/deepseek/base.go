package deepseek

import (
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"
)

type DeepseekProviderFactory struct{}

// 创建 DeepseekProvider
func (f DeepseekProviderFactory) Create(channel *model.Channel) base.ProviderInterface {
	config := getDeepseekConfig()
	return &DeepseekProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Config:    config,
				Channel:   channel,
				Requester: requester.NewHTTPRequester(*channel.Proxy, openai.RequestErrorHandle),
			},
			BalanceAction: false,
			UsageHandler:  deepSeekUsageHandler,
		},
	}
}

func deepSeekUsageHandler(usage *types.Usage) bool {
	if usage == nil {
		return false
	}
	if usage.ProviderTokenFields[config.UsageExtraDeepSeekCacheHit] {
		usage.SetExtraTokens(config.UsageExtraDeepSeekCacheHit, usage.ProviderPromptCacheHitTokens)
	}
	if usage.ProviderTokenFields[config.UsageExtraDeepSeekCacheMiss] {
		usage.SetExtraTokens(config.UsageExtraDeepSeekCacheMiss, usage.ProviderPromptCacheMissTokens)
	}
	usage.RequireTokenExtraEvidence(config.UsageExtraDeepSeekCacheHit, config.UsageExtraDeepSeekCacheMiss)
	usage.MarkProviderReported()
	return false
}

func getDeepseekConfig() base.ProviderConfig {
	return base.ProviderConfig{
		BaseURL:         "https://api.deepseek.com",
		ChatCompletions: "/v1/chat/completions",
		ModelList:       "/v1/models",
	}
}

type DeepseekProvider struct {
	openai.OpenAIProvider
}
