package responses

import (
	"errors"
	"reflect"
	"testing"

	"one-api/common/requester"
)

func TestEventStreamRequiresAcceptedEventObserver(t *testing.T) {
	if NewEventStream(nil, IgnoreAcceptedResponsesEvent) != nil {
		t.Fatal("nil transport must remain nil")
	}
	transport := &struct {
		requester.StreamReaderInterface[string]
	}{}
	if err := NewEventStream(transport, nil).ObserveAcceptedResponsesEvent("data: {}\n\n"); err == nil {
		t.Fatal("missing accounting observer must fail explicitly")
	}
}

func TestSSEChunkFramerPreservesEverySplitBoundary(t *testing.T) {
	for _, event := range []string{"data: {}\n\n", "event: future\r\ndata: {}\r\n\r\n"} {
		for split := 1; split < len(event); split++ {
			framer := NewSSEChunkFramer(len(event))
			var got []string
			visit := func(raw string) (bool, error) { got = append(got, raw); return false, nil }
			if _, err := framer.PushChunk(event[:split], visit); err != nil || len(got) != 0 {
				t.Fatalf("split %d committed incomplete event: events=%q err=%v", split, got, err)
			}
			if _, err := framer.PushChunk(event[split:], visit); err != nil || !reflect.DeepEqual(got, []string{event}) || framer.HasPending() {
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

func TestStreamObserverCommitsIdentityAndSequenceOnlyAfterAccounting(t *testing.T) {
	observer := NewStreamObserver()
	var identities []string
	observer.SetResponseIDObserver(func(id string) { identities = append(identities, id) })
	created := "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_a\"}}\n\n"
	failure := errors.New("accounting rejected")
	if err := observer.AcceptRawEvent(created, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if observer.ObservedResponseID() != "" || observer.FinalResponse() != nil || observer.ReliableNextSequenceNumber() != nil || len(identities) != 0 {
		t.Fatalf("rejected created event changed lifecycle: %+v identities=%v", observer, identities)
	}
	if err := observer.AcceptRawEvent(created, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	terminal := "data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_a\",\"status\":\"completed\"}}\n\n"
	if err := observer.AcceptRawEvent(terminal, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if observer.TerminalSeen() || observer.FinalResponse().Status == "completed" || *observer.ReliableNextSequenceNumber() != 1 || !reflect.DeepEqual(identities, []string{"resp_a"}) {
		t.Fatalf("rejected terminal changed accepted prefix: %+v identities=%v", observer, identities)
	}
	called := false
	conflict := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_b\",\"status\":\"completed\"}}\n\n"
	if err := observer.AcceptRawEvent(conflict, func() error { called = true; return nil }); err == nil || called {
		t.Fatalf("lifecycle conflict reached accounting: err=%v called=%t", err, called)
	}
}
