package xunfei

import (
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func (XunfeiProviderFactory) AssessChatRequest(_ *model.Channel, _ string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	if err := base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions"); err != nil {
		return base.ChatRequestSupport{}, err
	}
	if err := validateXunfeiChatRequest(request); err != nil {
		return base.ChatRequestSupport{}, &base.RequestCapabilityError{Param: "tools", Message: err.Error()}
	}
	return base.ChatRequestSupport{}, nil
}
