package providers

import (
	"encoding/json"
	"fmt"
	"sort"

	"one-api/common/config"
	"one-api/common/jsonobject"
	"one-api/common/providerendpoint"
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

type chatRequestFactoryPolicy interface {
	AssessChatRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest) (base.ChatRequestSupport, error)
}

type chatWireFactoryPolicy interface {
	AssessChatWireRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest, fields map[string]json.RawMessage) error
}

type speechRequestFactoryPolicy interface {
	AssessSpeechRequest(channel *model.Channel, request *types.SpeechAudioRequest) error
}

type transcriptionRequestFactoryPolicy interface {
	AssessTranscriptionRequest(channel *model.Channel, request *types.AudioRequest) error
}

type responsesRequestFactoryPolicy interface {
	ResponsesSupport(channel *model.Channel) base.OperationSupport
	ValidateResponsesRequest(channel *model.Channel, operation base.Operation, fields map[string]json.RawMessage, modelName string, applyPreAdd bool) error
}

type claudeRequestFactoryPolicy interface {
	SupportsClaude(channel *model.Channel) bool
	AssessClaudeRequest(channel *model.Channel, canonicalModel string, fields map[string]json.RawMessage) error
}

type geminiRequestFactoryPolicy interface {
	SupportsGemini(channel *model.Channel) bool
	AssessGeminiRequest(channel *model.Channel, canonicalModel string, fields map[string]json.RawMessage) error
}

type requestExtensionFactoryPolicy interface {
	AssessRequestExtensions(candidate *model.Channel, protocol base.RequestProtocol, fields map[string]json.RawMessage) error
}

// AssessRequestExtensions 让扩展所有者在候选验证前检查归属，不创建 provider。
// 不属于代理扩展的原生字段由上游判断。
func AssessRequestExtensions(channel *model.Channel, protocol base.RequestProtocol, fields map[string]json.RawMessage) error {
	if len(fields) == 0 {
		return nil
	}
	keys := make([]int, 0, len(providerFactories))
	for key := range providerFactories {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	for _, key := range keys {
		if policy, ok := providerFactories[key].(requestExtensionFactoryPolicy); ok {
			if err := policy.AssessRequestExtensions(channel, protocol, fields); err != nil {
				return normalizeRequestCapabilityError(err, "request")
			}
		}
	}
	return nil
}

func SupportsClaude(channel *model.Channel) bool {
	policy, ok := nativeClaudeProviderFactoryForChannel(channel).(claudeRequestFactoryPolicy)
	return ok && policy.SupportsClaude(channel)
}

func SupportsGemini(channel *model.Channel) bool {
	policy, ok := providerFactoryForChannel(channel).(geminiRequestFactoryPolicy)
	return ok && policy.SupportsGemini(channel)
}

func AssessClaudeRequest(channel *model.Channel, canonicalModel string, fields map[string]json.RawMessage) error {
	if err := AssessRequestExtensions(channel, base.RequestProtocolClaude, fields); err != nil {
		return err
	}
	policy, ok := nativeClaudeProviderFactoryForChannel(channel).(claudeRequestFactoryPolicy)
	if !ok || !policy.SupportsClaude(channel) {
		return operationCapabilityError("selected channel does not implement Claude Messages")
	}
	return normalizeRequestCapabilityError(policy.AssessClaudeRequest(channel, canonicalModel, fields), "request")
}

func AssessGeminiRequest(channel *model.Channel, canonicalModel string, fields map[string]json.RawMessage) error {
	if err := AssessRequestExtensions(channel, base.RequestProtocolGemini, fields); err != nil {
		return err
	}
	policy, ok := providerFactoryForChannel(channel).(geminiRequestFactoryPolicy)
	if !ok || !policy.SupportsGemini(channel) {
		return operationCapabilityError("selected channel does not implement Gemini generateContent")
	}
	return normalizeRequestCapabilityError(policy.AssessGeminiRequest(channel, canonicalModel, fields), "request")
}

// AssessChatRequest asks the concrete factory to validate one effective Chat
// request without constructing a provider. Method presence is the operation
// registration; factories must reuse the same local config and mapping rules as
// Create.
func AssessChatRequest(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest, fields map[string]json.RawMessage) (base.ChatRequestSupport, error) {
	if err := AssessRequestExtensions(channel, base.RequestProtocolChat, fields); err != nil {
		return base.ChatRequestSupport{}, err
	}
	factory := providerFactoryForChannel(channel)
	policy, ok := factory.(chatRequestFactoryPolicy)
	if !ok {
		return base.ChatRequestSupport{}, operationCapabilityError("selected channel does not implement Chat Completions")
	}
	support, err := policy.AssessChatRequest(channel, canonicalModel, request)
	if err != nil {
		return support, normalizeRequestCapabilityError(err, "request")
	}
	if wirePolicy, ok := factory.(chatWireFactoryPolicy); ok {
		if err := wirePolicy.AssessChatWireRequest(channel, canonicalModel, request, fields); err != nil {
			return support, normalizeRequestCapabilityError(err, "request")
		}
	}
	return support, nil
}

func AssessSpeechRequest(channel *model.Channel, request *types.SpeechAudioRequest) error {
	factory := providerFactoryForChannel(channel)
	policy, ok := factory.(speechRequestFactoryPolicy)
	if !ok {
		return operationCapabilityError("selected channel does not implement Speech")
	}
	return normalizeRequestCapabilityError(policy.AssessSpeechRequest(channel, request), "request")
}

func AssessTranscriptionRequest(channel *model.Channel, request *types.AudioRequest) error {
	factory := providerFactoryForChannel(channel)
	policy, ok := factory.(transcriptionRequestFactoryPolicy)
	if !ok {
		return operationCapabilityError("selected channel does not implement Transcription")
	}
	return normalizeRequestCapabilityError(policy.AssessTranscriptionRequest(channel, request), "request")
}

// ResolveAdapterSupport 根据 provider 声明与管理员的 Responses-to-Chat 配置，
// 唯一派生 create 执行目标；协议关系不决定调用哪个适配器。
func ResolveAdapterSupport(channel *model.Channel) base.OperationSupport {
	support := base.OperationSupport{Operations: map[base.Operation]base.DataPath{}}
	if channel == nil {
		return support
	}
	factory := providerFactoryForChannel(channel)
	if policy, ok := factory.(responsesRequestFactoryPolicy); ok {
		support = cloneOperationSupport(policy.ResponsesSupport(channel))
	}
	if _, native := support.DataPath(base.OperationResponsesCreate); native {
		support.ResponsesCreateTarget = base.ResponsesCreateTargetProvider
	} else if channel.CompatibleResponse {
		if _, chatSupported := factory.(chatRequestFactoryPolicy); chatSupported {
			support.Operations[base.OperationResponsesCreate] = base.DataPathCrossProtocol
			support.ResponsesCreateTarget = base.ResponsesCreateTargetChatAdapter
		}
	}
	return support
}

// ValidateResponsesRequest delegates request-specific semantics to the same
// factory that declared native Responses support. Cross-protocol mapping and
// the target Chat adapter are validated by their own owners in relay.
func ValidateResponsesRequest(channel *model.Channel, operation base.Operation, fields map[string]json.RawMessage, modelName string, applyPreAdd bool) error {
	extensionFields := fields
	if channel != nil && applyPreAdd {
		params, err := channel.GetCustomParameterMap()
		if err != nil {
			return normalizeRequestCapabilityError(err, "custom_parameter")
		}
		if preAdd, _ := params["pre_add"].(bool); preAdd {
			raw, err := json.Marshal(fields)
			if fields == nil {
				raw = []byte("{}")
			}
			if err != nil {
				return normalizeRequestCapabilityError(err, "request")
			}
			raw, err = base.ApplyRawCustomParams(raw, params, modelName, true)
			if err != nil {
				return normalizeRequestCapabilityError(err, "custom_parameter")
			}
			object, err := jsonobject.Parse(raw)
			if err != nil {
				return normalizeRequestCapabilityError(err, "request")
			}
			extensionFields = object.Fields
		}
	}
	if err := AssessRequestExtensions(channel, base.RequestProtocolResponses, extensionFields); err != nil {
		return err
	}
	factory := providerFactoryForChannel(channel)
	policy, ok := factory.(responsesRequestFactoryPolicy)
	if !ok {
		return nil
	}
	return normalizeRequestCapabilityError(policy.ValidateResponsesRequest(channel, operation, fields, modelName, applyPreAdd), "request")
}

func cloneOperationSupport(source base.OperationSupport) base.OperationSupport {
	cloned := base.OperationSupport{Operations: make(map[base.Operation]base.DataPath, len(source.Operations))}
	for operation, path := range source.Operations {
		cloned.Operations[operation] = path
	}
	return cloned
}

func operationCapabilityError(message string) error {
	return &base.RequestCapabilityError{Param: "operation", Message: message}
}

func normalizeRequestCapabilityError(err error, defaultParam string) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*base.RequestCapabilityError); ok {
		return err
	}
	return &base.RequestCapabilityError{Param: defaultParam, Message: fmt.Sprint(err)}
}

// AssessEndpoint 检查无需协议转换的操作是否被自定义渠道显式启用。
// Chat/Responses 等可转换操作由各自 factory policy 判断实际使用的上游接口。
func AssessEndpoint(channel *model.Channel, relayMode int) error {
	if channel == nil || channel.Type != config.ChannelTypeCustom {
		return nil
	}
	definition, ok := providerendpoint.ForRelayMode(relayMode)
	if !ok {
		return nil
	}
	uri, err := channel.ResolveEndpoint(definition.ID)
	if err != nil {
		return err
	}
	return base.RequireOperationEndpoint(uri, definition.Label)
}
