package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type round23SecretError struct{ secret string }

func (e *round23SecretError) Error() string { return "refresh failed: " + e.secret }

func TestMalformedEscapedRawIsNeverReturned(t *testing.T) {
	const secret = "escaped-secret"
	body := []byte(`{"safe":"value","access_token":"` + secret + `"`)
	credentials := &OAuth2Credentials{AccessToken: secret}

	snapshot, err := normalizeUsageSnapshot(23, credentials, 502, body)
	if err == nil || snapshot == nil || snapshot.Raw != invalidCodexRawPlaceholder {
		t.Fatalf("usage malformed raw was not omitted: snapshot=%+v err=%v", snapshot, err)
	}
	result, err := normalizeResetCreditResultWithCredentials(23, credentials, 502, body)
	if err == nil || result == nil {
		t.Fatalf("expected minimal reset failure result: result=%+v err=%v", result, err)
	}
	for _, value := range []any{snapshot.Raw, result} {
		encoded, _ := json.Marshal(value)
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("malformed response leaked secret: %s", encoded)
		}
	}
}

func TestRawAndErrorTextRedactUnknownSensitiveFieldsAndKnownClientID(t *testing.T) {
	credentials := &OAuth2Credentials{AccessToken: "old-access", RefreshToken: "old-refresh", ClientID: "known-client"}
	raw := map[string]any{
		"safe":          "visible",
		"id_token":      "brand-new-id-token",
		"authorization": "Bearer brand-new-access",
		"nested": map[string]any{
			"client_secret":              "brand-new-client-secret",
			"prefix-known-client-suffix": "visible",
		},
	}
	redacted := redactUsageCredentialValues(raw, credentials)
	encoded, _ := json.Marshal(redacted)
	text := string(encoded)
	for _, secret := range []string{"brand-new-id-token", "brand-new-access", "brand-new-client-secret", "known-client"} {
		if strings.Contains(text, secret) {
			t.Fatalf("raw leaked %q: %s", secret, text)
		}
	}
	message := redactUsageCredentialSecrets(`request failed authorization=Bearer-new id_token=new-id client_secret=new-secret`, credentials)
	for _, secret := range []string{"Bearer-new", "new-id", "new-secret"} {
		if strings.Contains(message, secret) {
			t.Fatalf("error text leaked %q: %q", secret, message)
		}
	}
}

func TestSanitizedTokenRefreshErrorDoesNotExposeCause(t *testing.T) {
	sentinel := errors.New("sentinel")
	cause := &round23SecretError{secret: "cause-secret"}
	wrapped := sanitizeTokenRefreshError(errors.Join(context.DeadlineExceeded, sentinel, cause), &OAuth2Credentials{AccessToken: "cause-secret"}, "")
	if !errors.Is(wrapped, context.DeadlineExceeded) || !errors.Is(wrapped, sentinel) {
		t.Fatalf("sanitized error lost Is semantics: %v", wrapped)
	}
	if errors.Unwrap(wrapped) != nil {
		t.Fatalf("secret cause remained unwrap-reachable: %v", errors.Unwrap(wrapped))
	}
	var exposed *round23SecretError
	if errors.As(wrapped, &exposed) {
		t.Fatalf("secret cause remained As-reachable: %+v", exposed)
	}
	if strings.Contains(wrapped.Error(), "cause-secret") {
		t.Fatalf("sanitized message leaked cause: %q", wrapped.Error())
	}
}
