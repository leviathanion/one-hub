package responsesws

import (
	"bytes"
	"encoding/json"
	"strings"

	commonresponses "one-api/common/responses"
	"one-api/types"
)

// ResponsesTerminalKind classifies whether a ResponsesWS payload is terminal.
type ResponsesTerminalKind int

const (
	ResponsesNonTerminal ResponsesTerminalKind = iota
	ResponsesSuccessTerminal
	ResponsesFailedTerminal
	ResponsesCancelledTerminal
)

// ResponsesTerminalResult is the parsed terminal classification for a
// ResponsesWS provider payload.
type ResponsesTerminalResult struct {
	Kind              ResponsesTerminalKind
	EventType         string
	Response          *types.OpenAIResponsesResponses
	ErrorCode         string
	ContinuationMiss  bool
	RequestError      bool
	ConnectionError   bool
	HasSequenceNumber bool
	SequenceNumber    int64
	Malformed         bool
	MalformedError    string
}

func ClassifyResponsesWSEvent(payload []byte) ResponsesTerminalResult {
	result := ResponsesTerminalResult{Kind: ResponsesNonTerminal}
	observed, err := commonresponses.ObserveEventLifecycle(payload)
	if err != nil {
		result.Kind = ResponsesFailedTerminal
		result.Malformed = true
		result.MalformedError = err.Error()
		return result
	}

	eventType := observed.Type
	result.EventType = eventType
	if isForeignResponsesWSEventType(eventType) {
		return malformedResponsesTerminalResult(result, "Realtime terminal event is not valid on a Responses websocket")
	}
	if observed.SequenceError == nil {
		result.HasSequenceNumber = observed.HasSequence
		result.SequenceNumber = observed.Sequence
	} else if strings.HasPrefix(eventType, "response.") {
		return malformedResponsesTerminalResult(result, "sequence_number must be a non-negative integer")
	}

	response := observed.Response
	result.Response = response
	if isKnownResponsesTerminalEventType(eventType) {
		if !observed.ResponsePresent || !observed.ResponseObject || response == nil {
			return malformedResponsesTerminalResult(result, "terminal response object is required")
		}
		if observed.ResponseFieldError != nil {
			return malformedResponsesTerminalResult(result, observed.ResponseFieldError.Error())
		}
		if strings.TrimSpace(response.ID) == "" {
			return malformedResponsesTerminalResult(result, "terminal response.id is required")
		}
		if !result.HasSequenceNumber {
			return malformedResponsesTerminalResult(result, "terminal sequence_number is required")
		}
	}

	topLevelErrorCode, topLevelErrorMessage, hasTopLevelError := rawOpenAIErrorFields(observed.TopLevelError)
	responseErrorCode := ""
	responseErrorMessage := ""
	hasResponseError := observed.ResponseErrorPresent
	if response != nil && response.Error != nil {
		responseErrorCode = openAIErrorCode(response.Error.Code)
		responseErrorMessage = response.Error.Message
	}
	if topLevelErrorCode != "" {
		result.ErrorCode = topLevelErrorCode
	} else if responseErrorCode != "" {
		result.ErrorCode = responseErrorCode
	} else {
		var code string
		_ = json.Unmarshal(observed.TopLevelCode, &code)
		result.ErrorCode = strings.TrimSpace(code)
	}

	status := ""
	if response != nil {
		status = strings.ToLower(strings.TrimSpace(response.Status))
	}

	switch eventType {
	case "error":
		if isResponsesWSConnectionError(result.ErrorCode) {
			result.ConnectionError = true
		} else {
			result.RequestError = true
		}
	case "response.completed":
		switch {
		case hasTopLevelError || hasResponseError:
			result.Kind = ResponsesFailedTerminal
		case isResponsesFailedStatus(status), isResponsesCancelledStatus(status):
			result.Kind = ResponsesFailedTerminal
		default:
			// The lifecycle event type is authoritative. Treat future or
			// contradictory non-terminal response statuses as completed here so
			// a valid response.completed event cannot strand the active turn.
			result.Kind = ResponsesSuccessTerminal
		}
	case "response.failed", "response.incomplete":
		result.Kind = ResponsesFailedTerminal
	}

	if result.Kind != ResponsesNonTerminal || result.RequestError || result.ConnectionError {
		var topLevelMessage string
		_ = json.Unmarshal(observed.TopLevelMessage, &topLevelMessage)
		result.ContinuationMiss = isContinuationMiss(result.ErrorCode, topLevelErrorMessage, responseErrorMessage, topLevelMessage)
	}
	return result
}

func isResponsesWSConnectionError(errorCode string) bool {
	switch strings.TrimSpace(errorCode) {
	case "websocket_connection_limit_reached":
		return true
	default:
		return false
	}
}

func isKnownResponsesTerminalEventType(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.completed",
		"response.failed",
		"response.incomplete":
		return true
	default:
		return false
	}
}

// isForeignResponsesWSEventType is a Responses WebSocket protocol gate, not a
// Realtime lifecycle classifier. It rejects known foreign terminal markers at
// the public Responses boundary.
func isForeignResponsesWSEventType(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.done", "response.cancelled", "response.canceled":
		return true
	default:
		return false
	}
}

func malformedResponsesTerminalResult(result ResponsesTerminalResult, message string) ResponsesTerminalResult {
	result.Kind = ResponsesFailedTerminal
	result.Malformed = true
	result.MalformedError = strings.TrimSpace(message)
	return result
}

// ClassifyResponsesWSTerminal classifies already-decoded official Responses
// lifecycle events. Raw provider frames must use ClassifyResponsesWSEvent so
// required wire fields are validated.
func ClassifyResponsesWSTerminal(eventType string, response *types.OpenAIResponsesResponses, hasEventError bool) ResponsesTerminalResult {
	result := ResponsesTerminalResult{
		Kind:      ResponsesNonTerminal,
		EventType: strings.TrimSpace(eventType),
		Response:  response,
	}
	if hasEventError || result.EventType == "error" {
		result.RequestError = true
		return result
	}
	switch result.EventType {
	case "response.completed":
		result.Kind = ResponsesSuccessTerminal
		if response != nil && (response.Error != nil || isResponsesFailedStatus(response.Status) || isResponsesCancelledStatus(response.Status)) {
			result.Kind = ResponsesFailedTerminal
		}
	case "response.failed", "response.incomplete":
		result.Kind = ResponsesFailedTerminal
	}
	return result
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
	if err := json.Unmarshal(raw, &openaiErr); err != nil {
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
