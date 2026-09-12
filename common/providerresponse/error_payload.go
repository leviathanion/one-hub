package providerresponse

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"one-api/types"
)

// SanitizeAPIError 仅生成对客错误副本，保留状态、分类和原始错误协议。
func SanitizeAPIError(apiErr *types.OpenAIErrorWithStatusCode, credentials ...string) *types.OpenAIErrorWithStatusCode {
	if apiErr == nil {
		return nil
	}
	safe := *apiErr
	safe.OpenAIError = SanitizeErrorFields(apiErr.OpenAIError, credentials...)
	if len(apiErr.RawBody) > 0 && apiErr.StatusCode >= http.StatusBadRequest {
		var changed bool
		safe.RawBody, changed = SanitizeErrorResponse(apiErr.RawBody, credentials...)
		if changed {
			safe.ResponseHeaders = apiErr.ResponseHeaders.Clone()
			for _, name := range []string{"Content-Encoding", "Content-Length", "Content-Range", "Digest", "Etag"} {
				safe.ResponseHeaders.Del(name)
			}
		}
	}
	if !apiErr.ReplayRawResponse || apiErr.StatusCode >= http.StatusBadRequest {
		safe.ResponseHeaders = FilterCredentialHeaders(safe.ResponseHeaders, credentials...)
	}
	return &safe
}

// 只对诊断做 JSON 转换，协议字段保留原始 Go 类型和值。
func SanitizeErrorFields(value types.OpenAIError, credentials ...string) types.OpenAIError {
	type diagnostics struct {
		Message    string `json:"message"`
		InnerError any    `json:"innererror,omitempty"`
	}
	raw, err := json.Marshal(diagnostics{Message: value.Message, InnerError: value.InnerError})
	if err != nil {
		return value
	}
	safe, changed := sanitizeErrorObject(raw, credentials)
	if !changed {
		return value
	}
	var result diagnostics
	decoder := json.NewDecoder(bytes.NewReader(safe))
	decoder.UseNumber()
	if decoder.Decode(&result) != nil {
		return value
	}
	value.Message = result.Message
	value.InnerError = result.InnerError
	return value
}

// SSEPayloadContainsError applies the shared error-envelope rule to a
// provider SSE payload. eventName is supplied separately because adapters
// that observe only data: lines do not retain the SSE event field.
func SSEPayloadContainsError(eventName string, payload []byte) bool {
	if strings.EqualFold(strings.TrimSpace(eventName), "error") {
		return true
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return false
	}
	if rawType, ok := fields["type"]; ok {
		var eventType string
		if json.Unmarshal(rawType, &eventType) == nil && strings.EqualFold(strings.TrimSpace(eventType), "error") {
			return true
		}
	}
	rawError, ok := fields["error"]
	return ok && len(rawError) > 0 && !bytes.Equal(bytes.TrimSpace(rawError), []byte("null"))
}
