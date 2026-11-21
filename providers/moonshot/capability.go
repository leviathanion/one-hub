package moonshot

import (
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func (MoonshotProviderFactory) AssessChatRequest(channel *model.Channel, _ string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	return base.ChatRequestSupport{
		PreservesRawStreamOptions: channel != nil && channel.AllowExtraBody && request != nil && !request.Stream,
	}, base.RequireOperationEndpoint(getMoonshotConfig().ChatCompletions, "Chat Completions")
}
