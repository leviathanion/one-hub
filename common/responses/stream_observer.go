package responses

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"one-api/types"
	"strings"
)

type StreamObserver struct {
	finalResponse      *types.OpenAIResponsesResponses
	terminalSeen       bool
	terminalKind       StreamTerminalKind
	lastSequence       int64
	hasSequence        bool
	sequenceReliable   bool
	lifecycleError     error
	terminalError      *StreamErrorEvent
	observedResponseID string
	nonErrorEventSeen  bool
	responseIDObserver func(string)
}

type StreamTerminalKind uint8

const (
	StreamTerminalNone StreamTerminalKind = iota
	StreamTerminalResponse
	StreamTerminalError
)

func NewStreamObserver() *StreamObserver {
	return &StreamObserver{sequenceReliable: true}
}

func (observer *StreamObserver) SetResponseIDObserver(callback func(string)) {
	if observer != nil {
		observer.responseIDObserver = callback
	}
}

func (observer *StreamObserver) ObserveRawEvent(rawEvent string) {
	if observer == nil || observer.lifecycleError != nil || observer.terminalSeen {
		return
	}
	payload, ok := SSEDataPayload(rawEvent)
	if !ok {
		return
	}
	observer.observeSSEPayload(payload)
}

// AcceptRawEvent stages lifecycle state, commits provider-local accounting,
// then publishes both as one accepted event. A rejected accounting event does
// not advance sequence, terminal, response identity, or affinity proof state.
func (observer *StreamObserver) AcceptRawEvent(rawEvent string, commitAccounting func() error) error {
	if observer == nil {
		return errors.New("responses stream observer is required")
	}
	if observer.lifecycleError != nil {
		return observer.lifecycleError
	}
	if observer.terminalSeen {
		return nil
	}

	candidate := *observer
	candidate.responseIDObserver = nil
	candidate.ObserveRawEvent(rawEvent)
	if candidate.lifecycleError != nil {
		return candidate.lifecycleError
	}
	if commitAccounting != nil {
		if err := commitAccounting(); err != nil {
			return err
		}
	}

	previousResponseID := observer.observedResponseID
	responseIDObserver := observer.responseIDObserver
	*observer = candidate
	observer.responseIDObserver = responseIDObserver
	if previousResponseID == "" && observer.observedResponseID != "" && responseIDObserver != nil {
		responseIDObserver(observer.observedResponseID)
	}
	return nil
}

func (observer *StreamObserver) observeSSEPayload(payload string) {
	payload = strings.TrimSpace(payload)
	if payload == "" || payload == "[DONE]" {
		return
	}

	observed, err := ObserveEventLifecycle([]byte(payload))
	if err != nil {
		observer.nonErrorEventSeen = true
		// Exact-wire delivery must not depend on the observer understanding every
		// provider event. A later ordinary terminal can still establish the local
		// lifecycle facts the relay owns.
		return
	}
	eventType := strings.TrimSpace(observed.Type)
	if eventType != "error" {
		observer.nonErrorEventSeen = true
	}
	responseID := ""
	if observed.Response != nil {
		responseID = strings.TrimSpace(observed.Response.ID)
	}
	if IsTerminalEventType(eventType) && (!observed.ResponseObject || observed.ResponseFieldError != nil || responseID == "" && observer.observedResponseID == "") {
		// A terminal response ID is a relay-owned ownership and settlement
		// boundary. Unlike provider sequence validation, this cannot be inferred
		// or deferred after the stream ends.
		observer.setLifecycleError(errors.New("responses stream terminal requires response.id"))
		return
	}
	if responseID != "" && observer.observedResponseID != "" && responseID != observer.observedResponseID {
		observer.setLifecycleError(fmt.Errorf("responses stream response.id changed from %s to %s", observer.observedResponseID, responseID))
		return
	}

	sequencePresent := observed.HasSequence
	sequence := observed.Sequence
	if !sequencePresent {
		observer.sequenceReliable = false
	} else if observed.SequenceError != nil {
		observer.sequenceReliable = false
		sequencePresent = false
	}
	if sequencePresent && sequence < 0 {
		observer.sequenceReliable = false
	} else if sequencePresent {
		if observer.hasSequence && sequence <= observer.lastSequence {
			observer.sequenceReliable = false
		} else {
			observer.lastSequence = sequence
			observer.hasSequence = true
		}
	}

	switch eventType {
	case "response.completed", "response.failed", "response.incomplete":
		observer.markTerminal(StreamTerminalResponse)
	case "error":
		var streamError StreamErrorEvent
		if err := json.Unmarshal([]byte(payload), &streamError); err == nil {
			observer.terminalError = &streamError
		}
		observer.markTerminal(StreamTerminalError)
	}

	if observed.Response != nil {
		responseCopy := *observed.Response
		if strings.TrimSpace(responseCopy.ID) == "" && IsTerminalEventType(eventType) {
			responseCopy.ID = observer.observedResponseID
		}
		if observer.finalResponse == nil || IsTerminalEventType(eventType) {
			observer.finalResponse = &responseCopy
		}
		if responseID != "" && observer.observedResponseID == "" {
			observer.observedResponseID = responseID
			if observer.responseIDObserver != nil {
				observer.responseIDObserver(responseID)
			}
		}
	}
}

// ProviderRejected requires an error before any accepted non-error payload.
// Absence of a response ID alone cannot prove that no provider work occurred.
func (observer *StreamObserver) ProviderRejected() bool {
	return observer != nil && observer.terminalKind == StreamTerminalError && observer.observedResponseID == "" && !observer.nonErrorEventSeen
}

func (observer *StreamObserver) markTerminal(kind StreamTerminalKind) {
	if observer == nil {
		return
	}
	if observer.terminalSeen {
		return
	}
	observer.terminalSeen = true
	observer.terminalKind = kind
}

func (observer *StreamObserver) setLifecycleError(err error) {
	if observer == nil {
		return
	}
	observer.sequenceReliable = false
	if observer.lifecycleError == nil {
		observer.lifecycleError = err
	}
}

func (observer *StreamObserver) StreamCompletionError() error {
	if observer == nil {
		return errors.New("responses stream observer is required")
	}
	if observer.lifecycleError != nil {
		return observer.lifecycleError
	}
	if !observer.terminalSeen {
		return errors.New("responses stream ended without a terminal event")
	}
	return nil
}

func (observer *StreamObserver) LifecycleError() error {
	if observer == nil {
		return errors.New("responses stream observer is required")
	}
	return observer.lifecycleError
}

func (observer *StreamObserver) ReliableNextSequenceNumber() *int64 {
	if observer == nil || !observer.hasSequence || !observer.sequenceReliable || observer.lastSequence == math.MaxInt64 {
		return nil
	}
	next := observer.lastSequence + 1
	return &next
}

func (observer *StreamObserver) TerminalSeen() bool {
	return observer != nil && observer.terminalSeen
}

func (observer *StreamObserver) TerminalKind() StreamTerminalKind {
	if observer == nil {
		return StreamTerminalNone
	}
	return observer.terminalKind
}

func (observer *StreamObserver) TerminalError() *StreamErrorEvent {
	if observer == nil || observer.terminalError == nil {
		return nil
	}
	copy := *observer.terminalError
	return &copy
}

func (observer *StreamObserver) ObservedResponseID() string {
	if observer == nil {
		return ""
	}
	return observer.observedResponseID
}

func (observer *StreamObserver) FinalResponse() *types.OpenAIResponsesResponses {
	if observer == nil {
		return nil
	}
	return observer.finalResponse
}

func IsTerminalEventType(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.completed", "response.failed", "response.incomplete":
		return true
	default:
		return false
	}
}
