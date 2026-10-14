package antigravity

import (
	"encoding/json"
	"one-api/common/config"
	"one-api/model"
	"one-api/providers/antigravity/wire"
	"one-api/providers/base"
	"one-api/providers/claude"
	"one-api/types"
)

func assessConversion(channel *model.Channel, dialect wire.Dialect, fields map[string]json.RawMessage, modelName string, applyPre bool) error {
	raw, err := json.Marshal(fields)
	var params map[string]interface{}
	if err == nil && channel != nil {
		params, err = channel.GetCustomParameterMap()
	}
	if err == nil && applyPre {
		raw, err = base.ApplyRawCustomParams(raw, params, modelName, true)
	}
	if err == nil {
		_, err = convertedRequest(params, dialect, raw, modelName)
	}
	return capabilityError(err)
}

func (AntigravityProviderFactory) AssessChatRequest(_ *model.Channel, _ string, _ *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	return base.ChatRequestSupport{}, nil
}

func (AntigravityProviderFactory) AssessChatWireRequest(channel *model.Channel, modelName string, request *types.ChatCompletionRequest, fields map[string]json.RawMessage) error {
	if fields == nil {
		raw, err := json.Marshal(request)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &fields); err != nil {
			return err
		}
	}
	return assessConversion(channel, wire.DialectChat, fields, modelName, false)
}

func (AntigravityProviderFactory) SupportsClaude(*model.Channel) bool { return true }
func (AntigravityProviderFactory) SupportsGemini(*model.Channel) bool { return true }

func (AntigravityProviderFactory) AssessClaudeRequest(channel *model.Channel, modelName string, fields map[string]json.RawMessage) error {
	return assessConversion(channel, wire.DialectClaude, fields, modelName, false)
}

func (AntigravityProviderFactory) AssessGeminiRequest(channel *model.Channel, modelName string, fields map[string]json.RawMessage) error {
	return assessConversion(channel, wire.DialectGemini, fields, modelName, false)
}

func (AntigravityProviderFactory) ResponsesSupport(*model.Channel) base.OperationSupport {
	return base.OperationSupport{Operations: map[base.Operation]base.DataPath{base.OperationResponsesCreate: base.DataPathCrossProtocol}}
}

func (AntigravityProviderFactory) ValidateResponsesRequest(channel *model.Channel, operation base.Operation, fields map[string]json.RawMessage, modelName string, applyPre bool) error {
	if operation != base.OperationResponsesCreate {
		return &base.RequestCapabilityError{Param: "operation", Message: "该渠道只支持无状态 Responses create"}
	}
	if err := base.ValidateResponsesCustomParameterCompatibility(channel, fields, modelName, operation, applyPre); err != nil {
		return err
	}
	return assessConversion(channel, wire.DialectResponses, fields, modelName, applyPre)
}

func (AntigravityProviderFactory) AssessRequestExtensions(candidate *model.Channel, protocol base.RequestProtocol, fields map[string]json.RawMessage) error {
	if protocol == base.RequestProtocolGemini {
		return nil
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	present, err := wire.InspectCarrier(raw, wire.Dialect(protocol))
	if err != nil {
		return capabilityError(err)
	}
	if present && (candidate == nil || candidate.Type != config.ChannelTypeAntigravity) {
		return &base.RequestCapabilityError{Param: "signature", Message: "Antigravity 签名扩展需要匹配的渠道"}
	}
	return nil
}

func (AntigravityProviderFactory) AssessChatRemoteMedia(*model.Channel, *types.ChatCompletionRequest, base.ChatRemoteMediaSummary) (base.RemoteMediaMode, error) {
	return base.RemoteMediaMaterialize, nil
}

func (AntigravityProviderFactory) AssessNativeClaudeRemoteMedia(_ *model.Channel, request *claude.ClaudeRequest, summary claude.NativeRemoteMediaSummary) (base.RemoteMediaMode, error) {
	return base.RemoteMediaMaterialize, claude.ValidateMaterializableNativeRemoteMedia(request, summary)
}

func (p *AntigravityProvider) MaterializeNativeClaudeRemoteMedia(request *claude.ClaudeRequest, fetcher base.RemoteMediaFetcher) (*claude.ClaudeRequest, error) {
	return claude.MaterializeNativeRemoteMedia(request, fetcher)
}
