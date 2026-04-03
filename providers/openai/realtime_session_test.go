package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/model"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/gorilla/websocket"
)

type recordingOpenAIRealtimeObserver struct {
	mu        sync.Mutex
	observed  []*types.UsageEvent
	finalized []runtimesession.TurnFinalizePayload
}

type blockingOpenAIRealtimeObserver struct {
	observeStarted      chan struct{}
	allowObserveReturn  chan struct{}
	finalizeCalled      chan struct{}
	observeStartedOnce  sync.Once
	allowObserveOnce    sync.Once
	finalizeCalledOnce  sync.Once
	mu                  sync.Mutex
	observeCount        int
	finalizeCount       int
	observeCompleted    bool
	finalizedBeforeDone bool
}

type failingOpenAIRealtimeObserver struct {
	recordingOpenAIRealtimeObserver
	observeErr error
}

func (r *recordingOpenAIRealtimeObserver) ObserveTurnUsage(usage *types.UsageEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observed = append(r.observed, usage.Clone())
	return nil
}

func (r *recordingOpenAIRealtimeObserver) FinalizeTurn(payload runtimesession.TurnFinalizePayload) {
	r.mu.Lock()
	defer r.mu.Unlock()
	payload.Usage = payload.Usage.Clone()
	r.finalized = append(r.finalized, payload)
}

func (r *recordingOpenAIRealtimeObserver) observeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.observed)
}

func (r *recordingOpenAIRealtimeObserver) finalizeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.finalized)
}

func (r *recordingOpenAIRealtimeObserver) lastPayload() runtimesession.TurnFinalizePayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.finalized) == 0 {
		return runtimesession.TurnFinalizePayload{}
	}
	return r.finalized[len(r.finalized)-1]
}

func (r *recordingOpenAIRealtimeObserver) lastObservedUsage() *types.UsageEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.observed) == 0 {
		return nil
	}
	return r.observed[len(r.observed)-1].Clone()
}

func newBlockingOpenAIRealtimeObserver() *blockingOpenAIRealtimeObserver {
	return &blockingOpenAIRealtimeObserver{
		observeStarted:     make(chan struct{}),
		allowObserveReturn: make(chan struct{}),
		finalizeCalled:     make(chan struct{}),
	}
}

func (o *blockingOpenAIRealtimeObserver) ObserveTurnUsage(usage *types.UsageEvent) error {
	_ = usage
	o.mu.Lock()
	o.observeCount++
	o.mu.Unlock()

	o.observeStartedOnce.Do(func() {
		close(o.observeStarted)
	})
	<-o.allowObserveReturn

	o.mu.Lock()
	o.observeCompleted = true
	o.mu.Unlock()
	return nil
}

func (o *blockingOpenAIRealtimeObserver) FinalizeTurn(payload runtimesession.TurnFinalizePayload) {
	_ = payload
	o.mu.Lock()
	o.finalizeCount++
	if !o.observeCompleted {
		o.finalizedBeforeDone = true
	}
	o.mu.Unlock()

	o.finalizeCalledOnce.Do(func() {
		close(o.finalizeCalled)
	})
}

func (o *blockingOpenAIRealtimeObserver) releaseObserve() {
	if o == nil {
		return
	}
	o.allowObserveOnce.Do(func() {
		close(o.allowObserveReturn)
	})
}

func (r *failingOpenAIRealtimeObserver) ObserveTurnUsage(usage *types.UsageEvent) error {
	_ = r.recordingOpenAIRealtimeObserver.ObserveTurnUsage(usage)
	return r.observeErr
}

func waitForOpenAIRealtimeFinalize(t *testing.T, recorder *recordingOpenAIRealtimeObserver, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if recorder.finalizeCount() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected finalize count %d within %v, got %d", want, timeout, recorder.finalizeCount())
}

func TestOpenAIRealtimeSessionNormalizesResponseCreateModel(t *testing.T) {
	receivedPayload := make(chan []byte, 1)
	server := newOpenAIRealtimeTestServer(t, func(conn *websocket.Conn) {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("failed to read upstream request: %v", err)
			return
		}
		receivedPayload <- payload
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected realtime session to open, got %v", errWithCode)
	}
	defer session.Abort("test_cleanup")

	if err := session.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.create","response":{"input":[]}}`)); err != nil {
		t.Fatalf("expected normalized response.create to send, got %v", err)
	}

	select {
	case payload := <-receivedPayload:
		var message map[string]any
		if err := json.Unmarshal(payload, &message); err != nil {
			t.Fatalf("expected upstream request payload to be valid JSON, got %v", err)
		}
		response, _ := message["response"].(map[string]any)
		if got := strings.TrimSpace(anyToString(response["model"])); got != "gpt-4o-realtime-preview" {
			t.Fatalf("expected normalized response.create to backfill session model, got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for normalized upstream request")
	}
}

func TestOpenAIRealtimeSessionForwardsBootstrapAndFinalizesTurn(t *testing.T) {
	releaseDone := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *websocket.Conn) {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created","session":{"id":"sess_123"}}`)); err != nil {
			t.Errorf("failed to write bootstrap event: %v", err)
			return
		}
		_, _, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("failed to read response.create request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_123","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`)); err != nil {
			t.Errorf("failed to write terminal response event: %v", err)
			return
		}
		<-releaseDone
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected realtime session to open, got %v", errWithCode)
	}
	defer func() {
		close(releaseDone)
		session.Abort("test_cleanup")
	}()

	recorder := &recordingOpenAIRealtimeObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })

	messageType, payload, usage, err := session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected bootstrap recv without error, got %v", err)
	}
	if messageType != websocket.TextMessage || !strings.Contains(string(payload), `"session.created"`) {
		t.Fatalf("expected first recv to be session.created, got type=%d payload=%q", messageType, payload)
	}
	if usage != nil {
		t.Fatalf("expected bootstrap not to carry usage, got %+v", usage)
	}

	if err := session.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.create","response":{"input":[]}}`)); err != nil {
		t.Fatalf("expected response.create send to succeed, got %v", err)
	}

	_, payload, usage, err = session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected terminal recv without error, got %v", err)
	}
	if !strings.Contains(string(payload), `"response.done"`) {
		t.Fatalf("expected second recv to be response.done, got %q", payload)
	}
	if usage == nil || usage.TotalTokens != 8 {
		t.Fatalf("expected terminal usage to be forwarded, got %+v", usage)
	}
	if recorder.observeCount() != 1 {
		t.Fatalf("expected observer to record exactly one usage snapshot, got %d", recorder.observeCount())
	}
	if recorder.finalizeCount() != 1 {
		t.Fatalf("expected observer to finalize exactly one turn, got %d", recorder.finalizeCount())
	}
	if got := recorder.lastPayload().LastResponseID; got != "resp_123" {
		t.Fatalf("expected finalized turn to preserve response id, got %q", got)
	}
}

func TestOpenAIRealtimeSessionDeduplicatesUsageSnapshots(t *testing.T) {
	releaseDone := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *websocket.Conn) {
		_, _, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("failed to read response.create request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_snapshot","status":"in_progress","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`)); err != nil {
			t.Errorf("failed to write snapshot response.created event: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_snapshot","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`)); err != nil {
			t.Errorf("failed to write snapshot response.done event: %v", err)
			return
		}
		<-releaseDone
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected realtime session to open, got %v", errWithCode)
	}
	defer func() {
		close(releaseDone)
		session.Abort("test_cleanup")
	}()

	recorder := &recordingOpenAIRealtimeObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })

	if err := session.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.create","response":{"input":[]}}`)); err != nil {
		t.Fatalf("expected response.create send to succeed, got %v", err)
	}

	_, payload, usage, err := session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected snapshot response.created recv without error, got %v", err)
	}
	if !strings.Contains(string(payload), `"response.created"`) {
		t.Fatalf("expected first recv to be response.created, got %q", payload)
	}
	if usage == nil || usage.TotalTokens != 8 {
		t.Fatalf("expected first usage delta to equal initial snapshot, got %+v", usage)
	}

	_, payload, usage, err = session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected snapshot response.done recv without error, got %v", err)
	}
	if !strings.Contains(string(payload), `"response.done"`) {
		t.Fatalf("expected second recv to be response.done, got %q", payload)
	}
	if usage != nil {
		t.Fatalf("expected duplicated terminal usage snapshot not to emit a second delta, got %+v", usage)
	}

	waitForOpenAIRealtimeFinalize(t, recorder, 1, 2*time.Second)
	if recorder.observeCount() != 1 {
		t.Fatalf("expected observer to record exactly one usage delta, got %d", recorder.observeCount())
	}
	if observed := recorder.lastObservedUsage(); observed == nil || observed.TotalTokens != 8 {
		t.Fatalf("expected observed usage delta to match the initial snapshot, got %+v", observed)
	}
	if finalUsage := recorder.lastPayload().Usage; finalUsage == nil || finalUsage.TotalTokens != 8 {
		t.Fatalf("expected finalized usage to preserve the cumulative snapshot, got %+v", finalUsage)
	}
}

func TestOpenAIRealtimeSessionPropagatesObserverUsageErrors(t *testing.T) {
	releaseDone := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *websocket.Conn) {
		_, _, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("failed to read response.create request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_quota","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`)); err != nil {
			t.Errorf("failed to write quota terminal response event: %v", err)
			return
		}
		<-releaseDone
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected realtime session to open, got %v", errWithCode)
	}
	defer func() {
		close(releaseDone)
		session.Abort("test_cleanup")
	}()

	observer := &failingOpenAIRealtimeObserver{observeErr: errors.New("user quota is not enough")}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return observer })

	if err := session.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.create","response":{"input":[]}}`)); err != nil {
		t.Fatalf("expected response.create send to succeed, got %v", err)
	}

	messageType, payload, usage, err := session.Recv(context.Background())
	if err == nil {
		t.Fatal("expected observer usage error to terminate the session")
	}
	if messageType != websocket.TextMessage {
		t.Fatalf("expected websocket text payload, got %d", messageType)
	}
	if !strings.Contains(string(payload), `"response.done"`) {
		t.Fatalf("expected terminal response payload before observer error, got %q", payload)
	}
	if usage == nil || usage.TotalTokens != 8 {
		t.Fatalf("expected terminal usage to remain attached to the forwarded payload, got %+v", usage)
	}

	errorPayload := string(runtimesession.ClientPayloadFromError(err))
	if !strings.Contains(errorPayload, "user quota is not enough") || !strings.Contains(errorPayload, "system_error") {
		t.Fatalf("expected structured quota error payload, got %q", errorPayload)
	}

	waitForOpenAIRealtimeFinalize(t, &observer.recordingOpenAIRealtimeObserver, 1, 2*time.Second)
	if observer.observeCount() != 1 {
		t.Fatalf("expected exactly one observer usage callback, got %d", observer.observeCount())
	}
	finalized := observer.lastPayload()
	if finalized.TerminationReason != "quota_exhausted" {
		t.Fatalf("expected quota observer failure to finalize with quota_exhausted, got %q", finalized.TerminationReason)
	}
	if finalized.Usage == nil || finalized.Usage.TotalTokens != 8 {
		t.Fatalf("expected finalized usage to preserve the terminal snapshot, got %+v", finalized.Usage)
	}
}

func TestOpenAIRealtimeSessionPassesThroughErrorEventsWithoutClosing(t *testing.T) {
	releaseDone := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *websocket.Conn) {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"bad_input","message":"bad request"}}`)); err != nil {
			t.Errorf("failed to write upstream error event: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_after_error","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)); err != nil {
			t.Errorf("failed to write post-error terminal event: %v", err)
			return
		}
		<-releaseDone
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected realtime session to open, got %v", errWithCode)
	}
	defer func() {
		close(releaseDone)
		session.Abort("test_cleanup")
	}()

	_, payload, usage, err := session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected raw upstream error event to be forwarded without closing session, got %v", err)
	}
	if usage != nil {
		t.Fatalf("expected error event not to carry usage, got %+v", usage)
	}
	if !strings.Contains(string(payload), `"bad_input"`) {
		t.Fatalf("expected raw upstream error event payload, got %q", payload)
	}

	_, payload, usage, err = session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected session to remain open after upstream error event, got %v", err)
	}
	if !strings.Contains(string(payload), `"response.done"`) {
		t.Fatalf("expected post-error response.done event, got %q", payload)
	}
	if usage == nil || usage.TotalTokens != 2 {
		t.Fatalf("expected usage after passthrough error event, got %+v", usage)
	}
}

func TestOpenAIRealtimeSessionErrorEventFinalizesTurnAndReleasesSessionBusy(t *testing.T) {
	releaseDone := make(chan struct{})
	secondCreateSeen := make(chan struct{}, 1)
	server := newOpenAIRealtimeTestServer(t, func(conn *websocket.Conn) {
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("failed to read first response.create request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"bad_input","message":"bad request"}}`)); err != nil {
			t.Errorf("failed to write terminal error event: %v", err)
			return
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("failed to read second response.create request: %v", err)
			return
		}
		secondCreateSeen <- struct{}{}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_recovered","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)); err != nil {
			t.Errorf("failed to write recovery response.done event: %v", err)
			return
		}
		<-releaseDone
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected realtime session to open, got %v", errWithCode)
	}
	defer func() {
		close(releaseDone)
		session.Abort("test_cleanup")
	}()

	recorder := &recordingOpenAIRealtimeObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })

	createPayload := []byte(`{"type":"response.create","response":{"input":[]}}`)
	if err := session.SendClient(context.Background(), websocket.TextMessage, createPayload); err != nil {
		t.Fatalf("expected first response.create send to succeed, got %v", err)
	}

	_, payload, usage, err := session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected upstream error event to be forwarded without closing the session, got %v", err)
	}
	if usage != nil {
		t.Fatalf("expected passthrough error event not to carry usage, got %+v", usage)
	}
	if !strings.Contains(string(payload), `"bad_input"`) {
		t.Fatalf("expected passthrough error payload, got %q", payload)
	}

	if err := session.SendClient(context.Background(), websocket.TextMessage, createPayload); err != nil {
		t.Fatalf("expected error event to release session_busy for the next response.create, got %v", err)
	}

	waitForOpenAIRealtimeFinalize(t, recorder, 1, 2*time.Second)
	if got := recorder.lastPayload().TerminationReason; got != types.EventTypeError {
		t.Fatalf("expected superseded error turn to finalize with reason %q, got %q", types.EventTypeError, got)
	}

	select {
	case <-secondCreateSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream to receive the second response.create request")
	}

	_, payload, usage, err = session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected recovery response.done without error, got %v", err)
	}
	if !strings.Contains(string(payload), `"response.done"`) {
		t.Fatalf("expected recovery payload to be response.done, got %q", payload)
	}
	if usage == nil || usage.TotalTokens != 3 {
		t.Fatalf("expected recovery response.done usage to be forwarded, got %+v", usage)
	}
}

func TestOpenAIRealtimeSessionIgnoresLateFinalizedResponseUsageAfterNewTurnStarts(t *testing.T) {
	releaseDone := make(chan struct{})
	allowCurrentDone := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *websocket.Conn) {
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("failed to read first response.create request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_old","status":"in_progress"}}`)); err != nil {
			t.Errorf("failed to write first response.created event: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"bad_input","message":"bad request"}}`)); err != nil {
			t.Errorf("failed to write terminal error event: %v", err)
			return
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("failed to read second response.create request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_new","status":"in_progress"}}`)); err != nil {
			t.Errorf("failed to write second response.created event: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_old","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`)); err != nil {
			t.Errorf("failed to write late finalized response.done event: %v", err)
			return
		}
		<-allowCurrentDone
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_new","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)); err != nil {
			t.Errorf("failed to write recovery response.done event: %v", err)
			return
		}
		<-releaseDone
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected realtime session to open, got %v", errWithCode)
	}
	defer func() {
		close(releaseDone)
		session.Abort("test_cleanup")
	}()

	recorder := &recordingOpenAIRealtimeObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })

	createPayload := []byte(`{"type":"response.create","response":{"input":[]}}`)
	if err := session.SendClient(context.Background(), websocket.TextMessage, createPayload); err != nil {
		t.Fatalf("expected first response.create send to succeed, got %v", err)
	}

	_, payload, usage, err := session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected first response.created without error, got %v", err)
	}
	if !strings.Contains(string(payload), `"resp_old"`) {
		t.Fatalf("expected first response.created payload, got %q", payload)
	}
	if usage != nil {
		t.Fatalf("expected first response.created not to carry usage, got %+v", usage)
	}

	_, payload, usage, err = session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected passthrough error event without closing the session, got %v", err)
	}
	if !strings.Contains(string(payload), `"bad_input"`) {
		t.Fatalf("expected passthrough error payload, got %q", payload)
	}
	if usage != nil {
		t.Fatalf("expected passthrough error event not to carry usage, got %+v", usage)
	}

	if err := session.SendClient(context.Background(), websocket.TextMessage, createPayload); err != nil {
		t.Fatalf("expected second response.create send to succeed, got %v", err)
	}

	waitForOpenAIRealtimeFinalize(t, recorder, 1, 2*time.Second)
	firstFinalize := recorder.lastPayload()
	if firstFinalize.LastResponseID != "resp_old" {
		t.Fatalf("expected first finalized turn to preserve old response id, got %q", firstFinalize.LastResponseID)
	}
	if firstFinalize.TerminationReason != types.EventTypeError {
		t.Fatalf("expected first finalized turn to use error termination, got %q", firstFinalize.TerminationReason)
	}

	_, payload, usage, err = session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected second response.created without error, got %v", err)
	}
	if !strings.Contains(string(payload), `"resp_new"`) {
		t.Fatalf("expected second response.created payload, got %q", payload)
	}
	if usage != nil {
		t.Fatalf("expected second response.created not to carry usage, got %+v", usage)
	}

	_, payload, usage, err = session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected late finalized response.done without session error, got %v", err)
	}
	if !strings.Contains(string(payload), `"resp_old"`) || !strings.Contains(string(payload), `"response.done"`) {
		t.Fatalf("expected late finalized response.done payload, got %q", payload)
	}
	if usage != nil {
		t.Fatalf("expected late finalized response.done not to attribute usage to the new turn, got %+v", usage)
	}
	if recorder.observeCount() != 0 {
		t.Fatalf("expected late finalized response.done not to bill usage, got %d observations", recorder.observeCount())
	}
	if recorder.finalizeCount() != 1 {
		t.Fatalf("expected late finalized response.done not to create a new finalization, got %d", recorder.finalizeCount())
	}

	close(allowCurrentDone)

	_, payload, usage, err = session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected current response.done without error, got %v", err)
	}
	if !strings.Contains(string(payload), `"resp_new"`) || !strings.Contains(string(payload), `"response.done"`) {
		t.Fatalf("expected current response.done payload, got %q", payload)
	}
	if usage == nil || usage.TotalTokens != 3 {
		t.Fatalf("expected current response.done usage to be preserved, got %+v", usage)
	}

	waitForOpenAIRealtimeFinalize(t, recorder, 2, 2*time.Second)
	if recorder.observeCount() != 1 {
		t.Fatalf("expected only the current turn usage to be observed, got %d", recorder.observeCount())
	}
	if got := recorder.lastPayload().LastResponseID; got != "resp_new" {
		t.Fatalf("expected second finalized turn to preserve new response id, got %q", got)
	}
}

func TestOpenAIRealtimeSessionKeepsActiveTurnWhenResponseIDChangesMidTurn(t *testing.T) {
	releaseDone := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *websocket.Conn) {
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("failed to read response.create request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_initial","status":"in_progress"}}`)); err != nil {
			t.Errorf("failed to write initial response.created event: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_reissued","status":"in_progress"}}`)); err != nil {
			t.Errorf("failed to write reissued response.created event: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_reissued","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`)); err != nil {
			t.Errorf("failed to write reissued response.done event: %v", err)
			return
		}
		<-releaseDone
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected realtime session to open, got %v", errWithCode)
	}
	defer func() {
		close(releaseDone)
		session.Abort("test_cleanup")
	}()

	recorder := &recordingOpenAIRealtimeObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })

	if err := session.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.create","response":{"input":[]}}`)); err != nil {
		t.Fatalf("expected response.create send to succeed, got %v", err)
	}

	for _, want := range []string{`"resp_initial"`, `"resp_reissued"`} {
		_, payload, usage, err := session.Recv(context.Background())
		if err != nil {
			t.Fatalf("expected response.created payload %s without error, got %v", want, err)
		}
		if !strings.Contains(string(payload), want) || !strings.Contains(string(payload), `"response.created"`) {
			t.Fatalf("expected response.created payload %s, got %q", want, payload)
		}
		if usage != nil {
			t.Fatalf("expected response.created payload %s not to carry usage, got %+v", want, usage)
		}
	}

	_, payload, usage, err := session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected reissued response.done without error, got %v", err)
	}
	if !strings.Contains(string(payload), `"resp_reissued"`) || !strings.Contains(string(payload), `"response.done"`) {
		t.Fatalf("expected reissued response.done payload, got %q", payload)
	}
	if usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("expected reissued response.done usage to be preserved, got %+v", usage)
	}

	waitForOpenAIRealtimeFinalize(t, recorder, 1, 2*time.Second)
	if recorder.observeCount() != 1 {
		t.Fatalf("expected reissued response id turn to bill once, got %d observations", recorder.observeCount())
	}
	if got := recorder.lastPayload().LastResponseID; got != "resp_reissued" {
		t.Fatalf("expected finalized turn to preserve reissued response id, got %q", got)
	}
}

func TestOpenAIRealtimeSessionIgnoresLateResponseIDSeenEarlierInFinalizedTurn(t *testing.T) {
	releaseDone := make(chan struct{})
	allowCurrentDone := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *websocket.Conn) {
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("failed to read first response.create request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_initial","status":"in_progress"}}`)); err != nil {
			t.Errorf("failed to write initial response.created event: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_reissued","status":"in_progress"}}`)); err != nil {
			t.Errorf("failed to write reissued response.created event: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_reissued","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`)); err != nil {
			t.Errorf("failed to write reissued response.done event: %v", err)
			return
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("failed to read second response.create request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_current","status":"in_progress"}}`)); err != nil {
			t.Errorf("failed to write current response.created event: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_initial","status":"completed","usage":{"input_tokens":7,"output_tokens":1,"total_tokens":8}}}`)); err != nil {
			t.Errorf("failed to write late initial response.done event: %v", err)
			return
		}
		<-allowCurrentDone
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_current","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)); err != nil {
			t.Errorf("failed to write current response.done event: %v", err)
			return
		}
		<-releaseDone
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected realtime session to open, got %v", errWithCode)
	}
	defer func() {
		close(releaseDone)
		session.Abort("test_cleanup")
	}()

	recorder := &recordingOpenAIRealtimeObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })

	createPayload := []byte(`{"type":"response.create","response":{"input":[]}}`)
	if err := session.SendClient(context.Background(), websocket.TextMessage, createPayload); err != nil {
		t.Fatalf("expected first response.create send to succeed, got %v", err)
	}

	for _, want := range []string{`"resp_initial"`, `"resp_reissued"`} {
		_, payload, usage, err := session.Recv(context.Background())
		if err != nil {
			t.Fatalf("expected response.created payload %s without error, got %v", want, err)
		}
		if !strings.Contains(string(payload), want) || !strings.Contains(string(payload), `"response.created"`) {
			t.Fatalf("expected response.created payload %s, got %q", want, payload)
		}
		if usage != nil {
			t.Fatalf("expected response.created payload %s not to carry usage, got %+v", want, usage)
		}
	}

	_, payload, usage, err := session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected reissued response.done without error, got %v", err)
	}
	if !strings.Contains(string(payload), `"resp_reissued"`) || !strings.Contains(string(payload), `"response.done"`) {
		t.Fatalf("expected reissued response.done payload, got %q", payload)
	}
	if usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("expected reissued response.done usage to be preserved, got %+v", usage)
	}

	waitForOpenAIRealtimeFinalize(t, recorder, 1, 2*time.Second)
	if recorder.observeCount() != 1 {
		t.Fatalf("expected first turn usage to be observed once, got %d", recorder.observeCount())
	}
	if got := recorder.lastPayload().LastResponseID; got != "resp_reissued" {
		t.Fatalf("expected first finalized turn to preserve reissued response id, got %q", got)
	}

	if err := session.SendClient(context.Background(), websocket.TextMessage, createPayload); err != nil {
		t.Fatalf("expected second response.create send to succeed, got %v", err)
	}

	_, payload, usage, err = session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected current response.created without error, got %v", err)
	}
	if !strings.Contains(string(payload), `"resp_current"`) || !strings.Contains(string(payload), `"response.created"`) {
		t.Fatalf("expected current response.created payload, got %q", payload)
	}
	if usage != nil {
		t.Fatalf("expected current response.created not to carry usage, got %+v", usage)
	}

	_, payload, usage, err = session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected late initial response.done without error, got %v", err)
	}
	if !strings.Contains(string(payload), `"resp_initial"`) || !strings.Contains(string(payload), `"response.done"`) {
		t.Fatalf("expected late initial response.done payload, got %q", payload)
	}
	if usage != nil {
		t.Fatalf("expected late initial response.done not to attribute usage to the new turn, got %+v", usage)
	}
	if recorder.observeCount() != 1 {
		t.Fatalf("expected late initial response.done not to bill usage, got %d observations", recorder.observeCount())
	}
	if recorder.finalizeCount() != 1 {
		t.Fatalf("expected late initial response.done not to finalize the new turn, got %d", recorder.finalizeCount())
	}

	close(allowCurrentDone)

	_, payload, usage, err = session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected current response.done without error, got %v", err)
	}
	if !strings.Contains(string(payload), `"resp_current"`) || !strings.Contains(string(payload), `"response.done"`) {
		t.Fatalf("expected current response.done payload, got %q", payload)
	}
	if usage == nil || usage.TotalTokens != 3 {
		t.Fatalf("expected current response.done usage to be preserved, got %+v", usage)
	}

	waitForOpenAIRealtimeFinalize(t, recorder, 2, 2*time.Second)
	if recorder.observeCount() != 2 {
		t.Fatalf("expected exactly two observed usage updates overall, got %d", recorder.observeCount())
	}
	if got := recorder.lastPayload().LastResponseID; got != "resp_current" {
		t.Fatalf("expected second finalized turn to preserve current response id, got %q", got)
	}
}

func TestOpenAIRealtimeSessionCompatModeClosesOnUpstreamErrorEvent(t *testing.T) {
	originalCompatMode := config.OpenAIRealtimeSessionCompatMode
	config.OpenAIRealtimeSessionCompatMode = true
	defer func() {
		config.OpenAIRealtimeSessionCompatMode = originalCompatMode
	}()

	server := newOpenAIRealtimeTestServer(t, func(conn *websocket.Conn) {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"bad_input","message":"bad request"}}`)); err != nil {
			t.Errorf("failed to write upstream error event: %v", err)
		}
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected realtime session to open in compat mode, got %v", errWithCode)
	}
	defer session.Abort("test_cleanup")

	_, payload, usage, err := session.Recv(context.Background())
	if err == nil {
		t.Fatal("expected compat mode to surface upstream error as terminal error")
	}
	if usage != nil {
		t.Fatalf("expected compat mode error event not to carry usage, got %+v", usage)
	}
	if !strings.Contains(string(payload), `"bad_input"`) {
		t.Fatalf("expected compat mode to forward raw error payload before closing, got %q", payload)
	}
}

func TestOpenAIRealtimeSessionRejectsConcurrentResponseCreate(t *testing.T) {
	allowDone := make(chan struct{})
	checkedOverlap := make(chan struct{})
	releaseDone := make(chan struct{})
	upstreamCreates := make(chan []byte, 2)
	server := newOpenAIRealtimeTestServer(t, func(conn *websocket.Conn) {
		defer func() {
			select {
			case <-checkedOverlap:
			default:
				close(checkedOverlap)
			}
		}()

		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("failed to read first response.create request: %v", err)
			return
		}
		upstreamCreates <- payload

		<-allowDone

		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_serialized","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`)); err != nil {
			t.Errorf("failed to write serialized response.done event: %v", err)
			return
		}

		if err := conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
			t.Errorf("failed to set websocket read deadline: %v", err)
			return
		}
		_, payload, err = conn.ReadMessage()
		if err == nil {
			upstreamCreates <- payload
		} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
			t.Errorf("unexpected error while checking for overlapping response.create: %v", err)
		}
		close(checkedOverlap)

		<-releaseDone
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected realtime session to open, got %v", errWithCode)
	}
	defer func() {
		close(releaseDone)
		session.Abort("test_cleanup")
	}()

	recorder := &recordingOpenAIRealtimeObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })

	createPayload := []byte(`{"type":"response.create","response":{"input":[]}}`)
	if err := session.SendClient(context.Background(), websocket.TextMessage, createPayload); err != nil {
		t.Fatalf("expected first response.create send to succeed, got %v", err)
	}

	err := session.SendClient(context.Background(), websocket.TextMessage, createPayload)
	if err == nil {
		t.Fatal("expected second inflight response.create to fail")
	}
	if got := err.Error(); !strings.Contains(got, "session_busy") {
		t.Fatalf("expected second inflight response.create to fail with session_busy, got %q", got)
	}

	close(allowDone)

	_, payload, usage, err := session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected first turn to still finalize successfully, got %v", err)
	}
	if !strings.Contains(string(payload), `"response.done"`) {
		t.Fatalf("expected serialized response.done payload after rejecting overlap, got %q", payload)
	}
	if usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("expected first turn usage to be preserved after overlap rejection, got %+v", usage)
	}

	waitForOpenAIRealtimeFinalize(t, recorder, 1, 2*time.Second)
	if recorder.finalizeCount() != 1 {
		t.Fatalf("expected exactly one finalized turn after overlap rejection, got %d", recorder.finalizeCount())
	}
	if got := recorder.lastPayload().LastResponseID; got != "resp_serialized" {
		t.Fatalf("expected first turn response id to be preserved, got %q", got)
	}

	select {
	case <-checkedOverlap:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream overlap check")
	}

	if got := len(upstreamCreates); got != 1 {
		t.Fatalf("expected only one upstream response.create to be forwarded, got %d", got)
	}
}

func TestOpenAIRealtimeSessionDetachContinuesTurnFinalization(t *testing.T) {
	releaseDone := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *websocket.Conn) {
		_, _, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("failed to read detached response.create request: %v", err)
			return
		}
		time.Sleep(50 * time.Millisecond)
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_detached","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`)); err != nil {
			t.Errorf("failed to write detached terminal response event: %v", err)
			return
		}
		<-releaseDone
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected realtime session to open, got %v", errWithCode)
	}
	defer func() {
		close(releaseDone)
		session.Abort("test_cleanup")
	}()

	concrete, ok := session.(*openAIRealtimeSession)
	if !ok {
		t.Fatalf("expected concrete openAI realtime session, got %T", session)
	}

	recorder := &recordingOpenAIRealtimeObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })

	if err := session.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.create","response":{"input":[]}}`)); err != nil {
		t.Fatalf("expected detached response.create send to succeed, got %v", err)
	}

	session.Detach("test_detach")

	waitForOpenAIRealtimeFinalize(t, recorder, 1, 2*time.Second)
	if recorder.finalizeCount() != 1 {
		t.Fatalf("expected detached session to finalize exactly one turn, got %d", recorder.finalizeCount())
	}
	if got := recorder.lastPayload().LastResponseID; got != "resp_detached" {
		t.Fatalf("expected detached turn finalization to preserve response id, got %q", got)
	}
	if usage := recorder.lastPayload().Usage; usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("expected detached turn finalization to preserve usage, got %+v", usage)
	}
	if queued := len(concrete.recvCh); queued != 0 {
		t.Fatalf("expected detached session to avoid queueing downstream events, got %d buffered frames", queued)
	}
}

func TestOpenAIRealtimeSessionAbortFinalizesInflightTurn(t *testing.T) {
	serverReady := make(chan struct{})
	releaseDone := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *websocket.Conn) {
		_, _, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("failed to read abort response.create request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_abort","status":"in_progress","usage":{"input_tokens":2,"output_tokens":0,"total_tokens":2}}}`)); err != nil {
			t.Errorf("failed to write abort response.created event: %v", err)
			return
		}
		close(serverReady)
		<-releaseDone
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected realtime session to open, got %v", errWithCode)
	}
	defer func() {
		close(releaseDone)
		session.Abort("test_cleanup")
	}()

	recorder := &recordingOpenAIRealtimeObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })

	if err := session.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.create","response":{"input":[]}}`)); err != nil {
		t.Fatalf("expected abort response.create send to succeed, got %v", err)
	}

	select {
	case <-serverReady:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for in-flight abort response")
	}

	_, payload, usage, err := session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected in-flight payload before abort, got %v", err)
	}
	if !strings.Contains(string(payload), `"response.created"`) {
		t.Fatalf("expected response.created payload before abort, got %q", payload)
	}
	if usage == nil || usage.TotalTokens != 2 {
		t.Fatalf("expected in-flight usage before abort, got %+v", usage)
	}

	session.Abort("test_abort")
	waitForOpenAIRealtimeFinalize(t, recorder, 1, 2*time.Second)

	if recorder.finalizeCount() != 1 {
		t.Fatalf("expected abort to finalize exactly once, got %d", recorder.finalizeCount())
	}
	if got := recorder.lastPayload().LastResponseID; got != "resp_abort" {
		t.Fatalf("expected abort finalization to preserve response id, got %q", got)
	}
	if finalUsage := recorder.lastPayload().Usage; finalUsage == nil || finalUsage.TotalTokens != 2 {
		t.Fatalf("expected abort finalization to preserve usage, got %+v", finalUsage)
	}
}

func TestOpenAIRealtimeSessionUpstreamCloseFinalizesInflightTurn(t *testing.T) {
	server := newOpenAIRealtimeTestServer(t, func(conn *websocket.Conn) {
		_, _, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("failed to read upstream-close response.create request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_upstream_closed","status":"in_progress","usage":{"input_tokens":4,"output_tokens":0,"total_tokens":4}}}`)); err != nil {
			t.Errorf("failed to write upstream-close response.created event: %v", err)
			return
		}
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected realtime session to open, got %v", errWithCode)
	}
	defer session.Abort("test_cleanup")

	recorder := &recordingOpenAIRealtimeObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })

	if err := session.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.create","response":{"input":[]}}`)); err != nil {
		t.Fatalf("expected upstream-close response.create send to succeed, got %v", err)
	}

	_, payload, usage, err := session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected in-flight payload before upstream close, got %v", err)
	}
	if !strings.Contains(string(payload), `"response.created"`) {
		t.Fatalf("expected response.created payload before upstream close, got %q", payload)
	}
	if usage == nil || usage.TotalTokens != 4 {
		t.Fatalf("expected in-flight usage before upstream close, got %+v", usage)
	}

	waitForOpenAIRealtimeFinalize(t, recorder, 1, 2*time.Second)

	if recorder.finalizeCount() != 1 {
		t.Fatalf("expected upstream close to finalize exactly once, got %d", recorder.finalizeCount())
	}
	if got := recorder.lastPayload().LastResponseID; got != "resp_upstream_closed" {
		t.Fatalf("expected upstream close finalization to preserve response id, got %q", got)
	}
	if finalUsage := recorder.lastPayload().Usage; finalUsage == nil || finalUsage.TotalTokens != 4 {
		t.Fatalf("expected upstream close finalization to preserve usage, got %+v", finalUsage)
	}
}

func TestOpenAIRealtimeSessionCloseFinalizesInflightTurn(t *testing.T) {
	serverReady := make(chan struct{})
	releaseDone := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *websocket.Conn) {
		_, _, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("failed to read close response.create request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_local_close","status":"in_progress","usage":{"input_tokens":6,"output_tokens":0,"total_tokens":6}}}`)); err != nil {
			t.Errorf("failed to write close response.created event: %v", err)
			return
		}
		close(serverReady)
		<-releaseDone
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected realtime session to open, got %v", errWithCode)
	}
	defer func() {
		close(releaseDone)
		session.Abort("test_cleanup")
	}()

	concrete, ok := session.(*openAIRealtimeSession)
	if !ok {
		t.Fatalf("expected concrete openAI realtime session, got %T", session)
	}

	recorder := &recordingOpenAIRealtimeObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })

	if err := session.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.create","response":{"input":[]}}`)); err != nil {
		t.Fatalf("expected close response.create send to succeed, got %v", err)
	}

	select {
	case <-serverReady:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for local close response")
	}

	_, payload, usage, err := session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected in-flight payload before local close, got %v", err)
	}
	if !strings.Contains(string(payload), `"response.created"`) {
		t.Fatalf("expected response.created payload before local close, got %q", payload)
	}
	if usage == nil || usage.TotalTokens != 6 {
		t.Fatalf("expected in-flight usage before local close, got %+v", usage)
	}

	concrete.close("idle_timeout")
	waitForOpenAIRealtimeFinalize(t, recorder, 1, 2*time.Second)

	if recorder.finalizeCount() != 1 {
		t.Fatalf("expected local close to finalize exactly once, got %d", recorder.finalizeCount())
	}
	if got := recorder.lastPayload().LastResponseID; got != "resp_local_close" {
		t.Fatalf("expected local close finalization to preserve response id, got %q", got)
	}
	if finalUsage := recorder.lastPayload().Usage; finalUsage == nil || finalUsage.TotalTokens != 6 {
		t.Fatalf("expected local close finalization to preserve usage, got %+v", finalUsage)
	}
}

func TestOpenAIRealtimeSessionAbortWaitsForObserverUsageBeforeFinalizing(t *testing.T) {
	server := newOpenAIRealtimeTestServer(t, func(conn *websocket.Conn) {
		_, _, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("failed to read observer-guard response.create request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_guarded","status":"in_progress","usage":{"input_tokens":2,"output_tokens":0,"total_tokens":2}}}`)); err != nil {
			t.Errorf("failed to write observer-guard response.created event: %v", err)
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected realtime session to open, got %v", errWithCode)
	}

	observer := newBlockingOpenAIRealtimeObserver()
	defer session.Abort("test_cleanup")
	defer observer.releaseObserve()
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return observer })

	if err := session.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.create","response":{"input":[]}}`)); err != nil {
		t.Fatalf("expected observer-guard response.create send to succeed, got %v", err)
	}

	select {
	case <-observer.observeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ObserveTurnUsage to start")
	}

	abortDone := make(chan struct{})
	go func() {
		session.Abort("test_abort")
		close(abortDone)
	}()

	select {
	case <-observer.finalizeCalled:
		t.Fatal("expected finalize to wait for the in-flight ObserveTurnUsage call")
	case <-abortDone:
		t.Fatal("expected Abort to remain blocked until ObserveTurnUsage returns")
	case <-time.After(150 * time.Millisecond):
	}

	observer.releaseObserve()

	select {
	case <-abortDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Abort to finish after ObserveTurnUsage returned")
	}

	select {
	case <-observer.finalizeCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for FinalizeTurn after ObserveTurnUsage returned")
	}

	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.observeCount != 1 {
		t.Fatalf("expected exactly one ObserveTurnUsage call, got %d", observer.observeCount)
	}
	if observer.finalizeCount != 1 {
		t.Fatalf("expected exactly one FinalizeTurn call, got %d", observer.finalizeCount)
	}
	if observer.finalizedBeforeDone {
		t.Fatal("expected FinalizeTurn not to reach the underlying observer before ObserveTurnUsage completed")
	}
}

func newOpenAIRealtimeTestProvider(serverURL string) *OpenAIProvider {
	proxy := ""
	channel := &model.Channel{
		Key:   "sk-test",
		Proxy: &proxy,
	}
	return CreateOpenAIProvider(channel, serverURL)
}

func newOpenAIRealtimeTestServer(t *testing.T, handler func(conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		handler(conn)
	}))
	return server
}
