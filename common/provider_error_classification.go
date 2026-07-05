package common

import (
	"fmt"
	"strings"

	"one-api/types"
)

func OpenAIErrorCodeText(code any) string {
	switch typed := code.(type) {
	case string:
		return strings.TrimSpace(typed)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func ProviderErrorIsQuotaExhausted(err types.OpenAIError) bool {
	switch strings.ToLower(strings.TrimSpace(err.Type)) {
	case "usage_limit_reached", "insufficient_quota":
		return true
	}
	switch strings.ToLower(OpenAIErrorCodeText(err.Code)) {
	case "usage_limit_reached", "insufficient_quota", "billing_not_active":
		return true
	}
	return false
}

func ProviderErrorIsAuthRejected(err types.OpenAIError) bool {
	switch strings.ToLower(strings.TrimSpace(err.Type)) {
	case "authentication_error", "permission_error", "forbidden":
		return true
	}
	switch strings.ToLower(OpenAIErrorCodeText(err.Code)) {
	case "invalid_api_key", "account_deactivated", "provider_authentication_failed",
		"token_invalidated", "invalid_token", "expired_token", "invalid_authentication_token":
		return true
	}
	return strings.TrimSpace(err.Param) == "PERMISSIONDENIED"
}

func ProviderErrorIsRateLimited(err types.OpenAIError) bool {
	switch strings.ToLower(strings.TrimSpace(err.Type)) {
	case "rate_limit_error":
		return true
	}
	switch strings.ToLower(OpenAIErrorCodeText(err.Code)) {
	case "rate_limit_exceeded":
		return true
	}
	return false
}
