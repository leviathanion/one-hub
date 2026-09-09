package codex

import (
	"encoding/json"

	"one-api/common/jsonobject"
	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/codex/wire"
	"one-api/types"
)

func (CodexProviderFactory) AssessChatRemoteMedia(_ *model.Channel, _ *types.ChatCompletionRequest, _ base.ChatRemoteMediaSummary) (base.RemoteMediaMode, error) {
	if err := base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions"); err != nil {
		return base.RemoteMediaReject, err
	}
	return base.RemoteMediaPassURL, nil
}

func (CodexProviderFactory) AssessChatRequest(_ *model.Channel, _ string, _ *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	if err := base.RequireOperationEndpoint(getConfig().ChatCompletions, "Chat Completions"); err != nil {
		return base.ChatRequestSupport{}, err
	}
	return base.ChatRequestSupport{UsesResponsesTransport: true}, nil
}

func (CodexProviderFactory) ResponsesSupport(_ *model.Channel) base.OperationSupport {
	support := base.OperationSupport{Operations: map[base.Operation]base.DataPath{}}
	if getConfig().Responses != "" {
		support.Operations[base.OperationResponsesCreate] = base.DataPathSameDialect
		support.Operations[base.OperationResponsesCompact] = base.DataPathSameDialect
		support.Operations[base.OperationResponsesWebSocket] = base.DataPathSameDialect
	}
	return support
}

func (CodexProviderFactory) ValidateResponsesRequest(_ *model.Channel, operation base.Operation, fields map[string]json.RawMessage, _ string, _ bool) error {
	object := &jsonobject.Object{Fields: fields}
	var err error
	switch operation {
	case base.OperationResponsesCreate, base.OperationResponsesWebSocket:
		err = wire.ValidateResponsesCreateBody(object)
	case base.OperationResponsesCompact:
		err = wire.ValidateResponsesCompactBody(object)
	default:
		return nil
	}
	if err == nil {
		return nil
	}
	if violation, ok := err.(*wire.Violation); ok {
		return &base.RequestCapabilityError{Param: violation.Param, Message: violation.Message}
	}
	return err
}
