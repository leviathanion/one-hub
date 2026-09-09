package ollama

import (
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func (OllamaProviderFactory) AssessChatRequest(_ *model.Channel, _ string, _ *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	return base.ChatRequestSupport{}, base.RequireOperationEndpoint(getOllamaConfig().ChatCompletions, "Chat Completions")
}
