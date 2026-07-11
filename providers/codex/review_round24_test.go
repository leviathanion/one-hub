package codex

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRawSensitiveFieldNormalizationRedactsNamingVariants(t *testing.T) {
	secrets := []string{
		"camel-access", "pascal-refresh", "dotted-id", "spaced-client", "hyphen-api", "auth-secret",
	}
	raw := map[string]any{
		"accessToken":   secrets[0],
		"RefreshToken":  secrets[1],
		"id.token":      secrets[2],
		"client secret": secrets[3],
		"API-Key":       secrets[4],
		"Authorization": "Bearer " + secrets[5],
		"safe":          "visible",
	}
	encoded, err := json.Marshal(redactUsageCredentialValues(raw, nil))
	if err != nil {
		t.Fatalf("marshal redacted raw: %v", err)
	}
	text := string(encoded)
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Fatalf("raw leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "visible") {
		t.Fatalf("safe raw value was removed: %s", text)
	}
}

func TestTextRedactionCoversAuthorizationValueCamelFieldsAndErrorDescription(t *testing.T) {
	const (
		bearerSecret = "standard-bearer-secret"
		camelSecret  = "camel-token-secret"
		clientSecret = "pascal-client-secret"
	)
	input := `error_description="Authorization: Bearer ` + bearerSecret + `; accessToken=` + camelSecret + `&ClientSecret=` + clientSecret + `"`
	redacted := redactUsageCredentialSecrets(input, nil)
	for _, secret := range []string{bearerSecret, camelSecret, clientSecret} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("text leaked %q: %q", secret, redacted)
		}
	}
	if !strings.Contains(redacted, "[redacted]") {
		t.Fatalf("expected redaction marker: %q", redacted)
	}
}
