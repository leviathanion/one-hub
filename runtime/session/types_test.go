package session

import (
	"errors"
	"strings"
	"testing"
)

func TestFrameConstructorsAndPayloadOwnershipContract(t *testing.T) {
	payload := []byte("hello")
	frame := NewTextFrame(payload)
	if frame.IsZero() {
		t.Fatal("expected constructed text frame not to be zero")
	}
	if frame.Kind() != FrameKindText {
		t.Fatalf("expected text frame kind, got %v", frame.Kind())
	}
	if string(frame.Payload()) != "hello" {
		t.Fatalf("expected payload hello, got %q", frame.Payload())
	}
	cloned := frame.ClonePayload()
	cloned[0] = 'H'
	if string(frame.Payload()) != "hello" {
		t.Fatalf("ClonePayload mutated original payload, got %q", frame.Payload())
	}
	if !((Frame{}).IsZero()) {
		t.Fatal("expected zero Frame to report IsZero")
	}
	if NewBinaryFrame([]byte{1}).Kind() != FrameKindBinary {
		t.Fatal("expected binary frame kind")
	}
}

func TestFrameConstructorsAreOnlyNormalKinds(t *testing.T) {
	frames := []Frame{
		NewTextFrame([]byte("text")),
		NewBinaryFrame([]byte{0x01}),
	}
	for _, frame := range frames {
		switch frame.Kind() {
		case FrameKindText, FrameKindBinary:
		default:
			t.Fatalf("expected constructor to produce only text/binary kind, got %v", frame.Kind())
		}
		if frame.IsZero() {
			t.Fatalf("expected constructed frame not to be zero: %+v", frame)
		}
		if !frame.valid() {
			t.Fatalf("expected constructed frame to be valid: %+v", frame)
		}
	}
}

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
