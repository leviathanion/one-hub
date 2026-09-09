package codex

import (
	"strings"

	"one-api/types"
)

// codexSupplierTerminalKind belongs to the private Codex supplier dialect.
// It must not be reused as either the public Realtime or Responses WebSocket
// lifecycle classifier.
type codexSupplierTerminalKind int

const (
	codexSupplierNonTerminal codexSupplierTerminalKind = iota
	codexSupplierCompletedTerminal
	codexSupplierFailedTerminal
	codexSupplierCancelledTerminal
)

func (k codexSupplierTerminalKind) isTerminal() bool {
	return k != codexSupplierNonTerminal
}

type codexSupplierTerminalEvidence struct {
	kind            codexSupplierTerminalKind
	publicEventType string
	responseStatus  string
}

func interpretCodexSupplierTerminal(eventType string, response *types.OpenAIResponsesResponses) (codexSupplierTerminalEvidence, bool) {
	eventType = strings.TrimSpace(eventType)
	if eventType == types.EventTypeError {
		return codexSupplierTerminalEvidence{}, false
	}
	kind := classifyCodexSupplierTerminal(eventType, response, false)
	if !kind.isTerminal() {
		return codexSupplierTerminalEvidence{}, false
	}

	evidence := codexSupplierTerminalEvidence{kind: kind}
	switch kind {
	case codexSupplierCompletedTerminal:
		evidence.publicEventType = "response.completed"
		evidence.responseStatus = types.ResponseStatusCompleted
	case codexSupplierCancelledTerminal:
		// Public Responses has no cancellation terminal. The adapter caller must
		// fail the transport explicitly instead of fabricating provider failure.
	case codexSupplierFailedTerminal:
		status := ""
		if response != nil {
			status = strings.ToLower(strings.TrimSpace(response.Status))
		}
		if eventType == "response.incomplete" || status == types.ResponseStatusIncomplete {
			evidence.publicEventType = "response.incomplete"
			evidence.responseStatus = types.ResponseStatusIncomplete
		} else {
			evidence.publicEventType = "response.failed"
			evidence.responseStatus = types.ResponseStatusFailed
		}
	default:
		return codexSupplierTerminalEvidence{}, false
	}
	return evidence, true
}

func classifyCodexSupplierTerminal(eventType string, response *types.OpenAIResponsesResponses, hasEventError bool) codexSupplierTerminalKind {
	eventType = strings.TrimSpace(eventType)
	if hasEventError || eventType == types.EventTypeError {
		return codexSupplierNonTerminal
	}

	status := ""
	hasResponseError := false
	if response != nil {
		status = strings.ToLower(strings.TrimSpace(response.Status))
		hasResponseError = response.Error != nil
	}

	switch eventType {
	case types.EventTypeResponseDone, "response.completed":
		if hasResponseError {
			return codexSupplierFailedTerminal
		}
		switch status {
		case "", types.ResponseStatusCompleted:
			return codexSupplierCompletedTerminal
		case types.ResponseStatusCancelled, "canceled":
			return codexSupplierCancelledTerminal
		case types.ResponseStatusFailed, types.ResponseStatusIncomplete:
			return codexSupplierFailedTerminal
		default:
			return codexSupplierNonTerminal
		}
	case "response.cancelled", "response.canceled":
		return codexSupplierCancelledTerminal
	case "response.failed", "response.incomplete":
		return codexSupplierFailedTerminal
	}

	// The private supplier can attach a terminal status to an otherwise
	// non-terminal event name. This compatibility rule stays provider-local.
	switch status {
	case types.ResponseStatusCompleted:
		return codexSupplierCompletedTerminal
	case types.ResponseStatusCancelled, "canceled":
		return codexSupplierCancelledTerminal
	case types.ResponseStatusFailed, types.ResponseStatusIncomplete:
		return codexSupplierFailedTerminal
	default:
		return codexSupplierNonTerminal
	}
}
