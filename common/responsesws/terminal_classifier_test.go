package responsesws

import "testing"

func TestClassifyResponsesWSEventTerminalCases(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		kind    ResponsesTerminalKind
		miss    bool
	}{
		{
			name:    "completed success without status",
			payload: `{"type":"response.completed","response":{"id":"resp_1"}}`,
			kind:    ResponsesSuccessTerminal,
		},
		{
			name:    "done success alias",
			payload: `{"type":"response.done","response":{"id":"resp_1","status":"completed"}}`,
			kind:    ResponsesSuccessTerminal,
		},
		{
			name:    "done with top level error",
			payload: `{"type":"response.done","error":{"code":"bad","message":"bad request"},"response":{"id":"resp_1"}}`,
			kind:    ResponsesFailedTerminal,
		},
		{
			name:    "completed with response error",
			payload: `{"type":"response.completed","response":{"id":"resp_1","error":{"code":"bad","message":"bad request"}}}`,
			kind:    ResponsesFailedTerminal,
		},
		{
			name:    "cancelled status",
			payload: `{"type":"response.done","response":{"id":"resp_1","status":"cancelled"}}`,
			kind:    ResponsesCancelledTerminal,
		},
		{
			name:    "cancelled event stays cancelled",
			payload: `{"type":"response.cancelled","response":{"id":"resp_1","status":"cancelled"}}`,
			kind:    ResponsesCancelledTerminal,
		},
		{
			name:    "continuation miss from message",
			payload: `{"type":"error","error":{"message":"previous response was not found"}}`,
			kind:    ResponsesFailedTerminal,
			miss:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyResponsesWSEvent([]byte(tc.payload))
			if got.Kind != tc.kind {
				t.Fatalf("expected kind %d, got %d", tc.kind, got.Kind)
			}
			if got.ContinuationMiss != tc.miss {
				t.Fatalf("expected continuation miss %v, got %v", tc.miss, got.ContinuationMiss)
			}
			if tc.kind != ResponsesNonTerminal && got.EventType == "" {
				t.Fatal("expected event type to be recorded")
			}
			if tc.name == "cancelled event stays cancelled" && string(got.NormalizedPayload) != "" {
				t.Fatalf("expected cancelled terminal not to normalize to failed payload, got %s", got.NormalizedPayload)
			}
		})
	}
}

func TestClassifyResponsesWSEventMalformedKnownTerminalResponseShape(t *testing.T) {
	got := ClassifyResponsesWSEvent([]byte(`{"type":"response.completed","response":"opaque"}`))
	if !got.Malformed || got.Kind != ResponsesFailedTerminal || got.MalformedError == "" {
		t.Fatalf("expected known terminal with non-object response to be malformed, got %+v", got)
	}

	future := ClassifyResponsesWSEvent([]byte(`{"type":"response.future","response":"opaque"}`))
	if future.Malformed || future.Kind != ResponsesNonTerminal {
		t.Fatalf("expected future event with opaque response to remain passthrough non-terminal, got %+v", future)
	}
}
