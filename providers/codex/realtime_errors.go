package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"strings"

	"one-api/common"
	"one-api/common/logger"
	"one-api/common/responsesws"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
	"one-api/types"
)

func newCodexRealtimeClientError(eventID, code, message string) error {
	return types.NewErrorEvent(eventID, "invalid_request_error", code, message)
}

func newCodexRealtimeProviderError(eventID, code, message string) error {
	return types.NewErrorEvent(eventID, "provider_error", code, message)
}

func codexStaleResponsesWSContinuationError(eventID string) error {
	return responsesws.NewClientPayloadError(responsesws.ErrStaleContinuation, codexResponsesWSPreviousResponseNotFoundPayload(eventID))
}

func codexResponsesWSPreviousResponseNotFoundPayload(eventID string) []byte {
	payload := map[string]any{
		"type":   "error",
		"status": http.StatusConflict,
		"error": map[string]any{
			"type":    "invalid_request_error",
			"code":    "previous_response_not_found",
			"message": "previous response was not found",
			"param":   "previous_response_id",
		},
	}
	if eventID = strings.TrimSpace(eventID); eventID != "" {
		payload["event_id"] = eventID
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}

func codexRealtimeProviderErrorEventPayload(eventID, code, message string) []byte {
	return []byte(types.NewErrorEvent(eventID, "provider_error", code, message).Error())
}

func codexRealtimeClientPayloadErrorFromObserver(eventID string, err error) error {
	if err == nil {
		return nil
	}
	if payload := runtimerealtime.ClientPayloadFromError(err); len(payload) > 0 {
		return err
	}
	var event *types.Event
	if errors.As(err, &event) && event != nil && event.IsError() {
		return err
	}
	code := "quota_exhausted"
	var apiErr *types.OpenAIErrorWithStatusCode
	if errors.As(err, &apiErr) && apiErr != nil {
		code = codexRealtimeErrorCodeString(apiErr.Code, code)
	}
	event = types.NewErrorEvent(eventID, "system_error", code, codexRealtimeObserverErrorMessage(code))
	return runtimerealtime.NewClientPayloadError(event, []byte(event.Error()))
}

func rollbackCodexTurnAdmissionLocked(observer runtimesession.TurnObserver, reason string) {
	if observer == nil {
		return
	}
	if err := runtimesession.RollbackTurnAdmission(observer, reason); err != nil {
		logCodexRealtimeInternalError("codex realtime turn admission rollback failed: " + err.Error())
	}
}

func codexRealtimeShouldRollbackAdmissionAfterLocalFailure(err error) bool {
	if err == nil {
		return false
	}
	var event *types.Event
	if !errors.As(err, &event) || event == nil || !event.IsError() {
		return false
	}
	switch codexRealtimeErrorCodeString(event.ErrorDetail.Code, "") {
	case "responses_ws_unsupported_for_channel", "transport_unavailable", "bridge_open_cancelled", "bridge_open_failed":
		return true
	default:
		return false
	}
}

func codexRealtimeStaticErrorMessage(code string) string {
	switch strings.TrimSpace(code) {
	case "invalid_event":
		return "invalid realtime event"
	case "invalid_session_id":
		return "invalid session id"
	case "ws_request_failed":
		return "upstream websocket request failed"
	case "ws_write_failed":
		return "upstream websocket write failed"
	case "provider_connection_closed":
		return "upstream websocket connection closed"
	case "bridge_open_cancelled":
		return "provider bridge open cancelled"
	case "bridge_open_failed":
		return "provider bridge open failed"
	case "bridge_stream_failed":
		return "provider bridge stream failed"
	default:
		return "codex realtime request failed"
	}
}

func codexRealtimeObserverErrorMessage(code string) string {
	switch strings.TrimSpace(code) {
	case "insufficient_user_quota", "quota_exhausted":
		return "realtime quota limit exceeded"
	case "pre_consume_token_quota_failed":
		return "realtime token quota is not enough"
	case "invalid_model_price":
		return "realtime model price is invalid"
	default:
		return "realtime quota check failed"
	}
}

func logCodexRealtimeInternalError(message string) {
	message = common.RedactSensitiveText(message)
	if _, file, line, ok := runtime.Caller(1); ok {
		if idx := strings.LastIndex(file, "/"); idx >= 0 {
			file = file[idx+1:]
		}
		message = fmt.Sprintf("%s caller=%s:%d", message, file, line)
	}
	if logger.Logger != nil {
		logger.LogError(context.Background(), message)
		return
	}
	log.Printf("%s", message)
}

func codexRealtimeErrorCodeString(code any, fallback string) string {
	switch typed := code.(type) {
	case string:
		if trimmed := strings.TrimSpace(typed); trimmed != "" {
			return trimmed
		}
	case nil:
	default:
		if trimmed := strings.TrimSpace(fmt.Sprint(typed)); trimmed != "" && trimmed != "<nil>" {
			return trimmed
		}
	}
	return fallback
}

func codexRealtimeErrorFromOpenAIError(eventID string, errWithCode *types.OpenAIErrorWithStatusCode) error {
	if errWithCode == nil {
		return newCodexRealtimeProviderError(eventID, "provider_error", "provider error")
	}

	code := codexRealtimeErrorCodeString(errWithCode.Code, "provider_error")
	message := strings.TrimSpace(errWithCode.Message)
	if message == "" {
		message = "provider error"
	}
	return newCodexRealtimeProviderError(eventID, code, message)
}
