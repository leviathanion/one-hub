package requester

import (
	"errors"
	"testing"
)

func TestSSEEventFramerPreservesCompleteEventWire(t *testing.T) {
	framer := NewSSEEventFramer(1024)
	for _, line := range []string{"event: speech.audio.delta\r\n", "data: {\"type\":\"speech.audio.delta\"}\r\n"} {
		if event, complete, err := framer.PushLine([]byte(line)); err != nil || complete || event != nil {
			t.Fatalf("premature frame: event=%q complete=%v err=%v", event, complete, err)
		}
	}
	event, complete, err := framer.PushLine([]byte("\r\n"))
	if err != nil || !complete || string(event) != "event: speech.audio.delta\r\ndata: {\"type\":\"speech.audio.delta\"}\r\n\r\n" {
		t.Fatalf("framed event changed: event=%q complete=%v err=%v", event, complete, err)
	}
}

func TestSSEEventFramerRejectsOversizedEvent(t *testing.T) {
	framer := NewSSEEventFramer(8)
	if _, _, err := framer.PushLine([]byte("data: 123\n")); !errors.Is(err, ErrSSEEventTooLarge) {
		t.Fatalf("expected bounded event error, got %v", err)
	}
}
