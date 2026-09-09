package base

import (
	"fmt"
	"strings"

	"one-api/common/providerresponse"
	"one-api/types"
)

// Operation identifies one public OpenAI operation. Adapter support is tracked
// per operation because lifecycle, transport, and wire compatibility differ.
type Operation = providerresponse.Operation

const (
	OperationResponsesCreate      = providerresponse.OperationResponsesCreate
	OperationResponsesCompact     = providerresponse.OperationResponsesCompact
	OperationResponsesInputTokens = providerresponse.OperationResponsesInputTokens
	OperationResponsesRetrieve    = providerresponse.OperationResponsesRetrieve
	OperationResponsesDelete      = providerresponse.OperationResponsesDelete
	OperationResponsesInputItems  = providerresponse.OperationResponsesInputItems
	OperationResponsesWebSocket   = providerresponse.OperationResponsesWebSocket
	OperationAudioTranscription   = providerresponse.OperationAudioTranscription
	OperationAudioTranslation     = providerresponse.OperationAudioTranslation
)

type DataPath = providerresponse.DataPath

const (
	DataPathExactWire     = providerresponse.DataPathExactWire
	DataPathSameDialect   = providerresponse.DataPathSameDialect
	DataPathCrossProtocol = providerresponse.DataPathCrossProtocol
)

type OperationSupport struct {
	Operations map[Operation]DataPath
}

// RequestCapabilityError describes a provider adapter's inability to
// represent one request field. Routing consumes it before provider work.
type RequestCapabilityError struct {
	Param   string
	Message string
}

func (e *RequestCapabilityError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// ChatRequestSupport carries the one execution-path fact the relay needs after
// provider-local candidate assessment. UsesResponsesTransport means the Chat
// adapter itself converts the request to Responses before provider I/O.
type ChatRequestSupport struct {
	UsesResponsesTransport bool
}

// RequireOperationEndpoint keeps factory capability registration tied to the
// same ProviderConfig field used by Create. It performs no provider work.
func RequireOperationEndpoint(endpoint, operation string) error {
	if strings.TrimSpace(endpoint) != "" {
		return nil
	}
	return &RequestCapabilityError{
		Param:   "operation",
		Message: fmt.Sprintf("selected channel does not implement %s", operation),
	}
}

func ValidateOnlyChatRequest(onlyChat bool, request *types.ChatCompletionRequest) error {
	if onlyChat && request != nil && len(request.Tools) > 0 {
		return &RequestCapabilityError{Param: "tools", Message: "channel only supports requests without tools"}
	}
	return nil
}

// ValidateFunctionOnlyChatRequest is the shared leaf validator for adapters
// whose provider dialect supports function tools but not the wider OpenAI Chat
// union. Dialect-specific output and tool-choice semantics stay provider-local.
func ValidateFunctionOnlyChatRequest(request *types.ChatCompletionRequest, adapter string) error {
	if request == nil {
		return &RequestCapabilityError{Param: "request", Message: "chat request is required"}
	}
	if request.N != nil && *request.N != 1 {
		return &RequestCapabilityError{Param: "n", Message: fmt.Sprintf("n cannot be represented by the %s adapter", adapter)}
	}
	for index, tool := range request.Tools {
		if tool == nil {
			return &RequestCapabilityError{Param: "tools", Message: fmt.Sprintf("tools[%d] cannot be represented by the %s adapter", index, adapter)}
		}
		if strings.TrimSpace(tool.Type) != types.ToolChoiceTypeFunction {
			return &RequestCapabilityError{Param: "tools", Message: fmt.Sprintf("tools[%d] cannot be represented by the %s adapter without changing its type", index, adapter)}
		}
	}
	for index, function := range request.Functions {
		if function == nil {
			return &RequestCapabilityError{Param: "functions", Message: fmt.Sprintf("functions[%d] cannot be represented by the %s adapter", index, adapter)}
		}
	}
	for messageIndex, message := range request.Messages {
		for partIndex, part := range message.ParseContent() {
			switch strings.TrimSpace(part.Type) {
			case types.ContentTypeText, types.ContentTypeImageURL:
			default:
				return &RequestCapabilityError{Param: "messages", Message: fmt.Sprintf("messages[%d].content[%d] cannot be represented by the %s adapter", messageIndex, partIndex, adapter)}
			}
		}
		for callIndex, call := range message.ToolCalls {
			callType := ""
			if call != nil {
				callType = strings.TrimSpace(call.Type)
			}
			if call == nil || call.Function == nil || (callType != "" && callType != types.ToolChoiceTypeFunction) || call.Custom != nil {
				return &RequestCapabilityError{Param: "messages", Message: fmt.Sprintf("messages[%d].tool_calls[%d] cannot be represented by the %s adapter", messageIndex, callIndex, adapter)}
			}
		}
	}
	if request.ParallelToolCalls != nil {
		return &RequestCapabilityError{Param: "parallel_tool_calls", Message: fmt.Sprintf("parallel_tool_calls cannot be represented by the %s adapter", adapter)}
	}
	return nil
}

func (c OperationSupport) Supports(operation Operation) bool {
	_, ok := c.Operations[operation]
	return ok
}

func (c OperationSupport) DataPath(operation Operation) (DataPath, bool) {
	path, ok := c.Operations[operation]
	return path, ok
}

func (c OperationSupport) SupportsStoredResponses() bool {
	for _, operation := range []Operation{
		OperationResponsesCreate,
		OperationResponsesRetrieve,
		OperationResponsesDelete,
		OperationResponsesInputItems,
	} {
		if !c.Supports(operation) {
			return false
		}
	}
	return true
}
