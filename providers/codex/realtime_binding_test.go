package codex

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/wsconn"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
	"one-api/types"
)

func TestCodexRealtimeModelMappingPreservesOtherWireFields(t *testing.T) {
	raw := []byte(`{ "type":"response.create", "model" : "public-voice", "input":"hello", "future":{"n":1e3}, "future":{"n":2e3} }`)
	models := runtimesession.ModelBinding{RequestedModel: "public-voice", ProviderModel: "gpt-5", BillingModel: "public-price"}
	_, request, encoded, err := (&CodexProvider{}).prepareCodexRealtimeCreatePayload(raw, models)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Replace(raw, []byte(`"public-voice"`), []byte(`"gpt-5"`), 1)
	if request.Model != "gpt-5" || !bytes.Equal(encoded, want) {
		t.Fatalf("model mapping changed unrelated wire: %s", encoded)
	}
	wrong := bytes.Replace(raw, []byte(`"public-voice"`), []byte(`"other-public"`), 1)
	if _, _, _, err := (&CodexProvider{}).prepareCodexRealtimeCreatePayload(wrong, models); err == nil {
		t.Fatal("unbound public model was allowed")
	}
}

type codexBindingObserver struct {
	exec       *runtimesession.ExecutionSession
	admission  runtimesession.TurnAdmission
	payload    runtimesession.TurnFinalizePayload
	started    chan struct{}
	release    chan struct{}
	result     runtimesession.TurnFinalizationResult
	lockFailed atomic.Bool
}

func (o *codexBindingObserver) AdmitBoundedTurn(admission runtimesession.TurnAdmission) error {
	if !o.exec.TryLock() {
		o.lockFailed.Store(true)
		return errors.New("admission ran with session lock held")
	}
	o.exec.Unlock()
	o.admission = admission
	return nil
}

func (o *codexBindingObserver) ObserveTurnUsage(*types.UsageEvent) error { return nil }

func (o *codexBindingObserver) FinalizeTurn(payload runtimesession.TurnFinalizePayload) {
	o.payload = payload
	if !o.exec.TryLock() {
		o.lockFailed.Store(true)
	} else {
		o.exec.Unlock()
	}
	close(o.started)
	<-o.release
}

func (o *codexBindingObserver) FinalizationResult() runtimesession.TurnFinalizationResult {
	return o.result
}

func TestCodexRealtimeFreezesModelsAndStopsFutureWorkAfterUnsettled(t *testing.T) {
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		defer conn.Close()
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			sends.Add(1)
			if !containsAll(string(payload), `"model":"gpt-5"`, `"future":12345678901234567890`) {
				t.Errorf("provider payload lost bound model or unknown field: %s", payload)
			}
			if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_binding","model":"gpt-5-reported","status":"completed","usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`)); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	provider := newTestCodexProviderWithContext(t, `{"access_token":"test-token","account_id":"acct-binding"}`, `{}`, nil)
	provider.Context.Set("token_id", 804)
	provider.Channel.BaseURL = stringPtr(server.URL)
	models := runtimesession.ModelBinding{RequestedModel: "public-voice", ProviderModel: "gpt-5", BillingModel: "public-price"}
	session, apiErr := provider.OpenRealtimeSessionWithOptions("gpt-5", runtimerealtime.RealtimeOpenOptions{Models: models})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	defer session.Abort("test_cleanup")
	managed := session.(*codexManagedRealtimeSession)
	observer := &codexBindingObserver{
		exec: managed.exec, started: make(chan struct{}), release: make(chan struct{}),
		result: runtimesession.TurnFinalizationResult{Unsettled: true, Err: errors.New("commit_unknown")},
	}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return observer })
	create := codexTestTextFrame([]byte(`{"type":"response.create","model":"public-voice","input":"hello","future":12345678901234567890}`))
	if err := session.SendClient(context.Background(), create); err != nil {
		t.Fatal(err)
	}
	if observer.admission.Models != models || observer.admission.WorkID == "" {
		t.Fatalf("admission model meanings lost: %+v", observer.admission)
	}
	managed.exec.Lock()
	getCodexManagedRuntimeStateLocked(managed.exec).models.BillingModel = "next-price"
	managed.exec.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	terminal := make(chan runtimerealtime.RecvEvent, 1)
	recvErr := make(chan error, 1)
	go func() {
		event, err := session.Recv(ctx)
		terminal <- event
		recvErr <- err
	}()
	select {
	case <-observer.started:
	case <-ctx.Done():
		t.Fatal("turn did not start finalization")
	}
	if err := session.SendClient(ctx, create); err == nil {
		t.Error("next turn began before prior settlement finished")
	}
	close(observer.release)
	if err := <-recvErr; err != nil {
		t.Fatal(err)
	}
	if event := <-terminal; event.Frame == nil || !containsAll(string(event.Frame.Payload()), "response.completed", "resp_binding") {
		t.Fatalf("settlement error hid provider terminal: %+v", event)
	}
	models.ReportedModel = "gpt-5-reported"
	if observer.payload.Models != models || observer.payload.Model != "public-price" || observer.lockFailed.Load() {
		t.Fatalf("finalization changed models or ran under session lock: %+v", observer.payload)
	}
	if err := session.SendClient(ctx, create); !errors.Is(err, runtimerealtime.ErrSessionClosed) {
		t.Fatalf("unsettled turn allowed future work: %v", err)
	}
	if sends.Load() != 1 {
		t.Fatalf("provider received %d creates", sends.Load())
	}
}
