package bedrock

import (
	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/bedrock/category"
	"one-api/providers/claude"
	"one-api/types"
)

func (BedrockProviderFactory) AssessChatRequest(_ *model.Channel, canonicalModel string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	if err := base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions"); err != nil {
		return base.ChatRequestSupport{}, err
	}
	providerCategory, err := category.GetCategory(canonicalModel)
	if err != nil || providerCategory == nil || providerCategory.ChatComplete == nil || providerCategory.ResponseChatComplete == nil {
		return base.ChatRequestSupport{}, &base.RequestCapabilityError{Param: "model", Message: "model cannot be represented by the Bedrock adapter"}
	}
	return base.ChatRequestSupport{}, claude.ValidateChatRequest(request, "Bedrock Claude")
}
