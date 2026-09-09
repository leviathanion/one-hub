package gemini

import (
	"encoding/json"
	"strings"

	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"
)

func (GeminiProviderFactory) AssessChatRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	version := geminiAPIVersionWithContext(nil, channel)
	if err := base.RequireOperationEndpoint(getConfig(version).ChatCompletions, "Chat Completions"); err != nil {
		return base.ChatRequestSupport{}, err
	}
	if UsesOpenAIAPI(channel) {
		return base.ChatRequestSupport{}, nil
	}
	return base.ChatRequestSupport{}, ValidateNativeChatRequest(canonicalModel, request, "Gemini")
}

func (GeminiProviderFactory) AssessChatWireRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest, fields map[string]json.RawMessage) error {
	if !UsesOpenAIAPI(channel) {
		return nil
	}
	return openai.ValidateChatRequestForChannel(channel, canonicalModel, request, fields)
}

func ValidateNativeChatRequest(canonicalModel string, request *types.ChatCompletionRequest, adapter string) error {
	if err := base.ValidateFunctionOnlyChatRequest(request, adapter); err != nil {
		return err
	}
	if strings.TrimSpace(request.ServiceTier) != "" {
		return &base.RequestCapabilityError{Param: "service_tier", Message: "service_tier cannot be represented by the " + adapter + " adapter"}
	}
	if request.Audio != nil {
		return &base.RequestCapabilityError{Param: "audio", Message: "audio output cannot be represented by the " + adapter + " adapter"}
	}
	for _, modality := range request.Modalities {
		if strings.EqualFold(strings.TrimSpace(modality), "audio") {
			return &base.RequestCapabilityError{Param: "audio", Message: "audio output cannot be represented by the " + adapter + " adapter"}
		}
	}
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(canonicalModel)), "-tts") {
		return &base.RequestCapabilityError{Param: "audio", Message: "audio output cannot be represented by the " + adapter + " adapter"}
	}
	for _, function := range request.GetFunctions() {
		if function.Name == "googleSearch" {
			return &base.RequestCapabilityError{Param: "tools", Message: "Google Search grounding has no configured provider-unit price contract"}
		}
	}
	if request.ToolChoice == nil {
		return nil
	}
	if choice, ok := request.ToolChoice.(string); ok && strings.TrimSpace(choice) == types.ToolChoiceTypeAuto {
		return nil
	}
	return &base.RequestCapabilityError{Param: "tool_choice", Message: "tool_choice cannot be represented by the " + adapter + " adapter"}
}
