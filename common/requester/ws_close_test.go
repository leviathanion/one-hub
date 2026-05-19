package requester

import (
	"encoding/binary"
	"testing"

	"github.com/gorilla/websocket"
)

func TestSafeWSCloseMessageOmitsPayloadForNoStatus(t *testing.T) {
	if payload := SafeWSCloseMessage(websocket.CloseNoStatusReceived, "ignored"); len(payload) != 0 {
		t.Fatalf("expected close-no-status payload to be empty, got %v", payload)
	}
}

func TestSafeWSCloseMessageFormatsCodeAndReason(t *testing.T) {
	payload := SafeWSCloseMessage(websocket.CloseNormalClosure, "ok")
	if len(payload) != 4 {
		t.Fatalf("expected code plus reason payload, got %v", payload)
	}
	if got := int(binary.BigEndian.Uint16(payload[:2])); got != websocket.CloseNormalClosure {
		t.Fatalf("expected close code %d, got %d", websocket.CloseNormalClosure, got)
	}
	if got := string(payload[2:]); got != "ok" {
		t.Fatalf("expected close reason to survive formatting, got %q", got)
	}
}
