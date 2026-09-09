package providerresponse

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"one-api/common"
	"one-api/types"
)

// SanitizeAPIError keeps provider failure classification available to routing
// while removing shared-account details before logging or notification.
func SanitizeAPIError(apiErr *types.OpenAIErrorWithStatusCode) *types.OpenAIErrorWithStatusCode {
	if apiErr == nil {
		return nil
	}
	quotaExhausted, authRejected := providerAccountFailure(apiErr)
	redactedMessage := common.RedactSensitiveText(apiErr.Message)
	redactedParam := common.RedactSensitiveText(apiErr.Param)
	classificationChanged := (quotaExhausted && !apiErr.ProviderQuotaExhausted) || (authRejected && !apiErr.ProviderAuthRejected)
	if !quotaExhausted && !authRejected && !classificationChanged && redactedMessage == apiErr.Message && redactedParam == apiErr.Param {
		return apiErr
	}

	safe := *apiErr
	safe.ProviderQuotaExhausted = safe.ProviderQuotaExhausted || quotaExhausted
	safe.ProviderAuthRejected = safe.ProviderAuthRejected || authRejected
	if quotaExhausted || authRejected {
		safe.OpenAIError = SafeAccountError(apiErr.StatusCode)
		safe.RawBody = nil
		safe.ReplayRawResponse = false
		return &safe
	}
	safe.Message = redactedMessage
	safe.Param = redactedParam
	if safe.Message != apiErr.Message || safe.Param != apiErr.Param {
		safe.RawBody = nil
		safe.ReplayRawResponse = false
	}
	return &safe
}

// SanitizeErrorPayload preserves ordinary provider payloads byte-for-byte. For
// confirmed provider errors it keeps the event envelope while replacing shared-
// account failures and redacting credential/account fields.
func SanitizeErrorPayload(payload []byte, apiErr *types.OpenAIErrorWithStatusCode) []byte {
	if apiErr == nil {
		return payload
	}
	redacted, _ := common.RedactSensitiveJSON(payload)
	safeErr := SanitizeAPIError(apiErr)
	quotaExhausted, authRejected := providerAccountFailure(apiErr)
	if !quotaExhausted && !authRejected {
		return redacted
	}

	decoder := json.NewDecoder(bytes.NewReader(redacted))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return redacted
	}
	replacement := openAIErrorObject(safeErr.OpenAIError)
	replaced := false
	nestedReplaced := false
	if _, ok := object["error"]; ok {
		object["error"] = replacement
		replaced = true
		nestedReplaced = true
	}
	if response, ok := object["response"].(map[string]any); ok {
		if _, exists := response["error"]; exists {
			response["error"] = replacement
			replaced = true
			nestedReplaced = true
		}
	}
	eventType, _ := object["type"].(string)
	if object["code"] != nil || object["message"] != nil || (strings.EqualFold(strings.TrimSpace(eventType), "error") && !nestedReplaced) {
		object["code"] = safeErr.Code
		object["message"] = safeErr.Message
		object["param"] = safeErr.Param
		replaced = true
	}
	if !replaced {
		return redacted
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return redacted
	}
	return encoded
}

// SafeAccountError replaces a shared provider account failure while callers
// preserve its HTTP status separately. Status fidelity does not authorize
// replaying an upstream account or credential error body.
func SafeAccountError(status int) types.OpenAIError {
	return types.OpenAIError{
		Message: "upstream provider account is unavailable",
		Type:    "upstream_error",
		Code:    "provider_account_error",
		Param:   strconv.Itoa(status),
	}
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

func providerAccountFailure(apiErr *types.OpenAIErrorWithStatusCode) (quotaExhausted, authRejected bool) {
	if apiErr == nil {
		return false, false
	}
	quotaExhausted = apiErr.ProviderQuotaExhausted || apiErr.StatusCode == http.StatusPaymentRequired || common.ProviderErrorIsQuotaExhausted(apiErr.OpenAIError)
	authRejected = apiErr.ProviderAuthRejected || apiErr.StatusCode == http.StatusUnauthorized || common.ProviderErrorIsAuthRejected(apiErr.OpenAIError)
	return quotaExhausted, authRejected
}

func openAIErrorObject(apiErr types.OpenAIError) map[string]any {
	return map[string]any{
		"message": apiErr.Message,
		"type":    apiErr.Type,
		"code":    apiErr.Code,
		"param":   apiErr.Param,
	}
}
