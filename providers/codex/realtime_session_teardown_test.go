package codex

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/requester"
	runtimesession "one-api/runtime/session"
)

// blockingCodexBridgeStream models an upstream SSE stream that hangs: its data and
// error channels never produce or close on their own. Only Close() closes dataCh,
// which is exactly what lets pumpRealtimeHTTPBridge exit. Before the teardown fix,
// Abort/Detach left state.bridgeStream untouched, so the pump kept spinning on this
// stream until the upstream eventually closed — leaking a goroutine + HTTP conn.
type blockingCodexBridgeStream struct {
	dataCh    chan string
	errCh     chan error
	closeOnce sync.Once
	closed    atomic.Bool
}

func newBlockingCodexBridgeStream() *blockingCodexBridgeStream {
	return &blockingCodexBridgeStream{
		dataCh: make(chan string),
		errCh:  make(chan error, 1),
	}
}

func (s *blockingCodexBridgeStream) Recv() (<-chan string, <-chan error) {
	return s.dataCh, s.errCh
}

func (s *blockingCodexBridgeStream) Close() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.dataCh)
	})
}

// newCodexHTTPBridgeInflightSession builds a managed session whose state owns an
// inflight HTTP bridge turn (bridgeStream + turnObserver + turnSeq), so Abort/Detach
// hit the bridge teardown path.
func newCodexHTTPBridgeInflightSession(t *testing.T, stream requester.StreamReaderInterface[string], observer runtimesession.TurnObserver) (*codexManagedRealtimeSession, *runtimesession.ExecutionSession) {
	t.Helper()
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "codex-bridge-teardown",
		SessionID: "codex-bridge-teardown",
		Model:     "gpt-5",
		Protocol:  codexRealtimeProtocolName,
	})
	attachment := newCodexAttachment()
	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	state.bridgeStream = stream
	state.attachment = attachment
	state.ownerSeq = 1
	state.turnObserver = observer
	state.turnSeq = 1
	exec.Attached = true
	exec.Inflight = true
	exec.Transport = runtimesession.TransportModeResponsesHTTPBridge
	exec.State = runtimesession.SessionStateActive
	exec.Unlock()

	session := &codexManagedRealtimeSession{
		provider:   &CodexProvider{},
		exec:       exec,
		attachment: attachment,
		ownerSeq:   1,
	}
	return session, exec
}

func TestCodexRealtimeAbortClosesHTTPBridgeStreamAndFinalizes(t *testing.T) {
	stream := newBlockingCodexBridgeStream()
	observer := &recordingTurnObserver{}
	session, exec := newCodexHTTPBridgeInflightSession(t, stream, observer)

	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		session.provider.pumpRealtimeHTTPBridge(exec, stream)
	}()

	session.Abort("client_closed")

	select {
	case <-pumpDone:
	case <-time.After(2 * time.Second):
		t.Fatal("pump goroutine leaked after Abort: bridge stream was not closed")
	}
	if !stream.closed.Load() {
		t.Fatal("expected Abort to close the HTTP bridge stream")
	}
	if got := observer.finalizeCount(); got != 1 {
		t.Fatalf("expected bridge turn to finalize exactly once on abort, got %d", got)
	}
}

func TestCodexRealtimeDetachClosesHTTPBridgeStreamAndFinalizes(t *testing.T) {
	stream := newBlockingCodexBridgeStream()
	observer := &recordingTurnObserver{}
	session, exec := newCodexHTTPBridgeInflightSession(t, stream, observer)

	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		session.provider.pumpRealtimeHTTPBridge(exec, stream)
	}()

	session.Detach("proxy_closed")

	select {
	case <-pumpDone:
	case <-time.After(2 * time.Second):
		t.Fatal("pump goroutine leaked after Detach: bridge stream was not closed")
	}
	if !stream.closed.Load() {
		t.Fatal("expected Detach to close the HTTP bridge stream")
	}
	if got := observer.finalizeCount(); got != 1 {
		t.Fatalf("expected bridge turn to finalize exactly once on detach, got %d", got)
	}

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	if state.bridgeStream != nil || exec.Inflight || exec.Attached || exec.State != runtimesession.SessionStateClosed {
		t.Fatalf("expected detach to clear bridge inflight state, bridge=%v inflight=%v attached=%v state=%s", state.bridgeStream, exec.Inflight, exec.Attached, exec.State)
	}
	exec.Unlock()
}
