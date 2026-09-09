package common

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactSensitiveJSONPreservesEnvelopeAndSafeFields(t *testing.T) {
	input := []byte(`{"type":"error","message":"upstream rejected Bearer abcdefghijklmnop","error":{"access_token":"provider-secret","code":"invalid_api_key"},"items":[{"authorization":"Basic hidden"}],"count":12345678901234567890}`)
	redacted, changed := RedactSensitiveJSON(input)
	if !changed {
		t.Fatal("expected credential-bearing JSON to be redacted")
	}
	for _, secret := range []string{"abcdefghijklmnop", "provider-secret", "Basic hidden"} {
		if strings.Contains(string(redacted), secret) {
			t.Fatalf("secret %q leaked from %s", secret, redacted)
		}
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(redacted, &payload); err != nil {
		t.Fatalf("redacted payload is not valid JSON: %v", err)
	}
	if string(payload["count"]) != "12345678901234567890" {
		t.Fatalf("number changed during redaction: %s", payload["count"])
	}
	if !strings.Contains(string(payload["error"]), `"code":"invalid_api_key"`) {
		t.Fatalf("safe error code was lost: %s", payload["error"])
	}
}

func TestRedactSensitiveJSONReturnsOriginalWhenSafe(t *testing.T) {
	input := []byte(`{ "type": "error", "message": "rate limited" }`)
	redacted, changed := RedactSensitiveJSON(input)
	if changed || string(redacted) != string(input) {
		t.Fatalf("safe JSON changed: changed=%v payload=%s", changed, redacted)
	}
}

func TestRedactSensitiveJSONRemovesProviderAccountIdentity(t *testing.T) {
	input := []byte(`{"type":"error","account_id":"acct-secret","organization_id":"org-secret","project_id":"proj-secret","tenant_id":"tenant-secret","message":"safe"}`)
	redacted, changed := RedactSensitiveJSON(input)
	if !changed {
		t.Fatal("expected provider identity fields to be redacted")
	}
	for _, secret := range []string{"acct-secret", "org-secret", "proj-secret", "tenant-secret"} {
		if strings.Contains(string(redacted), secret) {
			t.Fatalf("provider identity %q leaked from %s", secret, redacted)
		}
	}
	if !strings.Contains(string(redacted), `"message":"safe"`) {
		t.Fatalf("safe provider error fields were lost: %s", redacted)
	}
}

func TestRedactProviderMetadataJSONPreservesSuccessfulPayloadSemantics(t *testing.T) {
	input := []byte(`{"account_id":"acct-secret","choices":[{"message":{"content":"line 1\n  line 2 mentions session token"},"logprobs":{"content":[{"token":" hello","logprob":-0.1}]}}]}`)
	redacted, changed := RedactProviderMetadataJSON(input)
	if !changed {
		t.Fatal("expected provider account metadata to be redacted")
	}

	var payload struct {
		AccountID string `json:"account_id"`
		Choices   []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Logprobs struct {
				Content []struct {
					Token string `json:"token"`
				} `json:"content"`
			} `json:"logprobs"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(redacted, &payload); err != nil {
		t.Fatalf("decode redacted payload: %v", err)
	}
	if payload.AccountID != "[redacted]" {
		t.Fatalf("provider account metadata was not redacted: %s", redacted)
	}
	if got := payload.Choices[0].Message.Content; got != "line 1\n  line 2 mentions session token" {
		t.Fatalf("successful response content changed: %q", got)
	}
	if got := payload.Choices[0].Logprobs.Content[0].Token; got != " hello" {
		t.Fatalf("logprobs token changed: %q", got)
	}
}

func TestRedactSensitiveJSONRedactsExplicitErrorCredentialFields(t *testing.T) {
	input := []byte(`{"type":"error","message":"invalid tool","secret":"shared-secret","credential":"shared-credential","nested":{"credentials":"shared-credentials"}}`)
	redacted, changed := RedactSensitiveJSON(input)
	if !changed {
		t.Fatal("expected explicit error credential fields to be redacted")
	}
	for _, value := range []string{"shared-secret", "shared-credential", "shared-credentials"} {
		if strings.Contains(string(redacted), value) {
			t.Fatalf("error credential %q leaked from %s", value, redacted)
		}
	}
	if !strings.Contains(string(redacted), `"message":"invalid tool"`) {
		t.Fatalf("ordinary error semantics changed: %s", redacted)
	}
}

func TestRedactProviderMetadataJSONDoesNotApplyErrorOnlyLabels(t *testing.T) {
	input := []byte(`{"object":"provider.result","secret":"public-result","credential":"public-credential"}`)
	redacted, changed := RedactProviderMetadataJSON(input)
	if changed || string(redacted) != string(input) {
		t.Fatalf("successful provider payload used error-only redaction: changed=%v payload=%s", changed, redacted)
	}
}

func TestRedactProviderMetadataJSONDoesNotTraverseModelOutput(t *testing.T) {
	input := []byte(`{"account_id":"acct-secret","response":{"project_id":"proj-secret","output":[{"authorization":"model-authored","api_key":"example-value"}]},"choices":[{"message":{"content":{"access_token":"model-authored-token"}}}]}`)
	redacted, changed := RedactProviderMetadataJSON(input)
	if !changed {
		t.Fatal("expected envelope metadata to be redacted")
	}
	for _, secret := range []string{"acct-secret", "proj-secret"} {
		if strings.Contains(string(redacted), secret) {
			t.Fatalf("provider metadata %q leaked from %s", secret, redacted)
		}
	}
	for _, authored := range []string{"model-authored", "example-value", "model-authored-token"} {
		if !strings.Contains(string(redacted), authored) {
			t.Fatalf("model-authored structured content %q changed in %s", authored, redacted)
		}
	}
}

func TestRedactSensitiveAssignmentsUsesEscapeAwareQuotedBoundary(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		forbidden []string
		want      string
	}{
		{
			name:      "legal JSON escaped quote",
			input:     `prefix {"access_token":"secret-part\"secret-tail","detail":"safe-tail"}`,
			forbidden: []string{"secret-part", "secret-tail"},
			want:      `"detail":"safe-tail"`,
		},
		{
			name:      "JSON embedded in text",
			input:     `outer="{\"access_token\":\"nested-secret\\\"nested-tail\",\"detail\":\"visible-tail\"}"`,
			forbidden: []string{"nested-secret", "nested-tail"},
			want:      `\"detail\":\"visible-tail\"`,
		},
		{
			name:      "quoted authorization",
			input:     `safe Authorization="Bearer quoted-secret" suffix-visible`,
			forbidden: []string{"quoted-secret"},
			want:      `Authorization="[redacted]" suffix-visible`,
		},
		{
			name:      "single quoted camel case",
			input:     `safe accessToken='single-secret' suffix-visible`,
			forbidden: []string{"single-secret"},
			want:      `accessToken='[redacted]' suffix-visible`,
		},
		{
			name:      "newline after delimiter",
			input:     "prefix {\"access_token\":\n\t\"newline-secret\",\n\t\"detail\":\"visible-newline-tail\"}",
			forbidden: []string{"newline-secret"},
			want:      `"detail":"visible-newline-tail"`,
		},
		{
			name:      "CRLF after delimiter",
			input:     "prefix {\"authorization\":\r\n \"Bearer crlf-secret\",\r\n \"detail\":\"visible-crlf-tail\"}",
			forbidden: []string{"crlf-secret"},
			want:      `"detail":"visible-crlf-tail"`,
		},
		{
			name:      "nested object with escaped value",
			input:     `prefix {"wrapper":{"access_token":"deep-secret\\\"deep-secret-tail","detail":"visible-deep-tail"}}`,
			forbidden: []string{"deep-secret", "deep-secret-tail"},
			want:      `"detail":"visible-deep-tail"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactSensitiveAssignments(tt.input)
			for _, forbidden := range tt.forbidden {
				if strings.Contains(got, forbidden) {
					t.Fatalf("quoted secret %q leaked from %q", forbidden, got)
				}
			}
			if !strings.Contains(got, tt.want) {
				t.Fatalf("expected closing boundary and safe suffix %q in %q", tt.want, got)
			}
		})
	}
}

func TestRedactSensitiveAssignmentsRedactsFoldedAuthorizationAndEscapedKeys(t *testing.T) {
	tests := []string{
		"safe Authorization: Bearer\r\n folded-continuation-secret\r\nX-Debug: visible",
		`safe {"access\u005ftoken":"unicode-key-secret","detail":"visible"}`,
		`safe {"access\\u005ftoken":"double-escaped-unicode-secret","detail":"visible"}`,
		`outer="{\"access\\u005ftoken\":\"nested-unicode-secret\",\"detail\":\"visible\"}"`,
	}
	for _, input := range tests {
		got := RedactSensitiveAssignments(input)
		for _, secret := range []string{"folded-continuation-secret", "unicode-key-secret", "double-escaped-unicode-secret", "nested-unicode-secret"} {
			if strings.Contains(got, secret) {
				t.Fatalf("secret %q leaked from %q as %q", secret, input, got)
			}
		}
		if !strings.Contains(got, "safe") && !strings.Contains(got, "outer") {
			t.Fatalf("redaction discarded all safe context: %q", got)
		}
	}
}

func TestRedactSensitiveAssignmentsConsumesMalformedQuotedRemainder(t *testing.T) {
	got := RedactSensitiveAssignments(`safe accessToken='secret tail-with-secret`)
	if got != `safe accessToken='[redacted]` {
		t.Fatalf("unexpected malformed quoted redaction: %q", got)
	}
}

func TestRedactSensitiveTextRemovesProviderIdentityFreeText(t *testing.T) {
	input := "request rejected for organization org-secret_1, project proj_secret-2 and account acct_secret-3"
	got := RedactSensitiveText(input)
	for _, secret := range []string{"org-secret_1", "proj_secret-2", "acct_secret-3"} {
		if strings.Contains(got, secret) {
			t.Fatalf("provider identity %q leaked from %q", secret, got)
		}
	}
	for _, label := range []string{"organization", "project", "account"} {
		if !strings.Contains(got, label+" [redacted]") {
			t.Fatalf("diagnostic label %q was not retained in %q", label, got)
		}
	}
}

func TestRedactCredentialValuesTextMatchesOnlyRealSecret(t *testing.T) {
	got, changed := RedactCredentialValuesText("model mentions api_key and actual provider-secret-123", "provider-secret-123")
	if !changed || strings.Contains(got, "provider-secret-123") || !strings.Contains(got, "model mentions api_key") {
		t.Fatalf("credential value redaction changed the wrong text: %q changed=%v", got, changed)
	}
	if unchanged, changed := RedactCredentialValuesText("ordinary sk-example-like text", "short"); changed || unchanged != "ordinary sk-example-like text" {
		t.Fatalf("short/non-matching credential changed text: %q changed=%v", unchanged, changed)
	}
}

func TestRedactSensitiveTextRedactsNamespacedCredentialLabels(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		secret string
	}{
		{
			name:   "provider api key",
			input:  "safe openai-api-key provider-secret visible-tail",
			secret: "provider-secret",
		},
		{
			name:   "provider access token",
			input:  "safe codex-access-token token-secret visible-tail",
			secret: "token-secret",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactSensitiveText(tt.input)
			if strings.Contains(got, tt.secret) {
				t.Fatalf("namespaced credential %q leaked from %q", tt.secret, got)
			}
			if !strings.Contains(got, "safe") || !strings.Contains(got, "visible-tail") {
				t.Fatalf("redaction discarded safe context: %q", got)
			}
		})
	}
}

func TestRedactSensitiveTextPreservesNonCredentialCompoundLabels(t *testing.T) {
	input := "invalid_api_key token-budget 128 request-api-latency 42ms"
	if got := RedactSensitiveText(input); got != input {
		t.Fatalf("non-credential diagnostic labels were redacted: %q", got)
	}
}
