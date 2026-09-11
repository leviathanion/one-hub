package responses

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"one-api/common/requester"
)

func TestEventStreamRequiresAcceptedEventObserver(t *testing.T) {
	if NewEventStream(nil, IgnoreResponsesEvent) != nil {
		t.Fatal("nil transport must remain nil")
	}
	transport := &struct {
		requester.StreamReaderInterface[string]
	}{}
	if err := NewEventStream(transport, nil).ObserveResponsesEvent("data: {}\n\n"); err == nil {
		t.Fatal("missing accounting observer must fail explicitly")
	}
}

func TestSSEChunkFramerPreservesEverySplitBoundary(t *testing.T) {
	for _, event := range []string{"data: {}\n\n", "event: future\r\ndata: {}\r\n\r\n", "event: future\rdata: {}\r\r", "event: future\rdata: {\n" + "data: }\r\n\r"} {
		for split := 1; split < len(event); split++ {
			framer := NewSSEChunkFramer(len(event))
			var got []string
			visit := func(raw string) (bool, error) { got = append(got, raw); return false, nil }
			if _, err := framer.PushChunk(event[:split], visit); err != nil {
				t.Fatalf("split %d committed incomplete event: events=%q err=%v", split, got, err)
			}
			if _, err := framer.PushChunk(event[split:], visit); err != nil || strings.Join(got, "") != event || framer.HasPending() {
				t.Fatalf("split %d changed framing: events=%q err=%v", split, got, err)
			}
		}
	}
}

func TestSSEChunkFramerBoundsPartialLineAndEventTogether(t *testing.T) {
	framer := NewSSEChunkFramer(12)
	called := false
	visit := func(string) (bool, error) { called = true; return false, nil }
	for _, chunk := range []string{"event:x\n", "data"} {
		if _, err := framer.PushChunk(chunk, visit); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := framer.PushChunk(":", visit); !errors.Is(err, requester.ErrSSEEventTooLarge) || called || framer.HasPending() {
		t.Fatalf("partial line exceeded shared event budget: err=%v called=%t pending=%t", err, called, framer.HasPending())
	}
}

func TestStreamObserverKeepsIdentityIndependentOfAccounting(t *testing.T) {
	observer := NewStreamObserver()
	var identities []string
	observer.SetResponseIDObserver(func(id string) { identities = append(identities, id) })
	created := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_a\"}}\n\n"
	if err := observer.ObserveEvent(created); err != nil {
		t.Fatal(err)
	}
	terminal := "data: {\"type\":\"response.completed\",\"sequence_number\":{},\"response\":{\"id\":\"resp_a\",\"service_tier\":{}}}\n\n"
	if err := observer.ObserveEvent(terminal); err != nil {
		t.Fatal(err)
	}
	if !observer.TerminalSeen() || !reflect.DeepEqual(identities, []string{"resp_a"}) {
		t.Fatalf("lost independent identity: %+v %v", observer, identities)
	}
	conflict := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_b\"}}\n\n"
	if err := observer.ObserveEvent(conflict); err == nil {
		t.Fatal("cross-response evidence accepted")
	}
}
