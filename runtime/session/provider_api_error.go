package session

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"net/http"
	"strings"

	"one-api/common"
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

	object := readProviderAPIEnvelope(payload, false)
	if object == nil {
		return nil
	}
	return providerAPIErrorFromEnvelope(object)
}

func providerAPIErrorFromEnvelope(object *providerAPIEnvelope) *types.OpenAIErrorWithStatusCode {
	eventType := rawProviderAPIString(object.Type)
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
	if code := rawProviderAPIString(object.Code); code != "" {
		detail.Code = code
	}
	if message := rawProviderAPIString(object.Message); message != "" {
		detail.Message = message
	}
	if param := rawProviderAPIAnyString(object.Param); param != "" {
		detail.Param = param
	}
	if status := rawProviderAPIStatus(object.StatusCode); status > 0 {
		detail.StatusCode = status
	} else if status := rawProviderAPIStatus(object.Status); status > 0 {
		detail.StatusCode = status
	}

	applyProviderAPIOpenAIError(&detail, object.Error)
	applyProviderAPIResponseError(&detail, object.Response)
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
	apiErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Type:    detail.ErrType,
			Code:    detail.Code,
			Message: detail.Message,
			Param:   detail.Param,
		},
		StatusCode: detail.StatusCode,
		LocalError: false,
	}
	apiErr.ProviderQuotaExhausted = apiErr.StatusCode == http.StatusPaymentRequired || common.ProviderErrorIsQuotaExhausted(apiErr.OpenAIError)
	apiErr.ProviderAuthRejected = apiErr.StatusCode == http.StatusUnauthorized || common.ProviderErrorIsAuthRejected(apiErr.OpenAIError)
	return apiErr
}

// OpenAIErrorEnvelopeFromPayload recognizes only the top-level OpenAI error
// envelope used by Chat and legacy Completions. It deliberately ignores
// unrelated top-level message/code fields, which remain provider-owned future
// success fields on exact-wire streams.
func OpenAIErrorEnvelopeFromPayload(payload []byte) *types.OpenAIErrorWithStatusCode {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil
	}
	object := readProviderAPIEnvelope(payload, true)
	if object == nil || !hasProviderAPIOpenAIError(object.Error) {
		return nil
	}
	return providerAPIErrorFromEnvelope(object)
}

// 控制证据只保留相关值在原输入中的切片，不复制 output、delta 或其他扩展。
// 它不授予原文回放权限；对客诊断脱敏由 providerresponse 尽力处理。
type providerAPIEnvelope struct {
	Type, Code, Message, Param, StatusCode, Status, Error json.RawMessage
	Response, ResponseError                               json.RawMessage
}

func readProviderAPIEnvelope(raw []byte, openAIOnly bool) *providerAPIEnvelope {
	var fields providerAPIEnvelope
	// 沿用控制提取的 v1 解释规则；该投影不决定原文能否交付。
	decoder := jsontext.NewDecoder(bytes.NewBuffer(raw), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	if decoder.PeekKind() != '{' || fields.read(decoder, raw, false, openAIOnly) != nil {
		return nil
	}
	if _, err := decoder.ReadToken(); err != io.EOF {
		return nil
	}
	return &fields
}

func (f *providerAPIEnvelope) read(decoder *jsontext.Decoder, raw []byte, response, openAIOnly bool) error {
	if decoder.PeekKind() != '{' {
		return decoder.SkipValue()
	}
	if _, err := decoder.ReadToken(); err != nil {
		return err
	}
	for decoder.PeekKind() != '}' {
		token, err := decoder.ReadToken()
		if err != nil {
			return err
		}
		key := token.String()
		var target *json.RawMessage
		if response {
			// 原 response DTO 使用 encoding/json 的大小写兼容匹配。
			switch {
			case strings.EqualFold(key, "error"):
				target = &f.ResponseError
			}
		} else if openAIOnly {
			if key == "error" {
				target = &f.Error
			}
		} else {
			switch key {
			case "type":
				target = &f.Type
			case "code":
				target = &f.Code
			case "message":
				target = &f.Message
			case "param":
				target = &f.Param
			case "status_code":
				target = &f.StatusCode
			case "status":
				target = &f.Status
			case "error":
				target = &f.Error
			case "response":
				start := int(decoder.InputOffset())
				f.ResponseError = nil
				if err := f.read(decoder, raw, true, false); err != nil {
					return err
				}
				for start < len(raw) && strings.ContainsRune(" \r\n\t,:", rune(raw[start])) {
					start++
				}
				f.Response = raw[start:int(decoder.InputOffset())]
				continue
			}
		}
		start := int(decoder.InputOffset())
		if err := decoder.SkipValue(); err != nil {
			return err
		}
		if target != nil {
			for start < len(raw) && strings.ContainsRune(" \r\n\t,:", rune(raw[start])) {
				start++
			}
			*target = raw[start:int(decoder.InputOffset())]
		}
	}
	_, err := decoder.ReadToken()
	return err
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
	applyProviderAPITypedError(detail, openAIError)
}

func applyProviderAPITypedError(detail *providerAPIErrorDetail, openAIError types.OpenAIError) {
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
	decoder := json.NewDecoder(bytes.NewBuffer(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&response); err != nil || response.Error == nil {
		return
	}
	applyProviderAPITypedError(detail, *response.Error)
}

func providerAPIPayloadHasErrorSignal(object *providerAPIEnvelope, eventType string) bool {
	if object == nil {
		return false
	}
	eventType = strings.TrimSpace(eventType)
	eventIsError := strings.EqualFold(eventType, "error")
	if eventIsError {
		return true
	}

	failedTerminal := providerAPIEventTypeIsFailedTerminal(eventType)
	topLevelCode := rawProviderAPIString(object.Code)
	topLevelMessage := rawProviderAPIString(object.Message)
	hasExplicitTopLevelDetail := topLevelCode != "" || topLevelMessage != ""
	status := rawProviderAPIStatus(object.StatusCode)
	if status <= 0 {
		status = rawProviderAPIStatus(object.Status)
	}
	statusIndicatesHTTPError := status >= http.StatusBadRequest

	// 成功响应没有错误候选时，无需再次读取整个 response 来判断 status。
	if (hasProviderAPIOpenAIError(object.ResponseError) || hasExplicitTopLevelDetail || statusIndicatesHTTPError) &&
		(failedTerminal || providerAPIResponseHasFailureStatus(object.Response)) {
		return true
	}

	topLevelPayload := eventType == ""
	eventTypeIsErrorDetail := meaningfulProviderAPIErrorType(eventType)
	if (topLevelPayload || eventTypeIsErrorDetail) && hasProviderAPIOpenAIError(object.Error) {
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
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false
	}
	// Envelope 读取器已确认语法；只有空对象不提供错误信号。
	return !(raw[0] == '{' && raw[len(raw)-1] == '}' && len(bytes.TrimSpace(raw[1:len(raw)-1])) == 0)
}

func providerAPIResponseHasFailureStatus(raw json.RawMessage) bool {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return false
	}
	var response struct {
		Status string `json:"status,omitempty"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
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
