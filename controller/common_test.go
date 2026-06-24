package controller

import (
	"net/http"
	"testing"

	"one-api/common/config"
	"one-api/types"
)

func TestShouldDisableChannelProviderPayloadCodes(t *testing.T) {
	originalDisable := config.AutomaticDisableChannelEnabled
	config.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() {
		config.AutomaticDisableChannelEnabled = originalDisable
	})

	tests := []struct {
		name        string
		err         *types.OpenAIErrorWithStatusCode
		wantDisable bool
	}{
		{
			name: "usage limit disables",
			err: &types.OpenAIErrorWithStatusCode{
				OpenAIError: types.OpenAIError{Type: "usage_limit_reached", Code: "usage_limit_reached", Message: "monthly usage limit reached"},
				StatusCode:  http.StatusTooManyRequests,
			},
			wantDisable: true,
		},
		{
			name: "quota disables",
			err: &types.OpenAIErrorWithStatusCode{
				OpenAIError: types.OpenAIError{Type: "insufficient_quota", Code: "insufficient_quota", Message: "quota exhausted"},
				StatusCode:  http.StatusTooManyRequests,
			},
			wantDisable: true,
		},
		{
			name: "auth disables",
			err: &types.OpenAIErrorWithStatusCode{
				OpenAIError: types.OpenAIError{Type: "authentication_error", Code: "invalid_api_key", Message: "invalid api key"},
				StatusCode:  http.StatusUnauthorized,
			},
			wantDisable: true,
		},
		{
			name: "permission disables",
			err: &types.OpenAIErrorWithStatusCode{
				OpenAIError: types.OpenAIError{Type: "permission_error", Code: "permission_denied", Message: "permission denied"},
				StatusCode:  http.StatusForbidden,
			},
			wantDisable: true,
		},
		{
			name: "ordinary rate limit does not disable or fall through to keywords",
			err: &types.OpenAIErrorWithStatusCode{
				OpenAIError: types.OpenAIError{Type: "rate_limit_error", Code: "rate_limit_exceeded", Message: "Your credit balance is too low but this is only a short rate limit"},
				StatusCode:  http.StatusTooManyRequests,
			},
			wantDisable: false,
		},
		{
			name: "invalid request does not disable",
			err: &types.OpenAIErrorWithStatusCode{
				OpenAIError: types.OpenAIError{Type: "invalid_request_error", Code: "bad_input", Message: "invalid request"},
				StatusCode:  http.StatusBadRequest,
			},
			wantDisable: false,
		},
		{
			name: "local error does not disable",
			err: &types.OpenAIErrorWithStatusCode{
				OpenAIError: types.OpenAIError{Type: "authentication_error", Code: "invalid_api_key", Message: "local auth failure"},
				StatusCode:  http.StatusUnauthorized,
				LocalError:  true,
			},
			wantDisable: false,
		},
		{
			name: "gemini forbidden disables",
			err: &types.OpenAIErrorWithStatusCode{
				OpenAIError: types.OpenAIError{Message: "permission denied"},
				StatusCode:  http.StatusForbidden,
			},
			wantDisable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channelType := config.ChannelTypeOpenAI
			if tt.name == "gemini forbidden disables" {
				channelType = config.ChannelTypeGemini
			}
			if got := ShouldDisableChannel(channelType, tt.err); got != tt.wantDisable {
				t.Fatalf("expected disable=%v, got %v", tt.wantDisable, got)
			}
		})
	}
}
