package authutil

import (
	"net/url"
	"testing"
)

func TestPKCEChallengeUsesS256(t *testing.T) {
	if got := PKCEChallenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("challenge = %s", got)
	}
	a, err := GeneratePKCEVerifier()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GeneratePKCEVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) < 43 || len(a) > 128 || a == b {
		t.Fatal("invalid or repeated verifier")
	}
}

func TestOAuthCallbackRejectsAmbiguousOrForeignURL(t *testing.T) {
	redirect := "http://localhost:51121/oauth-callback"
	for _, suffix := range []string{"?code=ok&state=s&state=s", "?code=ok&code=next&state=s", "?code=ok&state=other", "?code=ok&state=s&x=1&x=2", "?code=ok&state=s#error", "?code=ok&state=s&error=denied", "?code=%zz&state=s"} {
		if _, err := ParseOAuthCallback(redirect+suffix, "s", redirect); err == nil {
			t.Fatalf("accepted %s", suffix)
		}
	}
	for _, input := range []string{"http://evil.test/oauth-callback?code=ok&state=s", "http://localhost:51121/other?code=ok&state=s", "code=ok&state=s", "http://user@localhost:51121/oauth-callback?code=ok&state=s"} {
		if _, err := ParseOAuthCallback(input, "s", redirect); err == nil {
			t.Fatal("accepted foreign callback")
		}
	}
	code := "opaque+/code"
	if got, err := ParseOAuthCallback(redirect+"?code="+url.QueryEscape(code)+"&state=s", "s", redirect); err != nil || got != code {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, err := ParseOAuthCallback("http://codex.test/callback?code=original&state=s&x=1&x=2", "s", ""); err != nil || got != "original" {
		t.Fatal("query-only callback contract changed")
	}
}
