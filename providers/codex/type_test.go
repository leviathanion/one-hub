package codex

import (
	"context"
	"errors"
	"fmt"
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
