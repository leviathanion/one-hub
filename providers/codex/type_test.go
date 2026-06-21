package codex

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFromJSONSupportsLegacyExpiredField(t *testing.T) {
	expireAt := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)
	creds, err := FromJSON(`{
		"access_token":"access",
		"refresh_token":"refresh",
		"expired":"` + expireAt.Format(time.RFC3339) + `"
	}`)
	if err != nil {
		t.Fatalf("FromJSON returned error: %v", err)
	}

	if creds.ExpiresAt.IsZero() {
		t.Fatalf("expected expires_at to be parsed from legacy expired field")
	}
	if !creds.ExpiresAt.Equal(expireAt) {
		t.Fatalf("expected expires_at %s, got %s", expireAt.Format(time.RFC3339), creds.ExpiresAt.Format(time.RFC3339))
	}
}

func TestFromJSONSupportsNumericExpiresAtField(t *testing.T) {
	expireAt := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)
	creds, err := FromJSON(fmt.Sprintf(`{
		"access_token":"access",
		"refresh_token":"refresh",
		"expires_at":%d
	}`, expireAt.Unix()))
	if err != nil {
		t.Fatalf("FromJSON returned error: %v", err)
	}

	if !creds.ExpiresAt.Equal(expireAt) {
		t.Fatalf("expected numeric expires_at %s, got %s", expireAt.Format(time.RFC3339), creds.ExpiresAt.Format(time.RFC3339))
	}
}

func TestCodexOAuthDefaultsAlignWithOpenAIAuth(t *testing.T) {
	if DefaultClientID != "app_EMoamEEZ73f0CkXaXp7hrann" {
		t.Fatalf("unexpected Codex client id: %s", DefaultClientID)
	}
	if AuthorizeEndpoint != "https://auth.openai.com/oauth/authorize" {
		t.Fatalf("unexpected Codex authorize endpoint: %s", AuthorizeEndpoint)
	}
	if TokenEndpoint != "https://auth.openai.com/oauth/token" {
		t.Fatalf("unexpected Codex token endpoint: %s", TokenEndpoint)
	}
	if DefaultRedirectURI != "http://localhost:1455/auth/callback" {
		t.Fatalf("unexpected Codex redirect URI: %s", DefaultRedirectURI)
	}
	if DefaultScope != "openid profile email offline_access" {
		t.Fatalf("unexpected Codex scope: %s", DefaultScope)
	}
}

func TestNeedsRefreshWithinLead(t *testing.T) {
	creds := &OAuth2Credentials{
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}

	if !creds.NeedsRefreshWithin(20 * time.Minute) {
		t.Fatalf("expected token to need refresh within 20 minutes")
	}
	if creds.NeedsRefreshWithin(5 * time.Minute) {
		t.Fatalf("expected token to not need refresh within 5 minutes")
	}
}

func TestJoinedScopesOmitsBlankEntries(t *testing.T) {
	scope := joinedScopes([]string{"openid", " ", "", "offline_access"})
	if scope != "openid offline_access" {
		t.Fatalf("expected scopes to be joined without blanks, got %q", scope)
	}
	if joinedScopes(nil) != "" {
		t.Fatalf("expected empty scopes to be omitted")
	}
}

func TestEnsureContextFallsBackToBackground(t *testing.T) {
	if ensureContext(nil) == nil {
		t.Fatalf("expected nil context to be replaced")
	}
}

func TestWaitForRetryHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := waitForRetry(ctx, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("expected cancellation to return promptly, took %v", elapsed)
	}
}

func TestOAuth2CredentialsRefreshDoesNotSetUserAgentOrOriginator(t *testing.T) {
	withTokenEndpointTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if values, exists := req.Header["User-Agent"]; exists {
			t.Fatalf("expected refresh token request not to send user agent, got %q", values)
		}
		if got := req.Header.Get("Originator"); got != "" {
			t.Fatalf("expected refresh token request not to set originator, got %q", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Fatalf("expected form content type, got %q", got)
		}

		bodyBytes, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("failed to read refresh request body: %v", err)
		}
		body := string(bodyBytes)
		if !strings.Contains(body, "grant_type=refresh_token") || !strings.Contains(body, "refresh_token=refresh-token") {
			t.Fatalf("expected form-encoded refresh body, got %q", body)
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
				"access_token":"new-access-token",
				"refresh_token":"new-refresh-token",
				"token_type":"Bearer",
				"expires_in":3600
			}`))
	}))

	creds := &OAuth2Credentials{RefreshToken: "refresh-token"}
	if err := creds.Refresh(context.Background(), "", 0); err != nil {
		t.Fatalf("expected refresh to succeed, got %v", err)
	}
	if creds.AccessToken != "new-access-token" || creds.RefreshToken != "new-refresh-token" {
		t.Fatalf("expected credentials to be updated, got %+v", creds)
	}
}

func TestOAuth2CredentialsRefreshRedactsNonJSONErrorResponse(t *testing.T) {
	withTokenEndpointTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html>upstream refresh failed access-secret refresh-secret client-secret</html>`))
	}))

	creds := &OAuth2Credentials{
		AccessToken:  "access-secret",
		RefreshToken: "refresh-secret",
		ClientID:     "client-secret",
	}
	err := creds.Refresh(context.Background(), "", 0)
	if err == nil {
		t.Fatal("expected refresh failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "token refresh failed with status 502: non-json response") {
		t.Fatalf("expected safe non-json response error, got %q", msg)
	}
	for _, forbidden := range []string{"<html>", "upstream refresh failed", "access-secret", "refresh-secret", "client-secret"} {
		if strings.Contains(msg, forbidden) {
			t.Fatalf("expected refresh error to omit %q, got %q", forbidden, msg)
		}
	}
}

func TestOAuth2CredentialsRefreshPreservesJSONOAuthErrorDetail(t *testing.T) {
	withTokenEndpointTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token expired"}`))
	}))

	creds := &OAuth2Credentials{RefreshToken: "refresh-token"}
	err := creds.Refresh(context.Background(), "", 0)
	if err == nil {
		t.Fatal("expected refresh failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "invalid_grant") || !strings.Contains(msg, "refresh token expired") {
		t.Fatalf("expected OAuth error detail to remain in internal error, got %q", msg)
	}
}

func TestTokenRefreshErrorBodyLogSnippetSanitizesBody(t *testing.T) {
	body := []byte("line\x00\naccess_token=access-secret&refresh_token=refresh-secret&client_id=client-secret " + strings.Repeat("x", tokenRefreshErrorBodyLogLimit+32))
	snippet := tokenRefreshErrorBodyLogSnippet(body, &OAuth2Credentials{
		AccessToken:  "access-secret",
		RefreshToken: "refresh-secret",
		ClientID:     "client-secret",
	}, "client-secret")
	if len(snippet) > tokenRefreshErrorBodyLogLimit {
		t.Fatalf("expected log snippet to be capped at %d bytes, got %d", tokenRefreshErrorBodyLogLimit, len(snippet))
	}
	for _, forbidden := range []string{"\x00", "\n", "access-secret", "refresh-secret", "client-secret"} {
		if strings.Contains(snippet, forbidden) {
			t.Fatalf("expected log snippet to omit %q, got %q", forbidden, snippet)
		}
	}
	for _, r := range snippet {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("expected log snippet to omit control character %q in %q", r, snippet)
		}
	}
	if strings.Count(snippet, "[redacted]") < 3 {
		t.Fatalf("expected known token fields to be redacted, got %q", snippet)
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
