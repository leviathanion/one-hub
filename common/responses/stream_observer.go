package responses

import (
	"encoding/json"
	"errors"
	"fmt"
	"one-api/types"
	"strings"
)

type StreamObserver struct {
	finalResponse      *types.OpenAIResponsesResponses
	terminalSeen       bool
	terminalKind       StreamTerminalKind
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
	return &StreamObserver{}
}

func (observer *StreamObserver) SetResponseIDObserver(callback func(string)) {
	if observer != nil {
		observer.responseIDObserver = callback
	}
}

func (observer *StreamObserver) ObserveRawEvent(rawEvent string) {
	if observer == nil || observer.lifecycleError != nil {
		return
	}
	payload, ok := SSEDataPayload(rawEvent)
	if !ok {
		return
	}
	observer.observeSSEPayload(payload)
}

// ObserveEvent 只校验已准入执行的资源归属；计费和交付由各自所有者推进。
func (observer *StreamObserver) ObserveEvent(rawEvent string) error {
	observer.ObserveRawEvent(rawEvent)
	return observer.LifecycleError()
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
	wasResponseTerminal := observer.terminalSeen && observer.terminalKind == StreamTerminalResponse
	if !IsResponseLifecycleEvent(eventType) && eventType != "error" {
		// 未知事件不授予新的资源 owner；其原始字节仍由调用者交付。
		observed.Response = nil
	}
	if eventType != "error" {
		observer.nonErrorEventSeen = true
	}
	responseID := ""
	if observed.Response != nil {
		responseID = strings.TrimSpace(observed.Response.ID)
	}
	if IsTerminalEventType(eventType) && (!observed.ResponseObject || responseID == "" && observer.observedResponseID == "") {
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
		if observer.finalResponse == nil || IsTerminalEventType(eventType) && !wasResponseTerminal {
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
	if observer.terminalSeen && observer.terminalKind == StreamTerminalResponse {
		return
	}
	observer.terminalSeen = true
	observer.terminalKind = kind
}

func (observer *StreamObserver) setLifecycleError(err error) {
	if observer == nil {
		return
	}
	if observer.lifecycleError == nil {
		observer.lifecycleError = err
	}
}

func (observer *StreamObserver) LifecycleError() error {
	if observer == nil {
		return errors.New("responses stream observer is required")
	}
	return observer.lifecycleError
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

func IsResponseLifecycleEvent(eventType string) bool {
	return eventType == "response.created" || eventType == "response.queued" || eventType == "response.in_progress" || IsTerminalEventType(eventType)
}
