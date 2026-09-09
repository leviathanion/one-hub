package baidu

import (
	"encoding/json"

	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"
)

func (BaiduProviderFactory) AssessChatRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	useOpenAI := usesOpenAIAPI(channel)
	if err := base.RequireOperationEndpoint(getConfig(useOpenAI).ChatCompletions, "Chat Completions"); err != nil {
		return base.ChatRequestSupport{}, err
	}
	if useOpenAI {
		return base.ChatRequestSupport{}, nil
	}
	return base.ChatRequestSupport{}, nil
}

func (BaiduProviderFactory) AssessChatWireRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest, fields map[string]json.RawMessage) error {
	if !usesOpenAIAPI(channel) {
		return nil
	}
	return openai.ValidateChatRequestForChannel(channel, canonicalModel, request, fields)
}

func usesOpenAIAPI(channel *model.Channel) bool {
	if channel == nil || channel.Plugin == nil {
		return false
	}
	plugin := channel.Plugin.Data()
	setting, ok := plugin["use_openai_api"]
	if !ok {
		return false
	}
	enabled, _ := setting["enable"].(bool)
	return enabled
}
