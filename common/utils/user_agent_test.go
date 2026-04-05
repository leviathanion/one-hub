package utils

import (
	"strings"
	"testing"
)

func TestNormalizeUserAgent(t *testing.T) {
	if got := NormalizeUserAgent("  Codex/1.2  "); got != "Codex/1.2" {
		t.Fatalf("expected whitespace-trimmed user-agent, got %q", got)
	}
	if got := NormalizeUserAgent("   "); got != "" {
		t.Fatalf("expected blank user-agent to normalize to empty string, got %q", got)
	}

	longUA := strings.Repeat("a", maxNormalizedUserAgentLength+32)
	got := NormalizeUserAgent(longUA)
	if len([]rune(got)) != maxNormalizedUserAgentLength {
		t.Fatalf("expected normalized user-agent to be truncated to %d characters, got %d", maxNormalizedUserAgentLength, len([]rune(got)))
	}
}

func TestAppendUserAgentMetadata(t *testing.T) {
	if got := AppendUserAgentMetadata(nil, " "); got != nil {
		t.Fatalf("expected empty user-agent to leave nil metadata untouched, got %#v", got)
	}

	meta := map[string]any{"source": "relay"}
	got := AppendUserAgentMetadata(meta, " Codex/1.2 ")
	if got["source"] != "relay" || got["user_agent"] != "Codex/1.2" {
		t.Fatalf("expected metadata merge to preserve existing keys and add user_agent, got %#v", got)
	}
}
