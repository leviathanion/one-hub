package ali

import (
	"bytes"
	"encoding/json"
	"strings"

	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"
)

func (AliProviderFactory) AssessChatRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	useOpenAI := usesOpenAIAPI(channel)
	if err := base.RequireOperationEndpoint(getConfig(useOpenAI).ChatCompletions, "Chat Completions"); err != nil {
		return base.ChatRequestSupport{}, err
	}
	// The raw body may override or supply provider fields when extra-body
	// merging is enabled. Defer the decision to the wire-aware policy in that
	// case; providers.AssessChatRequest invokes it immediately after this hook.
	if channel != nil && channel.AllowExtraBody && request != nil {
		return base.ChatRequestSupport{}, nil
	}
	return base.ChatRequestSupport{}, validateAliSearchCapability(channel, canonicalModel, request)
}

func (AliProviderFactory) AssessChatWireRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest, fields map[string]json.RawMessage) error {
	if usesOpenAIAPI(channel) {
		if err := openai.ValidateChatRequestForChannel(channel, canonicalModel, request, fields); err != nil {
			return err
		}
	}
	return validateAliSearchCapabilityWithFields(channel, canonicalModel, request, fields)
}

func validateAliSearchCapability(channel *model.Channel, modelName string, request *types.ChatCompletionRequest) error {
	return validateAliSearchPlan(channel, modelName, request, nil)
}

func validateAliSearchCapabilityWithFields(channel *model.Channel, modelName string, request *types.ChatCompletionRequest, fields map[string]json.RawMessage) error {
	raw, err := aliRawFieldsMap(fields)
	if err != nil {
		return &base.RequestCapabilityError{Param: "request", Message: "Chat request cannot be evaluated for DashScope search billing evidence"}
	}
	return validateAliSearchPlan(channel, modelName, request, raw)
}

func validateAliSearchPlan(channel *model.Channel, modelName string, request *types.ChatCompletionRequest, raw map[string]interface{}) error {
	plan, err := planAliSearchRequest(channel, modelName, request, raw)
	if err != nil {
		return &base.RequestCapabilityError{Param: "custom_parameter", Message: "channel custom parameters are invalid"}
	}
	if plan.enabled {
		return &base.RequestCapabilityError{Param: "model", Message: "DashScope web search has no configured provider-unit price contract"}
	}
	return nil
}

type aliSearchPlan struct {
	native  bool
	body    map[string]interface{}
	enabled bool
}

// planAliSearchRequest 复用 BuildRequestWithMerge 的纯 body 规划器，使候选门禁
// 与 provider 入口检查同一 typed/raw/custom 合并结果，不执行 I/O，也不近似
// 嵌套 JSON 的合并语义。
func planAliSearchRequest(channel *model.Channel, modelName string, request *types.ChatCompletionRequest, raw map[string]interface{}) (aliSearchPlan, error) {
	native := !usesOpenAIAPI(channel)
	var typedBody interface{}
	if native {
		typedBody = aliNativeTypedRequest(channel, modelName, request)
	} else {
		typedBody = aliCompatibleTypedRequest(modelName, request)
	}

	var customParams map[string]interface{}
	if channel != nil {
		var err error
		customParams, err = channel.GetCustomParameterMap()
		if err != nil {
			return aliSearchPlan{}, err
		}
	}
	body, err := base.PlanRequestBodyWithMerge(
		typedBody,
		channel != nil && channel.AllowExtraBody,
		raw,
		customParams,
		modelName,
	)
	if err != nil {
		return aliSearchPlan{}, err
	}

	return aliSearchPlan{
		native:  native,
		body:    body,
		enabled: aliSearchEnabledInBody(body, native),
	}, nil
}

func aliNativeTypedRequest(channel *model.Channel, modelName string, request *types.ChatCompletionRequest) *AliChatRequest {
	effective := aliCompatibleTypedRequest(modelName, request)
	planner := &AliProvider{
		OpenAIProvider: openai.OpenAIProvider{
			BaseProvider: base.BaseProvider{Channel: channel},
		},
	}
	return planner.convertFromChatOpenai(effective)
}

func aliCompatibleTypedRequest(modelName string, request *types.ChatCompletionRequest) *types.ChatCompletionRequest {
	if request == nil {
		return &types.ChatCompletionRequest{Model: modelName}
	}
	effective := *request
	effective.Model = modelName
	return &effective
}

func aliSearchEnabledInBody(body map[string]interface{}, native bool) bool {
	if native {
		parameters, ok := body["parameters"].(map[string]interface{})
		if !ok {
			return false
		}
		enabled, _ := parameters["enable_search"].(bool)
		return enabled
	}
	enabled, _ := body["enable_search"].(bool)
	return enabled
}

func aliRawFieldsMap(fields map[string]json.RawMessage) (map[string]interface{}, error) {
	if fields == nil {
		return nil, nil
	}
	body := make(map[string]interface{}, len(fields))
	for name, raw := range fields {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value interface{}
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		body[name] = value
	}
	return body, nil
}

func aliSearchEnabled(channel *model.Channel, modelName string) bool {
	if channel == nil || channel.Plugin == nil {
		return false
	}
	plugin := channel.Plugin.Data()
	web, ok := plugin["web_search"]
	if !ok {
		return false
	}
	enabled, _ := web["enable"].(bool)
	if !enabled {
		return false
	}
	for _, supported := range strings.Split(WebSearchSupportedModels, ",") {
		if strings.Contains(modelName, supported) {
			return true
		}
	}
	return false
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
