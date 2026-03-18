package codex

import (
	"strings"
	"testing"
)

func TestParseAuthFileSupportsCLIProxyStyleJSON(t *testing.T) {
	raw := `{
  "type": "codex",
  "email": "dev@example.com",
  "access_token": "access-token",
  "refresh_token": "refresh-token",
  "account_id": "account-123",
  "expired": "2030-01-02T03:04:05Z"
}`

	parsed, err := ParseAuthFile([]byte(raw), "codex-dev@example.com-plus.json")
	if err != nil {
		t.Fatalf("ParseAuthFile returned error: %v", err)
	}
	if parsed.Email != "dev@example.com" {
		t.Fatalf("expected email to be preserved, got %q", parsed.Email)
	}
	if parsed.DisplayLabel() != "dev@example.com" {
		t.Fatalf("expected display label to prefer email, got %q", parsed.DisplayLabel())
	}
	if parsed.Credentials == nil {
		t.Fatalf("expected credentials to be populated")
	}
	if parsed.Credentials.AccessToken != "access-token" {
		t.Fatalf("unexpected access token: %q", parsed.Credentials.AccessToken)
	}
	if parsed.Credentials.RefreshToken != "refresh-token" {
		t.Fatalf("unexpected refresh token: %q", parsed.Credentials.RefreshToken)
	}
	if parsed.Credentials.AccountID != "account-123" {
		t.Fatalf("unexpected account id: %q", parsed.Credentials.AccountID)
	}
	if parsed.Credentials.ClientID != DefaultClientID {
		t.Fatalf("expected default client id %q, got %q", DefaultClientID, parsed.Credentials.ClientID)
	}
	if parsed.Credentials.ExpiresAt.IsZero() {
		t.Fatalf("expected expiry to be parsed")
	}

	serialized, err := parsed.CredentialsJSON()
	if err != nil {
		t.Fatalf("CredentialsJSON returned error: %v", err)
	}
	if strings.Contains(serialized, "\n") {
		t.Fatalf("expected normalized credentials json to be single-line, got %q", serialized)
	}
}

func TestParseAuthFileRejectsOtherProviderTypes(t *testing.T) {
	_, err := ParseAuthFile([]byte(`{"type":"gemini","access_token":"token"}`), "gemini.json")
	if err == nil {
		t.Fatalf("expected ParseAuthFile to reject non-codex auth files")
	}
}
