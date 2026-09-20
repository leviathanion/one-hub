package providers

import (
	"one-api/model"
	"one-api/providers/openai"
)

type imageStreamFactory interface{ SupportsImageStreaming(*model.Channel) bool }

func SupportsImageStreaming(channel *model.Channel) bool {
	factory, ok := providerFactoryForChannel(channel).(imageStreamFactory)
	return ok && factory.SupportsImageStreaming(channel)
}

func (openAICompatibleProviderFactory) SupportsImageStreaming(channel *model.Channel) bool {
	// The generic factory is the Custom OpenAI dialect adapter.
	return (openai.OpenAIProviderFactory{}).SupportsImageStreaming(channel)
}
