package controller

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"one-api/providers/codex"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestExchangeCodexCodeForTokenUsesDefaultUserAgent(t *testing.T) {
	originalTransport := http.DefaultTransport
	t.Cleanup(func() {
		http.DefaultTransport = originalTransport
	})

	http.DefaultTransport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("User-Agent"); got != codex.DefaultUserAgent() {
			t.Fatalf("expected default codex user agent %q, got %q", codex.DefaultUserAgent(), got)
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

		return &http.Response{
			StatusCode: http.StatusOK,
			Body: io.NopCloser(strings.NewReader(`{
				"access_token":"access-token",
				"refresh_token":"refresh-token",
				"token_type":"Bearer",
				"expires_in":3600
			}`)),
			Header: make(http.Header),
		}, nil
	})

	tokenResp, err := exchangeCodexCodeForToken("auth-code", "verifier-123", "state-123", "")
	if err != nil {
		t.Fatalf("expected token exchange to succeed, got %v", err)
	}
	if tokenResp == nil || tokenResp.AccessToken != "access-token" || tokenResp.RefreshToken != "refresh-token" {
		t.Fatalf("expected parsed token response, got %+v", tokenResp)
	}
}
