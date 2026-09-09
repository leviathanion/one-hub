package groq

import (
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func (GroqProviderFactory) AssessChatRequest(_ *model.Channel, canonicalModel string, _ *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	if err := base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions"); err != nil {
		return base.ChatRequestSupport{}, err
	}
	return base.ChatRequestSupport{}, validateGroqCompoundCapability(canonicalModel)
}

func validateGroqCompoundCapability(modelName string) error {
	if !isCompoundModel(modelName) {
		return nil
	}
	return &base.RequestCapabilityError{Param: "model", Message: "Groq Compound requires per-model and executed-tool billing evidence"}
}
