package codex

import (
	"context"
	"errors"
	"one-api/common/wsconn"
	runtimesession "one-api/runtime/session"
	"sync"
	"testing"
	"time"
)

type shutdownBlockedFinalizer struct {
	recordingTurnObserver
	entered, release, done chan struct{}
}

func (o *shutdownBlockedFinalizer) FinalizeTurn(runtimesession.TurnFinalizePayload) {
	close(o.entered)
	<-o.release
	close(o.done)
}

func TestRuntimeShutdownJoinsDetachedRealtimeSettlement(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, nil)
	manager := runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{DefaultTTL: time.Minute})
	replaceCodexExecutionSessionsForTest(t, manager)
	exec, _, err := manager.GetOrCreate(runtimesession.Metadata{Key: "channel:1/hash/detached-review", SessionID: "detached-review", Model: "gpt-5", ClientSuppliedID: true, IdleTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	conn, cleanup := newCodexRealtimeConnPair(t)
	defer cleanup()
	observer := &shutdownBlockedFinalizer{entered: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(observer.release) }) }
	defer func() {
		unblock()
		select {
		case <-observer.done:
		case <-time.After(time.Second):
		}
	}()
	attachment := newCodexAttachmentWithCapacity(4)
	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	state.wsConn = conn
	state.ownerSeq = 1
	state.attachment = attachment
	state.turnSeq = 1
	state.turnObserver = observer
	exec.Attached = true
	exec.Inflight = true
	provider.startRealtimeWSReaderLocked(exec, state)
	exec.Unlock()
	session := &codexManagedRealtimeSession{provider: provider, exec: exec, attachment: attachment, ownerSeq: 1}
	session.Detach("client_disconnected")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindGracefulShutdown, Reason: "server_shutdown"})
	select {
	case <-conn.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-observer.entered:
	case <-ctx.Done():
		t.Fatal("provider close did not start finalization")
	}
	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if err := StopExecutionSessionRuntime(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("detached settlement not joined: %v", err)
	}
	unblock()
	if err := StopExecutionSessionRuntime(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observer.done:
	default:
		t.Fatal("runtime stopped before billing callback finished")
	}
}
