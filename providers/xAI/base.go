package xAI

import (
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"
)

// 定义供应商工厂
type XAIProviderFactory struct{}

func (XAIProviderFactory) AssessChatRemoteMedia(_ *model.Channel, _ *types.ChatCompletionRequest, _ base.ChatRemoteMediaSummary) (base.RemoteMediaMode, error) {
	if err := base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions"); err != nil {
		return base.RemoteMediaReject, err
	}
	return base.RemoteMediaPassURL, nil
}

// 创建 XAIProvider
func (f XAIProviderFactory) Create(channel *model.Channel) base.ProviderInterface {
	return &XAIProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{
				Config:    getConfig(),
				Channel:   channel,
				Requester: requester.NewHTTPRequester(*channel.Proxy, RequestErrorHandle),
			},
			UsageHandler:         usageHandler,
			SupportStreamOptions: true,
		},
	}
}

func getConfig() base.ProviderConfig {
	return base.ProviderConfig{
		BaseURL:           "https://api.x.ai",
		ChatCompletions:   "/v1/chat/completions",
		ImagesGenerations: "/v1/images/generations",
		ModelList:         "/v1/models",
	}
}

type XAIProvider struct {
	openai.OpenAIProvider
}
