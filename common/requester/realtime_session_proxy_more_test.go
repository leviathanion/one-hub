package requester

import (
	"context"
	"errors"
	"io"
	"net"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRealtimeSessionProxyHelperMethodsAndErrorClassification(t *testing.T) {
	proxy := NewRealtimeSessionProxy(nil, newFakeRealtimeSession(), 0)
	if proxy.timeout != 2*time.Minute {
		t.Fatalf("expected zero timeout to normalize to default, got %v", proxy.timeout)
	}
	if proxy.UserClosed() == nil || proxy.SupplierClosed() == nil {
		t.Fatal("expected proxy close notification channels to be initialized")
	}

	proxy.Close()
	proxy.Close()
	if got := proxy.session.(*fakeRealtimeSession).lastCloseReason(); got != "proxy_closed" {
		t.Fatalf("expected Close to detach session with proxy_closed, got %q", got)
	}

	proxy.markActivity(time.Time{})
	if proxy.lastActivityUnixNano.Load() == 0 {
		t.Fatal("expected markActivity to backfill zero timestamps")
	}

	if got := proxy.detachReason("supplier"); got != "supplier_closed" {
		t.Fatalf("expected supplier detach reason, got %q", got)
	}
	if got := proxy.detachReason(" idle "); got != "idle_timeout" {
		t.Fatalf("expected idle detach reason, got %q", got)
	}
	if got := proxy.detachReason("other"); got != "user_closed" {
		t.Fatalf("expected default detach reason, got %q", got)
	}

	structuredErr := types.NewErrorEvent("evt_busy", "invalid_request_error", "session_busy", "busy")
	if payload := proxyErrorPayload(nil); payload != nil {
		t.Fatalf("expected nil proxy error payload, got %q", payload)
	}
	if payload := proxyErrorPayload(context.Canceled); payload != nil {
		t.Fatalf("expected canceled proxy error payload to be ignored, got %q", payload)
	}
	if payload := proxyErrorPayload(runtimesession.NewClientPayloadError(structuredErr, []byte(`{"type":"error"}`))); string(payload) != `{"type":"error"}` {
		t.Fatalf("expected embedded client payload to win, got %q", payload)
	}
	if payload := string(proxyErrorPayload(structuredErr)); !strings.Contains(payload, `"code":"session_busy"`) {
		t.Fatalf("expected structured proxy error payload, got %q", payload)
	}
	if payload := string(proxyErrorPayload(errors.New("boom"))); !strings.Contains(payload, `"message":"boom"`) {
		t.Fatalf("expected generic proxy error payload, got %q", payload)
	}

	if !isRecoverableRealtimeProxyError(structuredErr) {
		t.Fatal("expected structured realtime event errors to be recoverable")
	}
	if isRecoverableRealtimeProxyError(context.Canceled) {
		t.Fatal("expected canceled realtime proxy errors not to be recoverable")
	}

	if !isRealtimeDisconnectError(io.EOF) || !isRealtimeDisconnectError(net.ErrClosed) || !isRealtimeDisconnectError(context.Canceled) {
		t.Fatal("expected EOF/net closed/context canceled to classify as disconnects")
	}
	if !isRealtimeDisconnectError(&websocket.CloseError{Code: websocket.CloseNormalClosure}) {
		t.Fatal("expected normal websocket closure to classify as disconnect")
	}
	if !isRealtimeDisconnectError(errors.New("broken pipe")) || !isRealtimeDisconnectError(errors.New("connection reset by peer")) {
		t.Fatal("expected socket reset strings to classify as disconnects")
	}
	if isRealtimeDisconnectError(errors.New("something else")) {
		t.Fatal("expected unrelated errors not to classify as disconnects")
	}

	if got := minRealtimeProxyDuration(0, 2*time.Second); got != 2*time.Second {
		t.Fatalf("expected zero left duration to fall back to right, got %v", got)
	}
	if got := minRealtimeProxyDuration(2*time.Second, 0); got != 2*time.Second {
		t.Fatalf("expected zero right duration to fall back to left, got %v", got)
	}
	if got := minRealtimeProxyDuration(2*time.Second, time.Second); got != time.Second {
		t.Fatalf("expected min duration selection, got %v", got)
	}

	proxy.safeSessionAction("noop", nil)
	proxy.safeSessionAction("panic", func() { panic("boom") })

	emergency := NewRealtimeSessionProxy(nil, newFakeRealtimeSession(), time.Second)
	emergency.emergencyShutdown(" emergency_abort ")
	if got := emergency.session.(*fakeRealtimeSession).lastCloseReason(); got != "emergency_abort" {
		t.Fatalf("expected emergency shutdown to abort session with trimmed reason, got %q", got)
	}

	doneProxy := NewRealtimeSessionProxy(nil, nil, time.Second)
	doneProxy.signalDone()
	doneProxy.signalDone()
	select {
	case <-doneProxy.done:
	default:
		t.Fatal("expected signalDone to close the done channel")
	}

	idleProxy := NewRealtimeSessionProxy(nil, nil, time.Second)
	idleProxy.timeout = 0
	idleDone := make(chan struct{})
	go func() {
		idleProxy.idleWatchdog()
		close(idleDone)
	}()
	select {
	case <-idleDone:
	case <-time.After(time.Second):
		t.Fatal("expected idleWatchdog with non-positive timeout to return immediately")
	}
}
