package gemini

import (
	"strings"
	"testing"

	"one-api/model"
)

func TestGeminiAPIVersionReadsJSONOther(t *testing.T) {
	channel := &model.Channel{Other: `{"api_version":"v1"}`}

	if got := geminiAPIVersion(channel); got != "v1" {
		t.Fatalf("expected JSON Other api_version, got %q", got)
	}
}

func TestCleaningErrorRedactsRepeatedAPIKey(t *testing.T) {
	const key = "gemini-secret-key"
	errorInfo := &GeminiError{Message: "request gemini-secret-key failed for gemini-secret-key"}

	cleaningError(errorInfo, key)

	if strings.Contains(errorInfo.Message, key) {
		t.Fatalf("expected repeated Gemini key to be redacted, got %q", errorInfo.Message)
	}
	if got := strings.Count(errorInfo.Message, "xxxxx"); got != 2 {
		t.Fatalf("expected both key occurrences to be redacted, got %d in %q", got, errorInfo.Message)
	}
}
