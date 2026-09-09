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
		{
			name: "redacted quota disposition disables",
			err: &types.OpenAIErrorWithStatusCode{
				OpenAIError:            types.OpenAIError{Type: "upstream_error", Code: "provider_account_error", Message: "provider account rejected the request"},
				StatusCode:             http.StatusTooManyRequests,
				ProviderQuotaExhausted: true,
			},
			wantDisable: true,
		},
		{
			name: "redacted auth disposition disables",
			err: &types.OpenAIErrorWithStatusCode{
				OpenAIError:          types.OpenAIError{Type: "upstream_error", Code: "provider_account_error", Message: "provider account rejected the request"},
				StatusCode:           http.StatusBadRequest,
				ProviderAuthRejected: true,
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

func TestShouldDisableChannelReadsLatestRuntimePublication(t *testing.T) {
	originalManager := config.GlobalOption
	manager := config.NewOptionManager()
	automaticDisable := false
	manager.RegisterBoolOption("AutomaticDisableChannelEnabled", &automaticDisable, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{"AutomaticDisableChannelEnabled": "false"}); err != nil {
		t.Fatalf("publish disabled policy: %v", err)
	}
	config.GlobalOption = manager
	t.Cleanup(func() { config.GlobalOption = originalManager })

	providerAuthError := &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusUnauthorized}
	if ShouldDisableChannel(config.ChannelTypeOpenAI, providerAuthError) {
		t.Fatal("disabled automatic policy should not disable the channel")
	}
	if _, err := manager.PublishRuntimeOverrides(2, map[string]string{"AutomaticDisableChannelEnabled": "true"}); err != nil {
		t.Fatalf("publish enabled policy: %v", err)
	}
	if !ShouldDisableChannel(config.ChannelTypeOpenAI, providerAuthError) {
		t.Fatal("a later disable decision should observe the newer publication")
	}
}

func TestShouldDisableChannelKeywordMatchingRemainsCaseSensitive(t *testing.T) {
	originalManager := config.GlobalOption
	manager := config.NewOptionManager()
	automaticDisable := true
	keywords := "Quota Exhausted"
	manager.RegisterBoolOption("AutomaticDisableChannelEnabled", &automaticDisable, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	manager.RegisterValueOption("DisableChannelKeywords", config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{
		"AutomaticDisableChannelEnabled": "true",
		"DisableChannelKeywords":         keywords,
	}); err != nil {
		t.Fatalf("publish disable policy: %v", err)
	}
	config.GlobalOption = manager
	t.Cleanup(func() { config.GlobalOption = originalManager })

	providerError := &types.OpenAIErrorWithStatusCode{
		StatusCode: http.StatusBadGateway,
		OpenAIError: types.OpenAIError{
			Message: "quota exhausted",
		},
	}
	if ShouldDisableChannel(config.ChannelTypeOpenAI, providerError) {
		t.Fatal("keyword matching must preserve its case-sensitive contract")
	}
	providerError.Message = "Quota Exhausted"
	if !ShouldDisableChannel(config.ChannelTypeOpenAI, providerError) {
		t.Fatal("exact-case keyword should disable the channel")
	}
}
