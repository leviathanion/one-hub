package codex

import "testing"

func TestCredentialInputPreservesRawTokensAndNormalizesJSONWhitespace(t *testing.T) {
	factory := CodexProviderFactory{}
	for _, test := range []struct{ input, want string }{
		{input: "raw-access-token", want: "raw-access-token"},
		{input: "{\n  \"access_token\": \"access\",\n  \"refresh_token\": \"refresh\"\n}", want: `{"access_token":"access","refresh_token":"refresh"}`},
		{input: "{incomplete", want: "{incomplete"},
	} {
		actual, err := factory.PrepareCredentialInput(test.input)
		if err != nil || actual != test.want {
			t.Fatalf("credential format changed: got=%q err=%v want=%q", actual, err, test.want)
		}
	}
}
