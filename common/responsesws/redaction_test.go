package responsesws

import (
	"strings"
	"testing"
)

func TestRedactSensitiveTextRedactsCommonCredentialAndDiagnosticShapes(t *testing.T) {
	input := strings.Join([]string{
		"model gpt-5",
		"sk-testSECRET123",
		"sk-proj-projectSECRET123",
		"api_key=query-secret",
		"token=token-secret",
		"access_token=access-secret",
		"Authorization: Bearer bearer-secret-token",
		"https://provider.example/v1/responses?token=url-secret",
		"session session-secret",
		"x-header header-secret",
		"request body request-secret",
		"response_body response-secret",
		"upstream-url upstream-secret",
		"jwt eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signatureSECRET",
	}, " ")

	got := RedactSensitiveText(input)
	if !strings.Contains(got, "model gpt-5") {
		t.Fatalf("expected non-sensitive diagnostic text to remain, got %q", got)
	}
	lower := strings.ToLower(got)
	for _, forbidden := range []string{
		"sk-testsecret123",
		"sk-proj-projectsecret123",
		"query-secret",
		"token-secret",
		"access-secret",
		"authorization",
		"bearer",
		"provider.example",
		"url-secret",
		"session-secret",
		"header-secret",
		"request-secret",
		"response-secret",
		"upstream-secret",
		"eyjhb",
		"signaturesecret",
	} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("expected %q to be redacted from %q", forbidden, got)
		}
	}
}

func TestRedactSensitiveTextRedactsFullAuthorizationAndCamelCaseTokens(t *testing.T) {
	input := "safe Authorization: Bearer standard-secret; accessToken=camel-secret, RefreshToken=pascal-secret"
	got := RedactSensitiveText(input)
	for _, forbidden := range []string{"standard-secret", "camel-secret", "pascal-secret"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("expected %q to be redacted from %q", forbidden, got)
		}
	}
	if !strings.Contains(got, "safe") {
		t.Fatalf("safe diagnostic text was removed: %q", got)
	}
}

func TestRedactSensitiveTextScansEscapedAndEmbeddedQuotedValues(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		forbidden []string
	}{
		{
			name:      "escaped JSON quote",
			input:     `prefix {"access_token":"secret-part\\\"secret-tail","detail":"safe-tail"}`,
			forbidden: []string{"secret-part", "secret-tail"},
		},
		{
			name:      "JSON embedded in text",
			input:     `outer="{\"access_token\":\"nested-secret\\\"nested-tail\",\"detail\":\"visible-tail\"}"`,
			forbidden: []string{"nested-secret", "nested-tail"},
		},
		{
			name:      "quoted authorization",
			input:     `safe Authorization="Bearer quoted-secret" suffix-visible`,
			forbidden: []string{"quoted-secret", "Bearer"},
		},
		{
			name:      "single quoted camel case token",
			input:     `safe accessToken='single-secret' suffix-visible`,
			forbidden: []string{"single-secret"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactSensitiveText(tt.input)
			for _, forbidden := range tt.forbidden {
				if strings.Contains(got, forbidden) {
					t.Fatalf("sensitive quoted content %q leaked from %q", forbidden, got)
				}
			}
		})
	}
}

func TestRedactSensitiveTextRedactsRemainderOfMalformedSensitiveQuote(t *testing.T) {
	got := RedactSensitiveText(`safe-prefix accessToken='unterminated-secret trailing-secret`)
	if strings.Contains(got, "unterminated-secret") || strings.Contains(got, "trailing-secret") {
		t.Fatalf("unterminated sensitive value leaked: %q", got)
	}
	if !strings.Contains(got, "safe-prefix") || !strings.Contains(got, "[redacted]") {
		t.Fatalf("expected safe prefix and redaction marker, got %q", got)
	}
}
