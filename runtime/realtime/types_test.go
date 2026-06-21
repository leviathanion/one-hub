package realtime

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

func TestClientPayloadErrorHelpersWithCauseAndNilReceiver(t *testing.T) {
	if err := NewClientPayloadError(nil, nil); err != nil {
		t.Fatalf("expected nil payload+cause to produce nil error, got %v", err)
	}
	payloadOnly := NewClientPayloadError(nil, []byte("payload-only"))
	if payloadOnly == nil || payloadOnly.Error() != "payload-only" {
		t.Fatalf("expected payload-only error string, got %v", payloadOnly)
	}

	baseErr := errors.New("base failure")
	originalPayload := []byte(`{"type":"error","message":"payload"}`)
	err := NewClientPayloadError(baseErr, originalPayload)
	if err == nil {
		t.Fatal("expected client payload error")
	}
	originalPayload[0] = '!'
	if got := string(ClientPayloadFromError(err)); got != `{"type":"error","message":"payload"}` {
		t.Fatalf("expected client payload helper to clone payload bytes, got %q", got)
	}
	if err.Error() != baseErr.Error() {
		t.Fatalf("expected client payload error string to follow cause, got %q", err.Error())
	}
	if !errors.Is(err, baseErr) {
		t.Fatal("expected client payload error to unwrap to original cause")
	}

	var nilErr *ClientPayloadError
	if nilErr.Error() != "" {
		t.Fatalf("expected nil ClientPayloadError string to be empty, got %q", nilErr.Error())
	}
	if nilErr.Unwrap() != nil {
		t.Fatal("expected nil ClientPayloadError unwrap to return nil")
	}
	if payload := ClientPayloadFromError(baseErr); payload != nil {
		t.Fatalf("expected non-payload errors to return nil payload, got %q", string(payload))
	}
}
