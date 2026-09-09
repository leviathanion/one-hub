package cohere

import (
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func (CohereProviderFactory) AssessChatRequest(_ *model.Channel, _ string, _ *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	return base.ChatRequestSupport{}, base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions")
}
