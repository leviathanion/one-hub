package requester

import (
	"encoding/json"
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

	if HTTPClient.Timeout != 300*time.Second {
		t.Fatalf("expected relay timeout to configure client timeout, got %s", HTTPClient.Timeout)
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
			}, true)

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
	}, true)

	if errWithStatus == nil || errWithStatus.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected non-quota numeric code to preserve status, got %+v", errWithStatus)
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
	}, false)

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
