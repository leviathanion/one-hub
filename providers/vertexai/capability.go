package vertexai

import (
	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/claude"
	"one-api/providers/gemini"
	"one-api/providers/vertexai/category"
	"one-api/types"
)

func (VertexAIProviderFactory) AssessChatRequest(_ *model.Channel, canonicalModel string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	if err := base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions"); err != nil {
		return base.ChatRequestSupport{}, err
	}
	providerCategory, err := category.GetCategory(canonicalModel)
	if err != nil || providerCategory == nil || providerCategory.ChatComplete == nil || providerCategory.ResponseChatComplete == nil {
		return base.ChatRequestSupport{}, &base.RequestCapabilityError{Param: "model", Message: "model cannot be represented by the Vertex AI adapter"}
	}
	switch providerCategory.Category {
	case "claude":
		return base.ChatRequestSupport{}, claude.ValidateChatRequest(request, "Vertex Claude")
	case "gemini":
		return base.ChatRequestSupport{}, gemini.ValidateNativeChatRequest(canonicalModel, request, "Vertex Gemini")
	default:
		return base.ChatRequestSupport{}, &base.RequestCapabilityError{Param: "model", Message: "model cannot be represented by the Vertex AI adapter"}
	}
}
