package session

import (
	"strings"
	"testing"
)

func TestBuildBindingKeyRoundTripsEscapedSegments(t *testing.T) {
	callerNS := "auth:tenant/a%2Fb"
	scope := "chat/realtime"
	sessionID := "session/a/b"

	bindingKey := BuildBindingKey(callerNS, scope, sessionID)
	if got := strings.Count(bindingKey, "/"); got != 2 {
		t.Fatalf("expected encoded binding key to contain exactly two separators, got %d in %q", got, bindingKey)
	}

	gotCallerNS, gotScope, gotSessionID, ok := parseBindingKey(bindingKey)
	if !ok {
		t.Fatalf("expected encoded binding key %q to parse", bindingKey)
	}
	if gotCallerNS != callerNS {
		t.Fatalf("expected caller namespace %q, got %q", callerNS, gotCallerNS)
	}
	if gotScope != scope {
		t.Fatalf("expected scope %q, got %q", scope, gotScope)
	}
	if gotSessionID != sessionID {
		t.Fatalf("expected session id %q, got %q", sessionID, gotSessionID)
	}
}
