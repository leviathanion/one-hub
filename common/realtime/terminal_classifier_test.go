package realtime

import (
	"testing"

	"one-api/types"
)

func TestClassifyTerminalUsesRealtimeLifecycleOnly(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		status    string
		kind      TerminalKind
		terminal  bool
	}{
		{name: "completed done", eventType: types.EventTypeResponseDone, status: types.ResponseStatusCompleted, kind: TerminalCompleted, terminal: true},
		{name: "cancelled done", eventType: types.EventTypeResponseDone, status: types.ResponseStatusCancelled, kind: TerminalCancelled, terminal: true},
		{name: "failed done", eventType: types.EventTypeResponseDone, status: types.ResponseStatusFailed, kind: TerminalFailed, terminal: true},
		{name: "incomplete done", eventType: types.EventTypeResponseDone, status: types.ResponseStatusIncomplete, kind: TerminalFailed, terminal: true},
		{name: "missing done status", eventType: types.EventTypeResponseDone, kind: TerminalUnknownOutcome, terminal: true},
		{name: "invalid done status", eventType: types.EventTypeResponseDone, status: types.ResponseStatusInProgress, kind: TerminalUnknownOutcome, terminal: true},
		{name: "error event is not a response terminal", eventType: types.EventTypeError, kind: TerminalNonTerminal},
		{name: "responses completed is foreign", eventType: "response.completed", status: types.ResponseStatusCompleted, kind: TerminalNonTerminal},
		{name: "responses failed is foreign", eventType: "response.failed", status: types.ResponseStatusFailed, kind: TerminalNonTerminal},
		{name: "responses incomplete is foreign", eventType: "response.incomplete", status: types.ResponseStatusIncomplete, kind: TerminalNonTerminal},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyTerminal(tc.eventType, tc.status)
			if got.Kind != tc.kind || got.IsTerminal() != tc.terminal {
				t.Fatalf("ClassifyTerminal() = %+v, terminal=%v; want kind=%v terminal=%v", got, got.IsTerminal(), tc.kind, tc.terminal)
			}
		})
	}
}
