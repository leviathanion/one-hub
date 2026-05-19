package responsesws

import (
	"bytes"
	"encoding/json"
	"one-api/types"
	"strings"
)

type ResponsesTerminalKind int

const (
	ResponsesNonTerminal ResponsesTerminalKind = iota
	ResponsesSuccessTerminal
	ResponsesFailedTerminal
	ResponsesCancelledTerminal
)

type ResponsesTerminalResult struct {
	Kind              ResponsesTerminalKind
	EventType         string
	NormalizedPayload []byte
	Response          *types.OpenAIResponsesResponses
	ErrorCode         string
	ContinuationMiss  bool
	Malformed         bool
	MalformedError    string
}

func ClassifyResponsesWSEvent(payload []byte) ResponsesTerminalResult {
	result := ResponsesTerminalResult{Kind: ResponsesNonTerminal}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		result.Kind = ResponsesFailedTerminal
		result.Malformed = true
		result.MalformedError = err.Error()
		return result
	}

	eventType := rawStringField(object, "type")
	result.EventType = eventType

	var response *types.OpenAIResponsesResponses
	if rawResponse, ok := object["response"]; ok && !isJSONNull(rawResponse) {
		var decoded types.OpenAIResponsesResponses
		decoder := json.NewDecoder(bytes.NewReader(rawResponse))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err == nil {
			response = &decoded
			result.Response = response
		}
	}

	topLevelErrorCode, topLevelErrorMessage, hasTopLevelError := rawOpenAIErrorFields(object["error"])
	responseErrorCode := ""
	responseErrorMessage := ""
	hasResponseError := false
	if response != nil && response.Error != nil {
		hasResponseError = true
		responseErrorCode = openAIErrorCode(response.Error.Code)
		responseErrorMessage = response.Error.Message
	}
	if topLevelErrorCode != "" {
		result.ErrorCode = topLevelErrorCode
	} else if responseErrorCode != "" {
		result.ErrorCode = responseErrorCode
	} else if code := rawStringField(object, "code"); code != "" {
		result.ErrorCode = code
	}

	status := ""
	if response != nil {
		status = strings.ToLower(strings.TrimSpace(response.Status))
	}

	switch eventType {
	case "error":
		result.Kind = ResponsesFailedTerminal
	case "response.completed", "response.done":
		switch {
		case hasTopLevelError || hasResponseError:
			result.Kind = ResponsesFailedTerminal
		case status == "" || status == types.ResponseStatusCompleted:
			result.Kind = ResponsesSuccessTerminal
		case isResponsesCancelledStatus(status):
			result.Kind = ResponsesCancelledTerminal
		case isResponsesFailedStatus(status):
			result.Kind = ResponsesFailedTerminal
		}
	case "response.cancelled", "response.canceled":
		result.Kind = ResponsesCancelledTerminal
	case "response.failed", "response.incomplete":
		result.Kind = ResponsesFailedTerminal
	}

	if result.Kind != ResponsesNonTerminal {
		result.ContinuationMiss = isContinuationMiss(result.ErrorCode, topLevelErrorMessage, responseErrorMessage, rawStringField(object, "message"))
		result.NormalizedPayload = normalizeTerminalPayload(object, eventType, result.Kind)
	}
	return result
}

func ClassifyResponsesWSTerminal(eventType string, response *types.OpenAIResponsesResponses, hasEventError bool) ResponsesTerminalResult {
	status := ""
	hasResponseError := false
	errorCode := ""
	if response != nil {
		status = response.Status
		if response.Error != nil {
			hasResponseError = true
			errorCode = openAIErrorCode(response.Error.Code)
		}
	}
	result := ClassifyResponsesWSTerminalStatus(eventType, status, hasEventError, hasResponseError, errorCode)
	result.Response = response
	return result
}

func ClassifyResponsesWSTerminalStatus(eventType string, status string, hasEventError bool, hasResponseError bool, errorCode string) ResponsesTerminalResult {
	result := ResponsesTerminalResult{
		Kind:      ResponsesNonTerminal,
		EventType: strings.TrimSpace(eventType),
	}
	status = strings.ToLower(strings.TrimSpace(status))
	result.ErrorCode = strings.TrimSpace(errorCode)
	if hasEventError {
		result.Kind = ResponsesFailedTerminal
		result.ContinuationMiss = isContinuationMiss(result.ErrorCode)
		return result
	}
	switch result.EventType {
	case "error":
		result.Kind = ResponsesFailedTerminal
	case "response.completed", "response.done":
		switch {
		case hasEventError || hasResponseError:
			result.Kind = ResponsesFailedTerminal
		case status == "" || status == types.ResponseStatusCompleted:
			result.Kind = ResponsesSuccessTerminal
		case isResponsesCancelledStatus(status):
			result.Kind = ResponsesCancelledTerminal
		case isResponsesFailedStatus(status):
			result.Kind = ResponsesFailedTerminal
		}
	case "response.cancelled", "response.canceled":
		result.Kind = ResponsesCancelledTerminal
	case "response.failed", "response.incomplete":
		result.Kind = ResponsesFailedTerminal
	}
	if result.Kind != ResponsesNonTerminal {
		result.ContinuationMiss = isContinuationMiss(result.ErrorCode)
	}
	return result
}

func normalizeTerminalPayload(object map[string]json.RawMessage, eventType string, kind ResponsesTerminalKind) []byte {
	if eventType == "response.done" && kind == ResponsesSuccessTerminal {
		object = cloneRawObject(object)
		object["type"] = json.RawMessage(`"response.completed"`)
		normalized, err := json.Marshal(object)
		if err == nil {
			return normalized
		}
	}
	if (eventType == "response.done" || eventType == "response.completed") && kind == ResponsesFailedTerminal {
		object = cloneRawObject(object)
		object["type"] = json.RawMessage(`"response.failed"`)
		normalized, err := json.Marshal(object)
		if err == nil {
			return normalized
		}
	}
	return nil
}

func isResponsesFailedStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case types.ResponseStatusFailed, types.ResponseStatusIncomplete:
		return true
	default:
		return false
	}
}

func isResponsesCancelledStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case types.ResponseStatusCancelled, "canceled":
		return true
	default:
		return false
	}
}

func rawOpenAIErrorFields(raw json.RawMessage) (code string, message string, ok bool) {
	if len(raw) == 0 || isJSONNull(raw) {
		return "", "", false
	}
	var openaiErr types.OpenAIError
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&openaiErr); err != nil {
		return "", "", true
	}
	return openAIErrorCode(openaiErr.Code), openaiErr.Message, true
}

func openAIErrorCode(code any) string {
	switch typed := code.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return strings.TrimSpace(typed.String())
	case nil:
		return ""
	default:
		return strings.TrimSpace(strings.TrimSpace(toString(typed)))
	}
}

func toString(value any) string {
	if value == nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return strings.Trim(string(encoded), `"`)
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func isContinuationMiss(values ...string) bool {
	for _, value := range values {
		trimmed := strings.ToLower(strings.TrimSpace(value))
		if trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, "previous_response_not_found") {
			return true
		}
		if strings.Contains(trimmed, "previous response") && strings.Contains(trimmed, "not found") {
			return true
		}
	}
	return false
}
