package zhipu

import (
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func (ZhipuProviderFactory) AssessChatRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	if err := base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions"); err != nil {
		return base.ChatRequestSupport{}, err
	}
	return base.ChatRequestSupport{}, validateZhipuSearchCapability(channel, canonicalModel, request)
}

func validateZhipuSearchCapability(channel *model.Channel, modelName string, request *types.ChatCompletionRequest) error {
	if request != nil {
		if modelName == "glm-4-alltools" && !request.Stream {
			return &base.RequestCapabilityError{Param: "stream", Message: "glm-4-alltools requires streaming"}
		}
		for _, tool := range request.Tools {
			if tool != nil && (tool.Type == "web_search" || tool.Type == "web_browser") {
				return &base.RequestCapabilityError{Param: "tools", Message: "Zhipu web search has no provider execution-count price contract"}
			}
		}
	}
	if channel == nil || channel.Plugin == nil {
		return nil
	}
	plugin := channel.Plugin.Data()
	if modelName == "glm-4-alltools" {
		if web, ok := plugin["web_browser"]; ok {
			if enabled, _ := web["enable"].(bool); enabled {
				return &base.RequestCapabilityError{Param: "tools", Message: "Zhipu web search has no provider execution-count price contract"}
			}
		}
		return nil
	}
	if retrieval, ok := plugin["retrieval"]; ok {
		if knowledgeID, _ := retrieval["knowledge_id"].(string); knowledgeID != "" {
			return nil
		}
	}
	if web, ok := plugin["web_search"]; ok {
		if enabled, _ := web["enable"].(bool); enabled {
			return &base.RequestCapabilityError{Param: "tools", Message: "Zhipu web search has no provider execution-count price contract"}
		}
	}
	return nil
}
