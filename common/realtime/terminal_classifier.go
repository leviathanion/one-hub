// Package realtime owns OpenAI Realtime wire semantics. Runtime session and
// transport abstractions remain in runtime/realtime.
package realtime

import (
	"strings"

	"one-api/types"
)

// TerminalKind is the outcome of an OpenAI Realtime lifecycle event. It is
// intentionally distinct from Responses streaming and Responses WebSocket
// terminal kinds.
type TerminalKind int

const (
	TerminalNonTerminal TerminalKind = iota
	TerminalCompleted
	TerminalFailed
	TerminalCancelled
	// TerminalUnknownOutcome means the Realtime event is terminal but its
	// response status cannot establish a documented outcome.
	TerminalUnknownOutcome
)

type TerminalResult struct {
	Kind      TerminalKind
	EventType string
	Status    string
}

func (r TerminalResult) IsTerminal() bool {
	return r.Kind != TerminalNonTerminal
}

// ClassifyTerminal classifies only the OpenAI Realtime server-event model.
// response.done is always terminal; its nested response.status determines the
// outcome. Responses streaming events are deliberately not aliases here.
func ClassifyTerminal(eventType string, status string) TerminalResult {
	result := TerminalResult{
		Kind:      TerminalNonTerminal,
		EventType: strings.TrimSpace(eventType),
		Status:    strings.ToLower(strings.TrimSpace(status)),
	}
	if result.EventType != types.EventTypeResponseDone {
		return result
	}
	switch result.Status {
	case types.ResponseStatusCompleted:
		result.Kind = TerminalCompleted
	case types.ResponseStatusCancelled:
		result.Kind = TerminalCancelled
	case types.ResponseStatusFailed, types.ResponseStatusIncomplete:
		result.Kind = TerminalFailed
	default:
		result.Kind = TerminalUnknownOutcome
	}
	return result
}
