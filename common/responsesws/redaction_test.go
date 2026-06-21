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
