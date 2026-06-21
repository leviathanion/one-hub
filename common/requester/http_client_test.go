package requester

import (
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
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"usage_limit_reached","message":"limit"}}`)),
		Header:     make(http.Header),
	}

	errWithStatus := HandleErrorResp(resp, func(*http.Response) *types.OpenAIError {
		return &types.OpenAIError{
			Type:    "usage_limit_reached",
			Code:    "usage_limit_reached",
			Message: "limit",
		}
	}, true)

	if errWithStatus == nil || errWithStatus.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected usage_limit_reached to normalize to 429, got %+v", errWithStatus)
	}
}
