package openrouter

import (
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func (OpenRouterProviderFactory) AssessChatRequest(channel *model.Channel, _ string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	return base.ChatRequestSupport{
		PreservesRawStreamOptions: channel != nil && channel.AllowExtraBody && request != nil && !request.Stream,
	}, base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions")
}
