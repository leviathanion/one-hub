package codex

import (
	"encoding/json"
	"net/http"
	"strings"

	"one-api/common/responsesws"
	runtimerealtime "one-api/runtime/realtime"
)

func codexStaleResponsesWSContinuationError(eventID string) error {
	return runtimerealtime.NewRecoverableClientPayloadError(responsesws.ErrStaleContinuation, codexResponsesWSPreviousResponseNotFoundPayload(eventID))
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
