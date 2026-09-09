package replicate

import (
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func (ReplicateProviderFactory) AssessChatRequest(_ *model.Channel, _ string, _ *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	return base.ChatRequestSupport{}, base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions")
}
