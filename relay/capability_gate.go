package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	commonresponses "one-api/common/responses"
	"one-api/model"
	"one-api/providers"
	providersBase "one-api/providers/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

const unsupportedCapabilityCode = "unsupported_capability"

type capabilityGateError struct {
	message string
	param   string
	status  int
}

func (e *capabilityGateError) Error() string { return e.message }

func newCapabilityGateError(param, message string) error {
	return &capabilityGateError{message: message, param: param, status: http.StatusBadRequest}
}

func providerCapabilityGateError(err error) error {
	if err == nil {
		return nil
	}
	var requestErr *providersBase.RequestCapabilityError
	if !errors.As(err, &requestErr) {
		return newCapabilityGateError("request", err.Error())
	}
	status := http.StatusBadRequest
	if requestErr.Param == "operation" {
		status = http.StatusServiceUnavailable
	}
	return &capabilityGateError{message: requestErr.Message, param: requestErr.Param, status: status}
}

func capabilityGateAPIError(err error) *types.OpenAIErrorWithStatusCode {
	var gateErr *capabilityGateError
	if !errors.As(err, &gateErr) {
		return nil
	}
	status := gateErr.status
	if status == 0 {
		status = http.StatusBadRequest
	}
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Message: gateErr.message,
			Type:    "invalid_request_error",
			Param:   gateErr.param,
			Code:    unsupportedCapabilityCode,
		},
		StatusCode: status,
		LocalError: true,
	}
}

func UnsupportedCapability(param, message string) gin.HandlerFunc {
	return func(c *gin.Context) {
		relayResponseWithOpenAIErr(c, capabilityGateAPIError(newCapabilityGateError(param, message)))
	}
}

// UnsupportedCapabilityUnlessSpecifiedChannel preserves the administrator-only
// raw relay escape hatch while giving ordinary clients a stable capability
// error before any provider work begins.
func UnsupportedCapabilityUnlessSpecifiedChannel(param, message string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetInt("specific_channel_id") > 0 {
			c.Set("specific_channel_id_ignore", false)
			RelayOnly(c)
			return
		}
		relayResponseWithOpenAIErr(c, capabilityGateAPIError(newCapabilityGateError(param, message)))
	}
}

func validateChatSupportedSurface(request *types.ChatCompletionRequest, raw map[string]json.RawMessage) error {
	if request != nil && request.Store != nil && *request.Store {
		return newCapabilityGateError("store", "store=true is not supported for chat completions because Stored Chat lifecycle is unavailable")
	}
	if messages, ok := raw["messages"]; ok {
		if field, found := commonresponses.FindAccountScopedResourceReferenceJSON(messages); found {
			return newCapabilityGateError("messages", fmt.Sprintf("chat resource reference %s is not supported", field))
		}
	}
	if tools, ok := raw["tools"]; ok {
		if err := validateResponsesToolsSurface(tools); err != nil {
			return err
		}
	}
	return nil
}

func validateChatToResponsesRepresentability(request *types.ChatCompletionRequest, fields map[string]json.RawMessage) error {
	if request == nil {
		return newCapabilityGateError("request", "chat request cannot be represented as a Responses request")
	}
	if request.N != nil && *request.N != 1 {
		return newCapabilityGateError("n", "the selected channel cannot preserve multiple Chat choices")
	}
	if len(request.Functions) > 0 || request.FunctionCall != nil {
		return newCapabilityGateError("functions", "legacy function fields cannot be represented by the selected Responses adapter")
	}
	if request.Reasoning != nil && request.Reasoning.MaxTokens != 0 {
		return newCapabilityGateError("reasoning", "reasoning.max_tokens cannot be represented by the selected Responses adapter")
	}
	allowedFields := map[string]struct{}{
		"model": {}, "messages": {}, "max_tokens": {}, "max_completion_tokens": {},
		"temperature": {}, "top_p": {}, "n": {}, "stream": {}, "stream_options": {},
		"response_format": {}, "tools": {}, "tool_choice": {}, "parallel_tool_calls": {},
		"reasoning_effort": {}, "reasoning": {}, "verbosity": {}, "store": {},
		"instructions": {}, "service_tier": {}, "processing_class": {},
		"safety_identifier": {}, "prompt_cache_key": {},
	}
	for field, raw := range fields {
		if _, ok := allowedFields[field]; !ok && !isJSONNull(raw) {
			return newCapabilityGateError(field, fmt.Sprintf("%s cannot be represented by the selected Responses adapter", field))
		}
	}
	if raw, ok := fields["prompt_cache_key"]; ok && !isJSONNull(raw) {
		var key string
		if err := json.Unmarshal(raw, &key); err != nil {
			return newCapabilityGateError("prompt_cache_key", "prompt_cache_key cannot be represented by the selected Responses adapter")
		}
	}
	if err := validateChatReasoningToResponsesRepresentability(fields["reasoning"]); err != nil {
		return err
	}
	if raw, ok := fields["stream_options"]; ok && !isJSONNull(raw) {
		var streamOptions map[string]json.RawMessage
		if err := json.Unmarshal(raw, &streamOptions); err != nil {
			return newCapabilityGateError("stream_options", "stream_options cannot be represented by the selected Responses adapter")
		}
		if field, ok := firstMeaningfulUnexpectedField(streamOptions, map[string]struct{}{"include_usage": {}}); ok {
			return newCapabilityGateError("stream_options", fmt.Sprintf("stream_options.%s cannot be represented by the selected Responses adapter", field))
		}
	}
	if err := validateChatMessagesToResponsesRepresentability(fields["messages"]); err != nil {
		return err
	}
	for index, tool := range request.Tools {
		if tool == nil {
			continue
		}
		switch strings.TrimSpace(tool.Type) {
		case "function", "custom":
		default:
			return newCapabilityGateError("tools", fmt.Sprintf("tools[%d].type cannot be represented by the selected Responses adapter", index))
		}
	}
	for messageIndex, message := range request.Messages {
		if message.Name != nil || message.FunctionCall != nil || message.Audio != nil || message.Annotations != nil || message.CacheControl != nil ||
			message.Refusal != "" || message.ReasoningContent != "" || message.Reasoning != "" || len(message.Image) > 0 || len(message.Images) > 0 {
			return newCapabilityGateError("messages", fmt.Sprintf("messages[%d] contains fields that cannot be represented by the selected Responses adapter", messageIndex))
		}
		for callIndex, call := range message.ToolCalls {
			if call == nil {
				return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].tool_calls[%d] cannot be represented by the selected Responses adapter", messageIndex, callIndex))
			}
			if call.Index < 0 {
				return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].tool_calls[%d].index cannot be represented by the selected Responses adapter", messageIndex, callIndex))
			}
			switch strings.TrimSpace(call.Type) {
			case "", types.ToolChoiceTypeFunction:
				if call.Function == nil {
					return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].tool_calls[%d].function is required", messageIndex, callIndex))
				}
			case types.ToolChoiceTypeCustom:
				if call.Custom == nil {
					return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].tool_calls[%d].custom is required", messageIndex, callIndex))
				}
			default:
				return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].tool_calls[%d].type cannot be represented by the selected Responses adapter", messageIndex, callIndex))
			}
		}
		for partIndex, part := range message.ParseContent() {
			if message.ToolCallID != "" && strings.TrimSpace(part.Type) != types.ContentTypeText {
				return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].content[%d].type cannot be represented as a Responses tool output", messageIndex, partIndex))
			}
			switch strings.TrimSpace(part.Type) {
			case types.ContentTypeText, types.ContentTypeImageURL:
			case types.ContentTypeFile:
				if part.File == nil || strings.TrimSpace(part.File.FileData) == "" {
					return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].content[%d] requires inline file_data", messageIndex, partIndex))
				}
			default:
				return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].content[%d].type cannot be represented by the selected Responses adapter", messageIndex, partIndex))
			}
		}
	}
	return nil
}

func validateChatReasoningToResponsesRepresentability(raw json.RawMessage) error {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil
	}
	var reasoning map[string]json.RawMessage
	if err := json.Unmarshal(raw, &reasoning); err != nil {
		return newCapabilityGateError("reasoning", "reasoning cannot be represented by the selected Responses adapter")
	}
	if field, ok := firstMeaningfulUnexpectedField(reasoning, map[string]struct{}{"effort": {}, "summary": {}}); ok {
		return newCapabilityGateError("reasoning", fmt.Sprintf("reasoning.%s cannot be represented by the selected Responses adapter", field))
	}
	return nil
}

func validateChatMessagesToResponsesRepresentability(raw json.RawMessage) error {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return newCapabilityGateError("messages", "messages cannot be represented by the selected Responses adapter")
	}
	allowedMessageFields := map[string]struct{}{"role": {}, "content": {}, "tool_calls": {}, "tool_call_id": {}}
	for index, message := range messages {
		if field, ok := firstMeaningfulUnexpectedField(message, allowedMessageFields); ok {
			return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].%s cannot be represented by the selected Responses adapter", index, field))
		}
		if err := validateChatContentToResponsesRepresentability(index, message["content"]); err != nil {
			return err
		}
		if err := validateChatToolCallsToResponsesRepresentability(index, message["tool_calls"]); err != nil {
			return err
		}
	}
	return nil
}

func validateChatContentToResponsesRepresentability(messageIndex int, raw json.RawMessage) error {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].content cannot be represented by the selected Responses adapter", messageIndex))
	}
	for partIndex, part := range parts {
		var partType string
		if json.Unmarshal(part["type"], &partType) != nil {
			return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].content[%d].type is required", messageIndex, partIndex))
		}
		var allowed map[string]struct{}
		switch strings.TrimSpace(partType) {
		case types.ContentTypeText:
			allowed = map[string]struct{}{"type": {}, "text": {}}
		case types.ContentTypeImageURL:
			allowed = map[string]struct{}{"type": {}, "image_url": {}}
			if err := validateChatNestedContentObject(messageIndex, partIndex, part["image_url"], "image_url", map[string]struct{}{"url": {}, "detail": {}}); err != nil {
				return err
			}
		case types.ContentTypeFile:
			allowed = map[string]struct{}{"type": {}, "file": {}}
			if err := validateChatNestedContentObject(messageIndex, partIndex, part["file"], "file", map[string]struct{}{"filename": {}, "file_data": {}, "file_id": {}}); err != nil {
				return err
			}
		default:
			return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].content[%d].type cannot be represented by the selected Responses adapter", messageIndex, partIndex))
		}
		if field, ok := firstMeaningfulUnexpectedField(part, allowed); ok {
			return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].content[%d].%s cannot be represented by the selected Responses adapter", messageIndex, partIndex, field))
		}
	}
	return nil
}

func validateChatNestedContentObject(messageIndex, partIndex int, raw json.RawMessage, field string, allowed map[string]struct{}) error {
	var object map[string]json.RawMessage
	if len(raw) == 0 || isJSONNull(raw) || json.Unmarshal(raw, &object) != nil {
		return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].content[%d].%s cannot be represented by the selected Responses adapter", messageIndex, partIndex, field))
	}
	if nested, ok := firstMeaningfulUnexpectedField(object, allowed); ok {
		return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].content[%d].%s.%s cannot be represented by the selected Responses adapter", messageIndex, partIndex, field, nested))
	}
	return nil
}

func validateChatToolCallsToResponsesRepresentability(messageIndex int, raw json.RawMessage) error {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil
	}
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &calls); err != nil {
		return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].tool_calls cannot be represented by the selected Responses adapter", messageIndex))
	}
	for callIndex, call := range calls {
		if field, ok := firstMeaningfulUnexpectedField(call, map[string]struct{}{"id": {}, "type": {}, "index": {}, "function": {}, "custom": {}}); ok {
			return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].tool_calls[%d].%s cannot be represented by the selected Responses adapter", messageIndex, callIndex, field))
		}
		if rawIndex, ok := call["index"]; ok && !isJSONNull(rawIndex) {
			var indexValue int
			if err := json.Unmarshal(rawIndex, &indexValue); err != nil || indexValue < 0 {
				return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].tool_calls[%d].index cannot be represented by the selected Responses adapter", messageIndex, callIndex))
			}
		}
		for field, allowed := range map[string]map[string]struct{}{
			"function": {"name": {}, "arguments": {}},
			"custom":   {"name": {}, "input": {}},
		} {
			nestedRaw, ok := call[field]
			if !ok || isJSONNull(nestedRaw) {
				continue
			}
			var nested map[string]json.RawMessage
			if json.Unmarshal(nestedRaw, &nested) != nil {
				return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].tool_calls[%d].%s cannot be represented by the selected Responses adapter", messageIndex, callIndex, field))
			}
			if nestedField, ok := firstMeaningfulUnexpectedField(nested, allowed); ok {
				return newCapabilityGateError("messages", fmt.Sprintf("messages[%d].tool_calls[%d].%s.%s cannot be represented by the selected Responses adapter", messageIndex, callIndex, field, nestedField))
			}
		}
	}
	return nil
}

func validateResponsesToChatRepresentability(request *types.OpenAIResponsesRequest, fields map[string]json.RawMessage) error {
	if request == nil {
		return newCapabilityGateError("request", "responses request cannot be represented as a Chat Completions request")
	}
	if fields == nil {
		encoded, err := json.Marshal(request)
		if err != nil {
			return newCapabilityGateError("request", "responses request cannot be represented as a Chat Completions request")
		}
		if err := json.Unmarshal(encoded, &fields); err != nil {
			return newCapabilityGateError("request", "responses request cannot be represented as a Chat Completions request")
		}
	}
	allowedFields := map[string]struct{}{
		"model": {}, "input": {}, "instructions": {}, "max_output_tokens": {},
		"parallel_tool_calls": {}, "reasoning": {}, "safety_identifier": {},
		"service_tier": {}, "processing_class": {}, "store": {}, "stream": {},
		"temperature": {}, "text": {}, "tool_choice": {}, "tools": {}, "top_p": {},
	}
	for field, raw := range fields {
		if _, ok := allowedFields[field]; !ok && !isJSONNull(raw) {
			return newCapabilityGateError(field, fmt.Sprintf("%s cannot be represented by the selected Chat Completions adapter", field))
		}
	}
	if err := validateResponsesReasoningToChatRepresentability(fields["reasoning"]); err != nil {
		return err
	}
	if err := validateResponsesTextToChatRepresentability(fields["text"]); err != nil {
		return err
	}
	if err := validateResponsesToolsToChatRepresentability(fields["tools"]); err != nil {
		return err
	}
	if err := validateResponsesToolChoiceToChatRepresentability(fields["tool_choice"]); err != nil {
		return err
	}
	for index, tool := range request.Tools {
		if strings.TrimSpace(tool.Type) != "function" {
			return newCapabilityGateError("tools", fmt.Sprintf("tools[%d].type cannot be represented by the selected Chat Completions adapter", index))
		}
	}
	if request.Stream && len(request.Tools) > 0 && (request.ParallelToolCalls == nil || *request.ParallelToolCalls) {
		return newCapabilityGateError("parallel_tool_calls", "function tools require parallel_tool_calls=false for the selected Chat Completions adapter")
	}
	if err := validateResponsesInputToChatRepresentability(request.Input); err != nil {
		return err
	}
	return nil
}

func validateResponsesReasoningToChatRepresentability(raw json.RawMessage) error {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil
	}
	return validateResponsesObjectFields(
		raw,
		map[string]struct{}{"effort": {}},
		"reasoning",
		"reasoning",
	)
}

func validateResponsesTextToChatRepresentability(raw json.RawMessage) error {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil
	}
	var text map[string]json.RawMessage
	if err := json.Unmarshal(raw, &text); err != nil {
		return newCapabilityGateError("text", "text cannot be represented by the selected Chat Completions adapter")
	}
	if field, ok := firstMeaningfulUnexpectedField(text, map[string]struct{}{"format": {}, "verbosity": {}}); ok {
		return newCapabilityGateError("text", fmt.Sprintf("text.%s cannot be represented by the selected Chat Completions adapter", field))
	}
	format, ok := text["format"]
	if !ok || isJSONNull(format) {
		return nil
	}
	return validateResponsesObjectFields(
		format,
		map[string]struct{}{"type": {}, "name": {}, "schema": {}, "description": {}, "strict": {}},
		"text",
		"text.format",
	)
}

func validateResponsesToolsToChatRepresentability(raw json.RawMessage) error {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return newCapabilityGateError("tools", "tools cannot be represented by the selected Chat Completions adapter")
	}
	allowed := map[string]struct{}{
		"type": {}, "name": {}, "description": {}, "parameters": {}, "strict": {},
	}
	for index, tool := range tools {
		if field, ok := firstMeaningfulUnexpectedField(tool, allowed); ok {
			return newCapabilityGateError("tools", fmt.Sprintf("tools[%d].%s cannot be represented by the selected Chat Completions adapter", index, field))
		}
	}
	return nil
}

func validateResponsesToolChoiceToChatRepresentability(raw json.RawMessage) error {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil
	}
	var stringChoice string
	if json.Unmarshal(raw, &stringChoice) == nil {
		return nil
	}
	var choice map[string]json.RawMessage
	if err := json.Unmarshal(raw, &choice); err != nil {
		return newCapabilityGateError("tool_choice", "tool_choice cannot be represented by the selected Chat Completions adapter")
	}
	if field, ok := firstMeaningfulUnexpectedField(choice, map[string]struct{}{"type": {}, "name": {}}); ok {
		return newCapabilityGateError("tool_choice", fmt.Sprintf("tool_choice.%s cannot be represented by the selected Chat Completions adapter", field))
	}
	var choiceType string
	var name string
	if json.Unmarshal(choice["type"], &choiceType) != nil || strings.TrimSpace(choiceType) != "function" ||
		json.Unmarshal(choice["name"], &name) != nil || strings.TrimSpace(name) == "" {
		return newCapabilityGateError("tool_choice", "only a named function tool choice can be represented by the selected Chat Completions adapter")
	}
	return nil
}

func validateResponsesObjectFields(raw json.RawMessage, allowed map[string]struct{}, param, path string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return newCapabilityGateError(param, fmt.Sprintf("%s cannot be represented by the selected Chat Completions adapter", path))
	}
	if field, ok := firstMeaningfulUnexpectedField(object, allowed); ok {
		return newCapabilityGateError(param, fmt.Sprintf("%s.%s cannot be represented by the selected Chat Completions adapter", path, field))
	}
	return nil
}

func validateResponsesInputToChatRepresentability(input any) error {
	if input == nil {
		return nil
	}
	if _, ok := input.(string); ok {
		return nil
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return newCapabilityGateError("input", "input cannot be represented by the selected Chat Completions adapter")
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return newCapabilityGateError("input", "input must be a string or an array of representable input items")
	}
	for index, item := range items {
		var itemType string
		if value, ok := item["type"]; ok && !isJSONNull(value) {
			if err := json.Unmarshal(value, &itemType); err != nil {
				return newCapabilityGateError("input", fmt.Sprintf("input[%d].type must be a string", index))
			}
		}
		itemType = strings.TrimSpace(itemType)
		switch itemType {
		case "", types.InputTypeMessage:
			if err := validateResponsesMessageItemToChatRepresentability(index, item); err != nil {
				return err
			}
		case types.InputTypeFunctionCall, types.InputTypeFunctionCallOutput,
			types.InputTypeCustomToolCall, types.InputTypeCustomToolCallOutput:
			if err := validateResponsesItemMetadata(index, item); err != nil {
				return err
			}
			if field, ok := firstMeaningfulUnexpectedField(item, responsesChatInputItemFields[itemType]); ok {
				return newCapabilityGateError("input", fmt.Sprintf("input[%d].%s cannot be represented by the selected Chat Completions adapter", index, field))
			}
			if itemType == types.InputTypeFunctionCallOutput || itemType == types.InputTypeCustomToolCallOutput {
				if err := validateResponsesToolOutputToChatRepresentability(index, item); err != nil {
					return err
				}
			}
			if itemType == types.InputTypeCustomToolCall {
				var input string
				if value, ok := item["input"]; !ok || isJSONNull(value) || json.Unmarshal(value, &input) != nil {
					return newCapabilityGateError("input", fmt.Sprintf("input[%d].input must be a string", index))
				}
			}
		default:
			return newCapabilityGateError("input", fmt.Sprintf("input[%d].type cannot be represented by the selected Chat Completions adapter", index))
		}
	}
	return nil
}

var responsesChatInputItemFields = map[string]map[string]struct{}{
	types.InputTypeFunctionCall:         {"type": {}, "id": {}, "status": {}, "call_id": {}, "name": {}, "arguments": {}},
	types.InputTypeFunctionCallOutput:   {"type": {}, "id": {}, "status": {}, "call_id": {}, "output": {}},
	types.InputTypeCustomToolCall:       {"type": {}, "id": {}, "status": {}, "call_id": {}, "name": {}, "input": {}},
	types.InputTypeCustomToolCallOutput: {"type": {}, "id": {}, "status": {}, "call_id": {}, "output": {}},
}

func validateResponsesToolOutputToChatRepresentability(index int, item map[string]json.RawMessage) error {
	output, ok := item["output"]
	if !ok || isJSONNull(output) {
		return newCapabilityGateError("input", fmt.Sprintf("input[%d].output is required", index))
	}
	var text string
	if json.Unmarshal(output, &text) == nil {
		return nil
	}

	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(output, &parts); err != nil || len(parts) == 0 {
		return newCapabilityGateError("input", fmt.Sprintf("input[%d].output must be a string or a non-empty input_text array", index))
	}
	allowed := map[string]struct{}{"type": {}, "text": {}}
	for partIndex, part := range parts {
		var partType string
		if value, ok := part["type"]; !ok || isJSONNull(value) || json.Unmarshal(value, &partType) != nil || strings.TrimSpace(partType) != types.ContentTypeInputText {
			return newCapabilityGateError("input", fmt.Sprintf("input[%d].output[%d].type cannot be represented by the selected Chat Completions adapter", index, partIndex))
		}
		if field, ok := firstMeaningfulUnexpectedField(part, allowed); ok {
			return newCapabilityGateError("input", fmt.Sprintf("input[%d].output[%d].%s cannot be represented by the selected Chat Completions adapter", index, partIndex, field))
		}
	}
	return nil
}

func validateResponsesMessageItemToChatRepresentability(index int, item map[string]json.RawMessage) error {
	allowed := map[string]struct{}{"type": {}, "id": {}, "status": {}, "role": {}, "content": {}}
	if field, ok := firstMeaningfulUnexpectedField(item, allowed); ok {
		return newCapabilityGateError("input", fmt.Sprintf("input[%d].%s cannot be represented by the selected Chat Completions adapter", index, field))
	}
	if err := validateResponsesItemMetadata(index, item); err != nil {
		return err
	}
	content, ok := item["content"]
	if !ok || isJSONNull(content) {
		return nil
	}
	var text string
	if json.Unmarshal(content, &text) == nil {
		return nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil {
		return newCapabilityGateError("input", fmt.Sprintf("input[%d].content cannot be represented by the selected Chat Completions adapter", index))
	}
	for partIndex, part := range parts {
		var partType string
		if value, ok := part["type"]; ok && !isJSONNull(value) {
			if err := json.Unmarshal(value, &partType); err != nil {
				return newCapabilityGateError("input", fmt.Sprintf("input[%d].content[%d].type must be a string", index, partIndex))
			}
		}
		allowed := responsesChatContentPartFields[strings.TrimSpace(partType)]
		if allowed == nil {
			return newCapabilityGateError("input", fmt.Sprintf("input[%d].content[%d].type cannot be represented by the selected Chat Completions adapter", index, partIndex))
		}
		if field, ok := firstMeaningfulUnexpectedField(part, allowed); ok {
			return newCapabilityGateError("input", fmt.Sprintf("input[%d].content[%d].%s cannot be represented by the selected Chat Completions adapter", index, partIndex, field))
		}
	}
	return nil
}

// validateResponsesItemMetadata validates only the structure of metadata that
// may be carried by a Responses output item. Chat history does not need to
// carry these values downstream, but rejecting them would make the proxy's own
// output unusable as the next request's input.
func validateResponsesItemMetadata(index int, item map[string]json.RawMessage) error {
	if rawID, ok := item["id"]; ok && !isJSONNull(rawID) {
		var id string
		if err := json.Unmarshal(rawID, &id); err != nil {
			return newCapabilityGateError("input", fmt.Sprintf("input[%d].id cannot be represented by the selected Chat Completions adapter", index))
		}
	}
	if rawStatus, ok := item["status"]; ok && !isJSONNull(rawStatus) {
		var status string
		if err := json.Unmarshal(rawStatus, &status); err != nil {
			return newCapabilityGateError("input", fmt.Sprintf("input[%d].status cannot be represented by the selected Chat Completions adapter", index))
		}
	}
	return nil
}

var responsesChatContentPartFields = map[string]map[string]struct{}{
	types.ContentTypeInputText:  {"type": {}, "text": {}},
	types.ContentTypeOutputText: {"type": {}, "text": {}},
	types.ContentTypeInputImage: {"type": {}, "image_url": {}, "detail": {}},
	types.ContentTypeInputFile:  {"type": {}, "file_data": {}, "filename": {}},
}

func firstMeaningfulUnexpectedField(fields map[string]json.RawMessage, allowed map[string]struct{}) (string, bool) {
	for field, raw := range fields {
		if _, ok := allowed[field]; !ok && !isJSONNull(raw) {
			return field, true
		}
	}
	return "", false
}

func validateResponsesSupportedSurface(request *types.OpenAIResponsesRequest, raw map[string]json.RawMessage, operation responsesOperation) error {
	if request == nil {
		return nil
	}
	if request.Background != nil && *request.Background {
		return newCapabilityGateError("background", "background responses are not supported")
	}
	if hasMeaningfulResponsesConversation(request.Conversation) {
		return newCapabilityGateError("conversation", "conversation resources are not supported")
	}
	if meaningfulJSONValue(request.Prompt) {
		return newCapabilityGateError("prompt", "saved prompt resources are not supported; inline the saved prompt content in instructions or input before the prompt resource API closes on 2026-11-30")
	}
	if operation == responsesOperationCompact {
		if value, ok := raw["multi_agent"]; ok && !isJSONNull(value) {
			return newCapabilityGateError("multi_agent", "multi_agent is not supported by responses compact")
		}
	}
	if value, ok := raw["tools"]; ok {
		if err := validateResponsesToolsSurface(value); err != nil {
			return err
		}
	}
	if value, ok := raw["input"]; ok {
		if field, found := commonresponses.FindAccountScopedResourceReferenceJSON(value); found {
			return newCapabilityGateError("input", fmt.Sprintf("input resource reference %s is not supported", field))
		}
	}
	return nil
}

func meaningfulJSONValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case map[string]any:
		return len(typed) > 0
	case []any:
		return len(typed) > 0
	default:
		return true
	}
}

func isJSONNull(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func validateResponsesToolsSurface(raw json.RawMessage) error {
	// Tool schemas and discriminated unions belong to the selected upstream on
	// same-dialect/exact-wire paths. This pre-selection gate only rejects resources
	// whose ownership/lifecycle the proxy cannot safely represent.
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		tools = []json.RawMessage{raw}
	}
	for _, rawTool := range tools {
		var tool map[string]json.RawMessage
		if err := json.Unmarshal(rawTool, &tool); err != nil {
			continue
		}
		if resource, unsupported := unsupportedHostedContainerTool(tool); unsupported {
			return newCapabilityGateError("tools", fmt.Sprintf("tools[] requires unsupported hosted container resource %s", resource))
		}
		for _, nestedField := range []string{"environment", "input_image_mask"} {
			if value, ok := tool[nestedField]; ok && !isJSONNull(value) {
				if field, found := commonresponses.FindAccountScopedResourceReferenceJSON(value); found {
					return newCapabilityGateError("tools", fmt.Sprintf("tools[].%s resource reference %s is not supported", nestedField, field))
				}
			}
		}
		for _, field := range []string{"vector_store_ids", "container", "container_id", "file_id", "skill_reference"} {
			if value, ok := tool[field]; ok && !isJSONNull(value) {
				return newCapabilityGateError("tools", fmt.Sprintf("tools[].%s resource reference is not supported", field))
			}
		}
	}
	return nil
}

func unsupportedHostedContainerTool(tool map[string]json.RawMessage) (string, bool) {
	var toolType string
	if json.Unmarshal(tool["type"], &toolType) != nil {
		return "", false
	}
	switch strings.TrimSpace(toolType) {
	case types.APIToolTypeCodeInterpreter:
		return types.APIToolTypeCodeInterpreter, true
	case types.APIToolTypeShell:
		var environment map[string]json.RawMessage
		if json.Unmarshal(tool["environment"], &environment) != nil {
			return "", false
		}
		var environmentType string
		if json.Unmarshal(environment["type"], &environmentType) != nil {
			return "", false
		}
		switch strings.TrimSpace(environmentType) {
		case "container_auto", "container_reference":
			return environmentType, true
		}
	}
	return "", false
}

func requireChatChannelCompatibility(modelName string, request *types.ChatCompletionRequest, fields map[string]json.RawMessage) requestChannelCapability {
	return func(channel *model.Channel) error {
		canonicalModel, err := mappedModelForChannel(channel, modelName)
		if err != nil {
			return &capabilityGateError{message: "channel has invalid model mapping configuration", status: http.StatusServiceUnavailable}
		}
		effectiveRequest, effectiveFields, err := effectiveChatRequestForChannel(channel, canonicalModel, request, fields)
		if err != nil {
			return &capabilityGateError{message: "channel has invalid request transform configuration", status: http.StatusServiceUnavailable}
		}
		if err := validateChatSupportedSurface(effectiveRequest, effectiveFields); err != nil {
			return err
		}
		if err := validateOnlyChatEffectiveRequest(channel, effectiveRequest); err != nil {
			return err
		}
		usesResponsesTransport := chatModelRequiresResponses(canonicalModel)
		if !usesResponsesTransport {
			chatSupport, err := providers.AssessChatRequest(channel, canonicalModel, effectiveRequest, effectiveFields)
			if err != nil {
				return providerCapabilityGateError(err)
			}
			usesResponsesTransport = chatSupport.UsesResponsesTransport
		}
		if usesResponsesTransport {
			path, ok := providers.ResolveAdapterSupport(channel).DataPath(providersBase.OperationResponsesCreate)
			if !ok || path == providersBase.DataPathCrossProtocol {
				return &capabilityGateError{message: "channel adapter cannot send this model through the Responses API", status: http.StatusServiceUnavailable}
			}
			if err := validateChatToResponsesRepresentability(effectiveRequest, effectiveFields); err != nil {
				return err
			}
			if err := validateChatResponsesTarget(channel, canonicalModel, effectiveRequest); err != nil {
				return err
			}
		}
		_, _, err = providers.AssessChatRemoteMedia(channel, effectiveRequest)
		return remoteMediaCapabilityGateError(err)
	}
}

func validateChatResponsesTarget(channel *model.Channel, modelName string, request *types.ChatCompletionRequest) error {
	converted := request.ToResponsesRequest()
	converted.Model = modelName
	body, err := json.Marshal(converted)
	if err != nil {
		return newCapabilityGateError("request", "chat request cannot be represented as a Responses request")
	}
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(body, &fields); err != nil {
		return newCapabilityGateError("request", "chat request cannot be represented as a Responses request")
	}
	if err := providers.ValidateResponsesRequest(channel, providersBase.OperationResponsesCreate, fields, modelName, false); err != nil {
		return providerCapabilityGateError(err)
	}
	return nil
}

func effectiveChatRequestForChannel(channel *model.Channel, canonicalModel string, request *types.ChatCompletionRequest, fields map[string]json.RawMessage) (*types.ChatCompletionRequest, map[string]json.RawMessage, error) {
	if request == nil {
		return nil, nil, errors.New("chat request is required")
	}
	body := make(map[string]interface{}, len(fields))
	for name, raw := range fields {
		var value interface{}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, nil, err
		}
		body[name] = value
	}
	if channel != nil {
		customParams, err := channel.GetCustomParameterMap()
		if err != nil {
			return nil, nil, err
		}
		preAdd, _ := customParams["pre_add"].(bool)
		if preAdd {
			body = providersBase.ApplyCustomParams(body, customParams, canonicalModel, true)
		}
	}
	// Candidate assessment must see the same mapped model that the selected
	// provider receives. The relay owns this field, so a channel transform may
	// not leave the public alias in the effective provider request.
	body["model"] = canonicalModel
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	effective := &types.ChatCompletionRequest{}
	if err := json.Unmarshal(encoded, effective); err != nil {
		return nil, nil, err
	}
	effectiveFields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(encoded, &effectiveFields); err != nil {
		return nil, nil, err
	}
	return effective, effectiveFields, nil
}

func validateOnlyChatEffectiveRequest(channel *model.Channel, request *types.ChatCompletionRequest) error {
	if channel == nil {
		return nil
	}
	return providerCapabilityGateError(providersBase.ValidateOnlyChatRequest(channel.OnlyChat, request))
}

func mappedModelForChannel(channel *model.Channel, modelName string) (string, error) {
	canonicalModel := modelName
	if channel != nil {
		rawMapping := strings.TrimSpace(channel.GetModelMapping())
		if rawMapping != "" && rawMapping != "{}" {
			mapping := make(map[string]string)
			if err := json.Unmarshal([]byte(rawMapping), &mapping); err != nil {
				return "", err
			}
			if mapped := mapping[modelName]; mapped != "" {
				canonicalModel = mapped
			}
		}
	}
	return strings.TrimPrefix(canonicalModel, "+"), nil
}

func requireAdapterOperationSupport(operation providersBase.Operation, requireStored bool) requestChannelCapability {
	return func(channel *model.Channel) error {
		support := providers.ResolveAdapterSupport(channel)
		if requireStored {
			if !support.SupportsStoredResponses() {
				return &capabilityGateError{message: "channel adapter cannot relay the complete Stored Responses lifecycle", status: http.StatusServiceUnavailable}
			}
			return nil
		}
		if operation != "" && !support.Supports(operation) {
			return &capabilityGateError{message: fmt.Sprintf("channel adapter cannot relay operation %s", operation), status: http.StatusServiceUnavailable}
		}
		return nil
	}
}

func requireResponsesRequestCompatibility(operation providersBase.Operation, requireStored bool, fields map[string]json.RawMessage, requestedModel string) requestChannelCapability {
	operationSupport := requireAdapterOperationSupport(operation, requireStored)
	return func(channel *model.Channel) error {
		if err := operationSupport(channel); err != nil {
			return err
		}
		path, _ := providers.ResolveAdapterSupport(channel).DataPath(operation)
		mappedModel, err := mappedModelForChannel(channel, requestedModel)
		if err != nil {
			return &capabilityGateError{message: "channel has invalid model mapping configuration", status: http.StatusServiceUnavailable}
		}
		if path != providersBase.DataPathCrossProtocol {
			if err := providers.ValidateResponsesRequest(channel, operation, fields, mappedModel, true); err != nil {
				return providerCapabilityGateError(err)
			}
		}
		if operation == providersBase.OperationResponsesCreate {
			if path == providersBase.DataPathCrossProtocol {
				if err := validateResponsesCrossProtocolAdapterRepresentability(channel, mappedModel, fields); err != nil {
					return err
				}
			}
		}
		return nil
	}
}

func validateResponsesCrossProtocolAdapterRepresentability(channel *model.Channel, mappedModel string, fields map[string]json.RawMessage) error {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return newCapabilityGateError("request", "request cannot be represented by the selected Chat Completions adapter")
	}
	var request types.OpenAIResponsesRequest
	if err := json.Unmarshal(encoded, &request); err != nil {
		return newCapabilityGateError("request", "request cannot be represented by the selected Chat Completions adapter")
	}
	if err := validateResponsesToChatRepresentability(&request, fields); err != nil {
		return err
	}
	chatRequest, err := request.ToChatCompletionRequest()
	if err != nil {
		return newCapabilityGateError("request", err.Error())
	}
	chatRequest.Model = mappedModel
	encodedChat, err := json.Marshal(chatRequest)
	if err != nil {
		return newCapabilityGateError("request", "request cannot be represented by the selected Chat Completions adapter")
	}
	var chatFields map[string]json.RawMessage
	if err := json.Unmarshal(encodedChat, &chatFields); err != nil {
		return newCapabilityGateError("request", "request cannot be represented by the selected Chat Completions adapter")
	}
	effectiveChat, effectiveChatFields, err := effectiveChatRequestForChannel(channel, mappedModel, chatRequest, chatFields)
	if err != nil {
		return &capabilityGateError{message: "channel has invalid request transform configuration", status: http.StatusServiceUnavailable}
	}
	if err := validateOnlyChatEffectiveRequest(channel, effectiveChat); err != nil {
		return err
	}
	if _, err := providers.AssessChatRequest(channel, mappedModel, effectiveChat, effectiveChatFields); err != nil {
		return providerCapabilityGateError(err)
	}
	_, _, err = providers.AssessChatRemoteMedia(channel, effectiveChat)
	return remoteMediaCapabilityGateError(err)
}

func requireResponsesWSAdapterSupport(requireStored bool, fields map[string]json.RawMessage, requestedModel string) requestChannelCapability {
	return func(channel *model.Channel) error {
		support := providers.ResolveAdapterSupport(channel)
		if !support.Supports(providersBase.OperationResponsesWebSocket) {
			return &capabilityGateError{message: "channel adapter is not enabled for native Responses WebSocket", status: http.StatusUpgradeRequired}
		}
		if requireStored && !support.SupportsStoredResponses() {
			return &capabilityGateError{message: "channel adapter cannot relay the complete Stored Responses lifecycle", status: http.StatusServiceUnavailable}
		}
		mappedModel, err := mappedModelForChannel(channel, requestedModel)
		if err != nil {
			return &capabilityGateError{message: "channel has invalid model mapping configuration", status: http.StatusServiceUnavailable}
		}
		if strings.TrimSpace(mappedModel) != strings.TrimSpace(requestedModel) {
			return &capabilityGateError{message: "native Responses WebSocket does not allow model mapping", status: http.StatusUpgradeRequired}
		}
		changesRequest, err := channelCustomParameterChangesRequest(channel, mappedModel, fields)
		if err != nil {
			return &capabilityGateError{message: "channel custom parameters are invalid", status: http.StatusServiceUnavailable}
		}
		if changesRequest {
			return &capabilityGateError{message: "native Responses WebSocket does not allow request body transforms", status: http.StatusUpgradeRequired}
		}
		if err := providers.ValidateResponsesRequest(channel, providersBase.OperationResponsesWebSocket, fields, mappedModel, true); err != nil {
			var requestErr *providersBase.RequestCapabilityError
			if errors.As(err, &requestErr) {
				return newCapabilityGateError(requestErr.Param, requestErr.Message)
			}
			return newCapabilityGateError("request", err.Error())
		}
		return nil
	}
}

func channelCustomParameterChangesRequest(channel *model.Channel, modelName string, fields map[string]json.RawMessage) (bool, error) {
	if channel == nil {
		return false, nil
	}
	customParams, err := channel.GetCustomParameterMap()
	if err != nil || len(customParams) == 0 {
		return false, err
	}
	body := make(map[string]interface{}, len(fields))
	for name, raw := range fields {
		var value interface{}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return false, err
		}
		body[name] = value
	}
	before, err := json.Marshal(body)
	if err != nil {
		return false, err
	}
	after := providersBase.ApplyCustomParams(body, customParams, modelName, true)
	encoded, err := json.Marshal(after)
	if err != nil {
		return false, err
	}
	return !bytes.Equal(before, encoded), nil
}

func validateResponsesWSClientEnvelope(fields map[string]json.RawMessage) error {
	if _, present := fields["stream_id"]; present {
		return newCapabilityGateError("stream_id", "responses websocket stream_id lanes are not supported")
	}
	return nil
}

func requireEndpointEnabled(relayMode int) requestChannelCapability {
	return func(channel *model.Channel) error {
		return providerCapabilityGateError(providers.AssessEndpoint(channel, relayMode))
	}
}
