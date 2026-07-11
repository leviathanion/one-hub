package common

import (
	"strings"
	"testing"
)

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
