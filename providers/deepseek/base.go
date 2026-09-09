package deepseek

import (
	"fmt"
	"one-api/common/config"
	"one-api/common/logger"
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
			BalanceAction:        false,
			UsageHandler:         deepSeekUsageHandler,
			SupportStreamOptions: true,
		},
	}
}

func deepSeekUsageHandler(usage *types.Usage) bool {
	if usage == nil {
		return false
	}
	hasCached := usage.ProviderTokenFields[config.UsageExtraCache]
	if usage.ProviderTokenFields["prompt_cache_hit_tokens"] {
		if !hasCached {
			// 原生命中数只是标准缓存字段的别名，显式的标准零值也具有优先权。
			usage.PromptTokensDetails.CachedTokens = usage.ProviderPromptCacheHitTokens
			usage.MarkProviderTokenField(config.UsageExtraCache)
		} else if usage.PromptTokensDetails.CachedTokens != usage.ProviderPromptCacheHitTokens {
			if !usage.BillingDiagnostics["deepseek_cache_alias_mismatch"] {
				logger.SysError(fmt.Sprintf("DeepSeek 缓存命中用量不一致：cached_tokens=%d，prompt_cache_hit_tokens=%d；采用 cached_tokens", usage.PromptTokensDetails.CachedTokens, usage.ProviderPromptCacheHitTokens))
			}
			usage.AddBillingDiagnostic("deepseek_cache_alias_mismatch")
		}
	}
	usage.RequireTokenExtraEvidence(config.UsageExtraCache)
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
