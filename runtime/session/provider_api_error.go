package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"one-api/types"
)

// ProviderAPIErrorFromPayload extracts the provider-facing API error signal
// from a websocket frame without changing the frame itself. It is intentionally
// control-plane only: callers should still forward the original payload as-is.
func ProviderAPIErrorFromPayload(payload []byte) *types.OpenAIErrorWithStatusCode {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil
	}

	var object map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil {
		return nil
	}
	eventType := rawProviderAPIString(object["type"])
	if !providerAPIPayloadHasErrorSignal(object, eventType) {
		return nil
	}

	detail := providerAPIErrorDetail{
		ErrType: "upstream_error",
		Code:    "upstream_error",
		Message: "upstream websocket request failed",
	}
	if meaningfulProviderAPIType(eventType) {
		detail.ErrType = eventType
	}
	if code := rawProviderAPIString(object["code"]); code != "" {
		detail.Code = code
	}
	if message := rawProviderAPIString(object["message"]); message != "" {
		detail.Message = message
	}
	if param := rawProviderAPIAnyString(object["param"]); param != "" {
		detail.Param = param
	}
	if status := rawProviderAPIStatus(object["status_code"]); status > 0 {
		detail.StatusCode = status
	} else if status := rawProviderAPIStatus(object["status"]); status > 0 {
		detail.StatusCode = status
	}

	applyProviderAPIOpenAIError(&detail, object["error"])
	applyProviderAPIResponseError(&detail, object["response"])
	if detail.Code == "" || detail.Code == "upstream_error" {
		if code := fallbackProviderAPICode(detail.ErrType); code != "" {
			detail.Code = code
		}
	}
	if detail.ErrType == "" || detail.ErrType == "upstream_error" {
		if errType := fallbackProviderAPIType(detail.Code); errType != "" {
			detail.ErrType = errType
		}
	}
	if strings.TrimSpace(detail.Message) == "" {
		detail.Message = "upstream websocket request failed"
	}
	if detail.StatusCode <= 0 {
		detail.StatusCode = defaultProviderAPIStatus(detail.ErrType, detail.Code)
	}
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Type:    detail.ErrType,
			Code:    detail.Code,
			Message: detail.Message,
			Param:   detail.Param,
		},
		StatusCode: detail.StatusCode,
		LocalError: false,
	}
}

type providerAPIErrorDetail struct {
	ErrType    string
	Code       string
	Message    string
	Param      string
	StatusCode int
}

func applyProviderAPIOpenAIError(detail *providerAPIErrorDetail, raw json.RawMessage) {
	if detail == nil || len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return
	}
	var openAIError types.OpenAIError
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&openAIError); err != nil {
		return
	}
	if errType := strings.TrimSpace(openAIError.Type); errType != "" {
		detail.ErrType = errType
	}
	if code := providerAPICodeString(openAIError.Code); code != "" {
		detail.Code = code
	}
	if message := strings.TrimSpace(openAIError.Message); message != "" {
		detail.Message = message
	}
	if param := strings.TrimSpace(openAIError.Param); param != "" {
		detail.Param = param
	}
	if detail.Code == "" || detail.Code == "upstream_error" {
		detail.Code = fallbackProviderAPICode(detail.ErrType)
	}
}

func applyProviderAPIResponseError(detail *providerAPIErrorDetail, raw json.RawMessage) {
	if detail == nil || len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return
	}
	var response struct {
		Error *types.OpenAIError `json:"error,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&response); err != nil || response.Error == nil {
		return
	}
	encoded, err := json.Marshal(response.Error)
	if err != nil {
		return
	}
	applyProviderAPIOpenAIError(detail, encoded)
}

func providerAPIPayloadHasErrorSignal(object map[string]json.RawMessage, eventType string) bool {
	if len(object) == 0 {
		return false
	}
	eventType = strings.TrimSpace(eventType)
	eventIsError := strings.EqualFold(eventType, "error")
	if eventIsError {
		return true
	}

	failedTerminal := providerAPIEventTypeIsFailedTerminal(eventType)
	responseHasFailureStatus := providerAPIResponseHasFailureStatus(object["response"])
	if (failedTerminal || responseHasFailureStatus) && providerAPIResponseHasError(object["response"]) {
		return true
	}

	topLevelCode := rawProviderAPIString(object["code"])
	topLevelMessage := rawProviderAPIString(object["message"])
	hasExplicitTopLevelDetail := topLevelCode != "" || topLevelMessage != ""
	status := rawProviderAPIStatus(object["status_code"])
	if status <= 0 {
		status = rawProviderAPIStatus(object["status"])
	}
	statusIndicatesHTTPError := status >= http.StatusBadRequest

	if (failedTerminal || responseHasFailureStatus) && (hasExplicitTopLevelDetail || statusIndicatesHTTPError) {
		return true
	}

	topLevelPayload := eventType == ""
	eventTypeIsErrorDetail := meaningfulProviderAPIErrorType(eventType)
	if (topLevelPayload || eventTypeIsErrorDetail) && hasProviderAPIOpenAIError(object["error"]) {
		return true
	}

	if !(topLevelPayload || eventTypeIsErrorDetail) {
		return false
	}
	if statusIndicatesHTTPError || hasExplicitTopLevelDetail {
		return true
	}
	return false
}

func providerAPIEventTypeIsFailedTerminal(eventType string) bool {
	switch strings.ToLower(strings.TrimSpace(eventType)) {
	case "response.failed", "response.incomplete":
		return true
	default:
		return false
	}
}

func hasProviderAPIOpenAIError(raw json.RawMessage) bool {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return true
	}
	return len(object) > 0
}

func providerAPIResponseHasError(raw json.RawMessage) bool {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return false
	}
	var response struct {
		Error json.RawMessage `json:"error,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&response); err != nil {
		return false
	}
	return hasProviderAPIOpenAIError(response.Error)
}

func providerAPIResponseHasFailureStatus(raw json.RawMessage) bool {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return false
	}
	var response struct {
		Status string `json:"status,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&response); err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(response.Status)) {
	case types.ResponseStatusFailed, types.ResponseStatusIncomplete:
		return true
	default:
		return false
	}
}

func rawProviderAPIString(raw json.RawMessage) string {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return strings.TrimSpace(value)
	}
	return rawProviderAPIAnyString(raw)
}

func rawProviderAPIAnyString(raw json.RawMessage) string {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return ""
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return ""
	}
	return providerAPIAnyString(value)
}

func rawProviderAPIStatus(raw json.RawMessage) int {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return 0
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return 0
	}
	switch typed := value.(type) {
	case json.Number:
		status, _ := typed.Int64()
		return int(status)
	case float64:
		return int(typed)
	case string:
		var status int
		if _, err := fmt.Sscanf(strings.TrimSpace(typed), "%d", &status); err == nil {
			return status
		}
	}
	return 0
}

func meaningfulProviderAPIType(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	lower := strings.ToLower(value)
	if lower == "error" || strings.HasPrefix(lower, "response.") || strings.HasPrefix(lower, "session.") {
		return false
	}
	return true
}

func meaningfulProviderAPIErrorType(value string) bool {
	if !meaningfulProviderAPIType(value) {
		return false
	}
	if meaningfulProviderAPIErrorCode(value) {
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(value))
	switch lower {
	case "usage_limit_reached", "insufficient_quota", "authentication_error", "permission_error", "invalid_request_error", "rate_limit_error",
		"provider_error", "upstream_error", "upstream_failed", "provider_authentication_failed", "permission_denied", "forbidden":
		return true
	default:
		return strings.Contains(lower, "error") || strings.Contains(lower, "failed") || strings.Contains(lower, "failure")
	}
}

func meaningfulProviderAPIErrorCode(value string) bool {
	key := strings.ToLower(strings.TrimSpace(value))
	if key == "" {
		return false
	}
	if _, ok := providerAPIStatusForKey(key); ok {
		return true
	}
	switch key {
	case "account_deactivated", "billing_not_active":
		return true
	default:
		return strings.Contains(key, "error") || strings.Contains(key, "failed") || strings.Contains(key, "failure")
	}
}

func fallbackProviderAPICode(errType string) string {
	errType = strings.TrimSpace(errType)
	if meaningfulProviderAPIType(errType) {
		return errType
	}
	return "upstream_error"
}

func fallbackProviderAPIType(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return "upstream_error"
	}
	switch strings.ToLower(code) {
	case "usage_limit_reached", "insufficient_quota":
		return code
	case "invalid_api_key", "provider_authentication_failed", "account_deactivated", "billing_not_active":
		return "authentication_error"
	case "permission_denied", "forbidden":
		return "permission_error"
	case "rate_limit_exceeded", "provider_rate_limit_exceeded":
		return "rate_limit_error"
	case "invalid_request_error", "invalid_event", "unsupported_client_event", "session_busy":
		return "invalid_request_error"
	default:
		return "upstream_error"
	}
}

func defaultProviderAPIStatus(errType string, code string) int {
	if status, ok := providerAPIStatusForKey(code); ok {
		return status
	}
	if status, ok := providerAPIStatusForKey(errType); ok {
		return status
	}
	return http.StatusBadGateway
}

func providerAPIStatusForKey(value string) (int, bool) {
	key := strings.ToLower(strings.TrimSpace(value))
	switch key {
	case "rate_limit_error", "rate_limit_exceeded", "provider_rate_limit_exceeded", "usage_limit_reached", "insufficient_quota":
		return http.StatusTooManyRequests, true
	case "invalid_api_key", "provider_authentication_failed", "authentication_error", "account_deactivated", "billing_not_active":
		return http.StatusUnauthorized, true
	case "permission_error", "permission_denied", "forbidden":
		return http.StatusForbidden, true
	case "invalid_request_error", "invalid_event", "unsupported_client_event", "session_busy":
		return http.StatusBadRequest, true
	default:
		return 0, false
	}
}

func providerAPICodeString(code any) string {
	switch typed := code.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return strings.TrimSpace(typed.String())
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func providerAPIAnyString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return strings.TrimSpace(typed.String())
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}
