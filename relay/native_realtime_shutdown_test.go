package relay

import (
	"context"
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"one-api/common/config"
	"one-api/model"
	"one-api/providers/openai"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"sync"
	"testing"
	"time"
)

type shutdownNativeFinalizer struct{ entered, release, done chan struct{} }

func (o *shutdownNativeFinalizer) ObserveTurnUsage(*types.UsageEvent) error { return nil }
func (o *shutdownNativeFinalizer) FinalizeTurn(runtimesession.TurnFinalizePayload) {
	close(o.entered)
	<-o.release
	close(o.done)
}

func TestNativeRealtimeBridgeJoinsBillingAfterSupplierCloses(t *testing.T) {
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, e := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if e != nil {
			return
		}
		defer c.Close()
		_, _, _ = c.ReadMessage()
	}))
	defer providerServer.Close()
	proxy := ""
	p := openai.CreateOpenAIProvider(&model.Channel{Key: "fixture", Type: config.ChannelTypeOpenAI, Other: `{"self_hosted":true}`, Proxy: &proxy}, providerServer.URL)
	session, apiErr := p.OpenRealtimeSession("gpt-4o-realtime-preview")
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	observer := &shutdownNativeFinalizer{entered: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(observer.release) }) }
	defer func() { unblock(); session.Abort("test_cleanup") }()
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return observer })
	if err := session.SendClient(context.Background(), runtimerealtime.NewTextFrame([]byte(`{"type":"response.create","response":{"input":[]}}`))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observer.entered:
	case <-time.After(time.Second):
		t.Fatal("provider finalizer did not start")
	}
	clientConn, _ := newRelayWebsocketPair(t)
	bridge := newRealtimeRelayActor(clientConn, session, time.Minute)
	bridge.Start()
	joined := make(chan struct{})
	go func() { bridge.Wait(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("bridge returned before provider billing finished")
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("bridge failed to join completed finalizer")
	}
	select {
	case <-observer.done:
	default:
		t.Fatal("provider billing not joined")
	}
}
