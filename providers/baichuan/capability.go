package baichuan

import (
	"strings"

	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func (BaichuanProviderFactory) AssessChatRequest(channel *model.Channel, canonicalModel string, _ *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	if err := base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions"); err != nil {
		return base.ChatRequestSupport{}, err
	}
	return base.ChatRequestSupport{}, validateBaichuanSearchCapability(channel, canonicalModel)
}

func validateBaichuanSearchCapability(channel *model.Channel, modelName string) error {
	modelLower := strings.ToLower(strings.TrimSpace(modelName))
	if strings.Contains(modelLower, "baichuan-m3-plus") || strings.Contains(modelLower, "baichuan-m2-plus") {
		return &base.RequestCapabilityError{Param: "model", Message: "Baichuan automatic search has no provider execution-count price contract"}
	}
	if channel == nil {
		return nil
	}
	customParams, err := channel.GetCustomParameterMap()
	if err != nil {
		return err
	}
	effective := base.ApplyCustomParams(map[string]any{}, customParams, modelName, false)
	if enabled, _ := effective["with_search_enhance"].(bool); enabled {
		return &base.RequestCapabilityError{Param: "with_search_enhance", Message: "Baichuan search enhance has no provider execution-count price contract"}
	}
	return nil
}
