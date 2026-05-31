package controller

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExchangeCodexCodeForTokenDoesNotSetUserAgentOrOriginator(t *testing.T) {
	withTokenEndpointTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if values, exists := req.Header["User-Agent"]; exists {
			t.Fatalf("expected token exchange not to send user agent, got %q", values)
		}
		if got := req.Header.Get("Originator"); got != "" {
			t.Fatalf("expected token exchange not to set originator, got %q", got)
		}
		if got := req.Header.Get("Accept"); got != "application/json" {
			t.Fatalf("expected json accept header, got %q", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Fatalf("expected form content type, got %q", got)
		}

		bodyBytes, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("failed to read token exchange request body: %v", err)
		}
		body := string(bodyBytes)
		if !strings.Contains(body, "grant_type=authorization_code") || !strings.Contains(body, "code=auth-code") {
			t.Fatalf("expected form-encoded auth code body, got %q", body)
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
				"access_token":"access-token",
				"refresh_token":"refresh-token",
				"token_type":"Bearer",
				"expires_in":3600
			}`))
	}))

	tokenResp, err := exchangeCodexCodeForToken("auth-code", "verifier-123", "state-123", "")
	if err != nil {
		t.Fatalf("expected token exchange to succeed, got %v", err)
	}
	if tokenResp == nil || tokenResp.AccessToken != "access-token" || tokenResp.RefreshToken != "refresh-token" {
		t.Fatalf("expected parsed token response, got %+v", tokenResp)
	}
}

func withTokenEndpointTLSServer(t *testing.T, handler http.Handler) {
	t.Helper()

	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)

	originalTransport := http.DefaultTransport
	t.Cleanup(func() {
		http.DefaultTransport = originalTransport
	})

	serverAddr := server.Listener.Addr().String()
	http.DefaultTransport = &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, serverAddr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
}
