package requester

import (
	"net/http"
	"testing"
	"time"

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
