package moonshot

import (
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func (MoonshotProviderFactory) AssessChatRequest(_ *model.Channel, _ string, _ *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	return base.ChatRequestSupport{}, base.RequireOperationEndpoint(getMoonshotConfig().ChatCompletions, "Chat Completions")
}
