package session

import (
	"errors"
	"strings"
	"testing"
)

func TestClientPayloadErrorWithoutCauseDoesNotUnwrapAsSessionClosed(t *testing.T) {
	err := NewClientPayloadError(nil, []byte(`{"type":"error","error":{"message":"payload only"}}`))
	if err == nil {
		t.Fatal("expected payload-only client payload error")
	}
	if errors.Is(err, ErrSessionClosed) {
		t.Fatal("expected payload-only client payload error not to unwrap as ErrSessionClosed")
	}
	if got := string(ClientPayloadFromError(err)); !strings.Contains(got, "payload only") {
		t.Fatalf("expected payload-only client payload error to preserve payload, got %q", got)
	}
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		t.Fatalf("expected payload-only client payload error to unwrap to nil, got %v", unwrapped)
	}
}

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
