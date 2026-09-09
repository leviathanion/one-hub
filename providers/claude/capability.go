package claude

import (
	"strings"

	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func (ClaudeProviderFactory) AssessChatRequest(_ *model.Channel, _ string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	if err := base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions"); err != nil {
		return base.ChatRequestSupport{}, err
	}
	return base.ChatRequestSupport{}, ValidateChatRequest(request, "Claude")
}

func ValidateChatRequest(request *types.ChatCompletionRequest, adapter string) error {
	if err := base.ValidateFunctionOnlyChatRequest(request, adapter); err != nil {
		return err
	}
	if chatRequestsAudioOutput(request) {
		return &base.RequestCapabilityError{Param: "audio", Message: "audio output cannot be represented by the " + adapter + " adapter"}
	}
	if request.ToolChoice == nil {
		return nil
	}
	if choice, ok := request.ToolChoice.(string); ok {
		switch strings.TrimSpace(choice) {
		case types.ToolChoiceTypeAuto, types.ToolChoiceTypeRequired:
			return nil
		}
	}
	if choice, ok := request.ToolChoice.(map[string]any); ok {
		if function, ok := choice["function"].(map[string]any); ok {
			if name, ok := function["name"].(string); ok && strings.TrimSpace(name) != "" {
				return nil
			}
		}
	}
	return &base.RequestCapabilityError{Param: "tool_choice", Message: "tool_choice cannot be represented by the " + adapter + " adapter"}
}

func chatRequestsAudioOutput(request *types.ChatCompletionRequest) bool {
	if request == nil || request.Audio != nil {
		return request != nil
	}
	for _, modality := range request.Modalities {
		if strings.EqualFold(strings.TrimSpace(modality), "audio") {
			return true
		}
	}
	return false
}
