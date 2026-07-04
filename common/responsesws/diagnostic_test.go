package responsesws

import (
	"strings"
	"testing"
)

func TestDiagnosticStackHashIsStableSummary(t *testing.T) {
	stack := []byte("goroutine 1 [running]:\nsecret payload")
	got := diagnosticStackHash(stack)
	if len(got) != 32 {
		t.Fatalf("expected 16-byte hex stack hash, got %q", got)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "payload") {
		t.Fatalf("stack hash exposed stack contents: %q", got)
	}
	if got != diagnosticStackHash(stack) {
		t.Fatal("expected stable stack hash")
	}
}
