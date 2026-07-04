package relay

import (
	"strings"
	"testing"
)

type responsesWSTestSecretPanic struct {
	token string
}

func (p responsesWSTestSecretPanic) String() string {
	return "secret=" + p.token
}

func TestResponsesWSPanicClassDoesNotExposeRecoveredValue(t *testing.T) {
	got := responsesWSPanicClass(responsesWSTestSecretPanic{token: "sk-test-secret"})
	if strings.Contains(got, "sk-test-secret") || strings.Contains(got, "secret=") {
		t.Fatalf("panic class exposed recovered value: %q", got)
	}
	if !strings.Contains(got, "responsesWSTestSecretPanic") {
		t.Fatalf("panic class should preserve type, got %q", got)
	}
}

func TestResponsesWSStackHashIsStableSummary(t *testing.T) {
	stack := []byte("goroutine 1 [running]:\nsecret payload")
	got := responsesWSStackHash(stack)
	if len(got) != 32 {
		t.Fatalf("expected 16-byte hex stack hash, got %q", got)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "payload") {
		t.Fatalf("stack hash exposed stack contents: %q", got)
	}
	if got != responsesWSStackHash(stack) {
		t.Fatalf("expected stable stack hash")
	}
}
