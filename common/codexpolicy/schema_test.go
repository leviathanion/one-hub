package codexpolicy

import "testing"

func TestKnownKeyAndResidencyGrammar(t *testing.T) {
	for _, key := range []string{
		KeyFedRAMP,
		KeyResidency,
		KeyDefaultOriginator,
		KeyTrustClientAttestation,
		KeyGenerateProxyInstallationID,
	} {
		if !KnownKey(key) {
			t.Fatalf("expected %q to be a known Codex policy key", key)
		}
	}
	if KnownKey("legacy_profile") {
		t.Fatal("expected legacy_profile to be rejected by shared key schema")
	}

	if !ValidResidency("us-east:fedramp") {
		t.Fatal("expected valid residency grammar")
	}
	if ValidResidency("") {
		t.Fatal("expected empty residency to be handled as optional by callers")
	}
	if ValidResidency("bad value") {
		t.Fatal("expected spaces to be rejected in residency grammar")
	}
}
