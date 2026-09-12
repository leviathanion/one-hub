package requester

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"one-api/types"

	"github.com/spf13/viper"
)

func TestInitHttpClientDoesNotClampResponseHeaderTimeout(t *testing.T) {
	t.Cleanup(func() {
		viper.Set("relay_timeout", 0)
		HTTPClient = nil
	})

	viper.Set("relay_timeout", 300)
	InitHttpClient()

	if HTTPClient == nil {
		t.Fatal("expected HTTP client to be initialized")
	}

	transport, ok := HTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", HTTPClient.Transport)
	}

	if transport.ResponseHeaderTimeout != 0 {
		t.Fatalf("expected no response header timeout clamp, got %s", transport.ResponseHeaderTimeout)
	}
	if !transport.DisableCompression {
		t.Fatal("expected provider transport transparent decompression to be disabled")
	}

	if HTTPClient.Timeout != 300*time.Second {
		t.Fatalf("expected relay timeout to configure client timeout, got %s", HTTPClient.Timeout)
	}
	original, _ := http.NewRequest(http.MethodPost, "http://provider.example/v1/responses", nil)
	upgrade, _ := http.NewRequest(http.MethodPost, "https://provider.example/v1/responses", nil)
	if err := HTTPClient.CheckRedirect(upgrade, []*http.Request{original}); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("expected provider redirect not to create a second submission, got %v", err)
	}
	crossAuthority, _ := http.NewRequest(http.MethodPost, "https://other.example/v1/responses", nil)
	if err := HTTPClient.CheckRedirect(crossAuthority, []*http.Request{upgrade}); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("expected cross-authority redirect to remain un-followed, got %v", err)
	}
	downgrade, _ := http.NewRequest(http.MethodPost, "http://provider.example/v1/responses", nil)
	if err := HTTPClient.CheckRedirect(downgrade, []*http.Request{upgrade}); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("expected HTTPS downgrade to remain un-followed, got %v", err)
	}
}

func TestHTTPRequesterTreatsRedirectAsProviderResponseStatus(t *testing.T) {
	requester := NewHTTPRequester("", nil)
	if !requester.IsFailureStatusCode(&http.Response{StatusCode: http.StatusTemporaryRedirect}) {
		t.Fatal("expected 307 to avoid success-body decoding and replay")
	}
}

func TestHandleErrorRespNormalizesUsageExhaustedStatus(t *testing.T) {
	tests := []struct {
		name string
		err  types.OpenAIError
	}{
		{
			name: "usage_limit_reached",
			err:  types.OpenAIError{Type: "usage_limit_reached", Code: "usage_limit_reached", Message: "limit"},
		},
		{
			name: "billing_not_active",
			err:  types.OpenAIError{Code: "billing_not_active", Message: "billing inactive"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: http.StatusBadRequest,
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"limit"}}`)),
				Header:     make(http.Header),
			}

			errWithStatus := HandleErrorResp(resp, func(*http.Response) *types.OpenAIError {
				errCopy := tt.err
				return &errCopy
			}, true, false)

			if errWithStatus == nil || errWithStatus.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("expected status %d, got %+v", http.StatusTooManyRequests, errWithStatus)
			}
		})
	}
}

func TestHandleErrorRespNormalizesNonStringQuotaCode(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"limit"}}`)),
		Header:     make(http.Header),
	}

	errWithStatus := HandleErrorResp(resp, func(*http.Response) *types.OpenAIError {
		return &types.OpenAIError{
			Code:    json.Number("12345"),
			Message: "numeric provider code",
		}
	}, true, false)

	if errWithStatus == nil || errWithStatus.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected non-quota numeric code to preserve status, got %+v", errWithStatus)
	}
}

func TestHandleErrorRespKeepsControlDispositionAfterPublicRedaction(t *testing.T) {
	tests := []struct {
		name          string
		providerError types.OpenAIError
		body          string
		wantStatus    int
		wantQuota     bool
		wantAuth      bool
	}{
		{
			name:          "anthropic exhausted balance",
			providerError: types.OpenAIError{Message: "Your credit balance is too low to access the API"},
			body:          `{"type":"error","error":{"message":"Your credit balance is too low to access the API"}}`,
			wantStatus:    http.StatusTooManyRequests,
			wantQuota:     true,
		},
		{
			name:          "gemini invalid key",
			providerError: types.OpenAIError{Message: "API key not valid. Please pass a valid API key.", Param: "INVALID_ARGUMENT"},
			body:          `{"error":{"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT"}}`,
			wantStatus:    http.StatusBadRequest,
			wantAuth:      true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{
				StatusCode: http.StatusBadRequest,
				Body:       io.NopCloser(strings.NewReader(test.body)),
				Header:     make(http.Header),
			}
			got := HandleErrorResp(response, func(*http.Response) *types.OpenAIError {
				mapped := test.providerError
				return &mapped
			}, false, false)
			if got.StatusCode != test.wantStatus || got.ProviderQuotaExhausted != test.wantQuota || got.ProviderAuthRejected != test.wantAuth {
				t.Fatalf("unexpected provider disposition: %+v", got)
			}
			if got.Code != test.providerError.Code || got.Message != test.providerError.Message {
				t.Fatalf("expected public error to remain redacted: %+v", got)
			}
		})
	}
}

func TestHandleErrorRespDoesNotTurnRateLimitTextIntoQuotaExhaustion(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"Your credit balance is too low but this is only a short rate limit"}}`)),
		Header:     make(http.Header),
	}
	got := HandleErrorResp(response, func(*http.Response) *types.OpenAIError {
		return &types.OpenAIError{
			Type:    "rate_limit_error",
			Code:    "rate_limit_exceeded",
			Message: "Your credit balance is too low but this is only a short rate limit",
		}
	}, false, false)
	if got.ProviderQuotaExhausted || !got.ProviderRateLimited || got.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected explicit rate-limit classification to win over message fallback, got %+v", got)
	}
}

func TestHandleErrorRespDoesNotExposeRawJSONWhenMapperReturnsNil(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       io.NopCloser(strings.NewReader(`{"email":"user@example.com","token":"secret"}`)),
		Header:     make(http.Header),
	}
	resp.Header.Set("Content-Type", "application/json")

	errWithStatus := HandleErrorResp(resp, func(*http.Response) *types.OpenAIError {
		return nil
	}, false, false)

	if errWithStatus == nil {
		t.Fatal("expected normalized upstream error")
	}
	if strings.Contains(errWithStatus.Message, "user@example.com") || strings.Contains(errWithStatus.Message, "secret") {
		t.Fatalf("expected raw JSON body to stay hidden, got %q", errWithStatus.Message)
	}
	if errWithStatus.Message != "bad response status code 502" {
		t.Fatalf("expected generic status message, got %q", errWithStatus.Message)
	}
}
