package wire

import "testing"

func TestValidationGrammarBoundaries(t *testing.T) {
	if validOriginator("bad value") {
		t.Fatal("originator with spaces must be invalid")
	}
	if !validOriginator("codex_cli_rs.test-1") {
		t.Fatal("expected documented originator token to be valid")
	}
	if validTraceparent("00-00000000000000000000000000000000-0000000000000000-01") {
		t.Fatal("zero trace/span ids must be invalid")
	}
	if !validTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01") {
		t.Fatal("expected W3C traceparent to be valid")
	}
	if validUnixMillisString("99999999999999999999999999999999") {
		t.Fatal("out-of-range unix millis must be invalid")
	}
}
