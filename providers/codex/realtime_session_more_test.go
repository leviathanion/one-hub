package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"one-api/common/authutil"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requestctx"
	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func openCodexResponsesWSTestSession(provider *CodexProvider, ctx context.Context, model string, req responsesws.OpenRequest) (responsesws.Upstream, *types.OpenAIErrorWithStatusCode) {
	if req.FirstFrame == nil {
		frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"` + model + `","input":"hello"}`))
		if err != nil {
			panic(err)
		}
		req.FirstFrame = frame
	}
	headers := requestctx.HeaderSnapshot{}
	principal := requestctx.Principal{}
	if provider != nil && provider.Context != nil {
		if provider.Context.Request != nil {
			headers = requestctx.NewHeaderSnapshot(provider.Context.Request.Header)
		}
		principal = requestctx.PrincipalFromGin(provider.Context)
	}
	req.InboundHeaders = headers
	req.Principal = principal
	req.SelectedModel = model
	return provider.OpenResponsesWS(ctx, &req)
}

func TestCodexRealtimePumpContextPreservesRequestValuesWithoutCancel(t *testing.T) {
	base, cancel := context.WithCancel(context.WithValue(context.Background(), logger.RequestIdKey, "req-codex-pump"))
	pumpCtx := codexRealtimePumpContext(base)
	cancel()

	if got := pumpCtx.Value(logger.RequestIdKey); got != "req-codex-pump" {
		t.Fatalf("expected request id to be preserved, got %v", got)
	}
	select {
	case <-pumpCtx.Done():
		t.Fatal("expected pump context to ignore request cancellation")
	default:
	}
}

func TestLogCodexRealtimeInternalErrorPreservesDetailAndCaller(t *testing.T) {
	core, observedLogs := observer.New(zapcore.ErrorLevel)
	originalLogger := logger.Logger
	logger.Logger = zap.New(core)
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	logCodexRealtimeInternalError(`codex realtime failed Authorization="Bearer log-secret\"tail" jwt abcdefghij.klmnopqrst.uvwxyzabcd upstream-url https://provider.example/v1?token=secret`)

	logs := observedLogs.All()
	if len(logs) != 1 {
		t.Fatalf("expected one log entry, got %d", len(logs))
	}
	message := logs[0].Message
	for _, expected := range []string{"log-secret", "tail", "abcdefghij.klmnopqrst.uvwxyzabcd", "provider.example", "token=secret"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("系统日志丢失原始诊断 %q: %q", expected, message)
		}
	}
	if !strings.Contains(message, "caller=realtime_session_more_test.go:") {
		t.Fatalf("expected caller metadata, got %q", message)
	}
}

func codexRealtimeTestWriteTimeout() func() time.Duration {
	timeout := config.RealtimeWebsocketWriteTimeout()
	return func() time.Duration { return timeout }
}

func TestCodexResponsesWSAdapterMapsProviderCloseOnlyForPeerClose(t *testing.T) {
	adapter := &codexResponsesWSAdapter{}
	cases := []struct {
		name string
		info responsesws.ProviderCloseInfo
		want bool
	}{
		{
			name: "peer close",
			info: responsesws.ProviderCloseInfo{Kind: responsesws.ProviderCloseKindPeerClose, Code: int(wsconn.CloseNormalClosure), Reason: "done"},
			want: true,
		},
		{
			name: "unknown with code and reason",
			info: responsesws.ProviderCloseInfo{Kind: responsesws.ProviderCloseKindUnknown, Code: int(wsconn.CloseNormalClosure), Reason: "done"},
		},
		{
			name: "unknown close error",
			info: responsesws.ProviderCloseInfo{Kind: responsesws.ProviderCloseKindUnknown, Err: &wsconn.CloseError{Code: wsconn.CloseNormalClosure, Reason: "done"}},
		},
		{
			name: "write error",
			info: responsesws.ProviderCloseInfo{Kind: responsesws.ProviderCloseKindWriteError, Code: int(wsconn.CloseNormalClosure), Reason: "local write failed"},
		},
		{
			name: "normal local close",
			info: responsesws.ProviderCloseInfo{Kind: responsesws.ProviderCloseKindNormal, Code: int(wsconn.CloseNormalClosure), Reason: "local normal"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := adapter.MapProviderClose(context.Background(), tc.info)
			if got := result.ProviderClose != nil; got != tc.want {
				t.Fatalf("provider close mapping mismatch: got %v want %v result=%+v", got, tc.want, result)
			}
			if tc.want && result.Origin != responsesws.RecvDetailOriginNativeProviderClose {
				t.Fatalf("expected native provider close origin, got %+v", result)
			}
		})
	}
}

func newCodexRealtimeConnPair(t *testing.T) (*wsconn.ManagedConn, func()) {
	t.Helper()

	wsURL, cleanupServer := wstest.Server(t, func(conn *wsconn.ManagedConn) {
		<-conn.Done()
	})
	conn, err := wsconn.DialManaged(context.Background(), wsURL, nil, wsconn.Config{
		Label:        "codex realtime test upstream",
		ReadLimit:    config.RealtimeWebsocketReadLimit(),
		WriteTimeout: codexRealtimeTestWriteTimeout(),
	}, wsconn.WithDialSecurityPolicy(wsconn.DialSecurityPolicy{
		AllowInsecureWS: true,
		AllowPrivateIP:  true,
	}))
	if err != nil {
		cleanupServer()
		t.Fatalf("failed to dial helper websocket: %v", err)
	}

	return conn, func() {
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
		cleanupServer()
	}
}

func newCodexRealtimeConnPairFromURL(t *testing.T, wsURL string) (*wsconn.ManagedConn, func()) {
	t.Helper()
	conn, err := wsconn.DialManaged(context.Background(), wsURL, nil, wsconn.Config{
		Label:        "codex realtime test upstream",
		ReadLimit:    config.RealtimeWebsocketReadLimit(),
		WriteTimeout: codexRealtimeTestWriteTimeout(),
	}, wsconn.WithDialSecurityPolicy(wsconn.DialSecurityPolicy{
		AllowInsecureWS: true,
		AllowPrivateIP:  true,
	}))
	if err != nil {
		t.Fatalf("failed to dial helper websocket: %v", err)
	}
	return conn, func() {
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
	}
}

func TestCodexRealtimeSessionSendClientRejectsZeroFrame(t *testing.T) {
	session := &codexManagedRealtimeSession{
		provider: &CodexProvider{},
		exec:     runtimesession.NewExecutionSession(runtimesession.Metadata{SessionID: "codex-zero-frame"}),
	}

	if err := session.SendClient(context.Background(), runtimerealtime.Frame{}); !errors.Is(err, runtimerealtime.ErrInvalidFrame) {
		t.Fatalf("expected zero frame to return ErrInvalidFrame, got %v", err)
	}
}

func TestCodexRealtimeSessionSendClientRejectsUnknownFrameKind(t *testing.T) {
	session := &codexManagedRealtimeSession{
		provider: &CodexProvider{},
		exec:     runtimesession.NewExecutionSession(runtimesession.Metadata{SessionID: "codex-unknown-frame-kind"}),
	}

	if err := session.SendClient(context.Background(), codexTestUnknownKindFrame([]byte("{}"))); !errors.Is(err, runtimerealtime.ErrInvalidFrame) {
		t.Fatalf("expected unknown frame kind to return ErrInvalidFrame, got %v", err)
	}
}

func TestCodexResponsesWSOpenBypassesExecutionSessionManager(t *testing.T) {
	var connections atomic.Int32
	server := newCodexRealtimeCountingServer(t, &connections)
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "client-supplied-realtime-session",
	})
	provider.Context.Set("token_id", 9201)
	provider.Channel.BaseURL = stringPtr(server.URL)
	enableCodexResponsesWSSelfHostedForTest(t, provider)

	before := ExecutionSessionStats()
	session, errWithCode := openCodexResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{UpstreamSessionID: "responses-ws-local-test"})
	if errWithCode != nil {
		t.Fatalf("expected ResponsesWS session to open, got %v", errWithCode)
	}
	upstream, ok := session.(*responsesws.NativeSession)
	if !ok {
		t.Fatalf("expected common native ResponsesWS upstream, got %T", session)
	}
	defer upstream.Abort("test_cleanup")

	after := ExecutionSessionStats()
	if after.LocalSessions != before.LocalSessions || after.LocalBindings != before.LocalBindings {
		t.Fatalf("expected ResponsesWS open to bypass global execution session manager, before=%+v after=%+v", before, after)
	}
	waitForCodexRealtimeConnectionCount(t, &connections, 1)
}

func TestCodexResponsesWSNativeUsesIndependentConnectionsForSameClientSession(t *testing.T) {
	var connections atomic.Int32
	server := newCodexRealtimeCountingServer(t, &connections)
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "same-client-session",
	})
	provider.Channel.BaseURL = stringPtr(server.URL)
	enableCodexResponsesWSSelfHostedForTest(t, provider)

	sessionA, errWithCode := openCodexResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{})
	if errWithCode != nil {
		t.Fatalf("open first ResponsesWS session: %v", errWithCode)
	}
	defer sessionA.Abort("test_cleanup")
	sessionB, errWithCode := openCodexResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{})
	if errWithCode != nil {
		t.Fatalf("open second ResponsesWS session: %v", errWithCode)
	}
	defer sessionB.Abort("test_cleanup")

	if _, ok := sessionA.(*responsesws.NativeSession); !ok {
		t.Fatalf("expected first session to use common native helper, got %T", sessionA)
	}
	if _, ok := sessionB.(*responsesws.NativeSession); !ok {
		t.Fatalf("expected second session to use common native helper, got %T", sessionB)
	}
	waitForCodexRealtimeConnectionCount(t, &connections, 2)
}

func waitForCodexRealtimeConnectionCount(t *testing.T, connections *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for connections.Load() != want {
		select {
		case <-deadline:
			t.Fatalf("expected %d upstream websocket connection(s), got %d", want, connections.Load())
		case <-ticker.C:
		}
	}
}

func TestCodexResponsesWSNativeDoesNotInjectOpenPreviousResponseID(t *testing.T) {
	received := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read upstream response.create: %v", err)
			return
		}
		received <- payload
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, nil)
	provider.Channel.BaseURL = stringPtr(server.URL)
	enableCodexResponsesWSSelfHostedForTest(t, provider)

	session, errWithCode := openCodexResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{
		UpstreamSessionID: "responses-ws-previous-default-test",
	})
	if errWithCode != nil {
		t.Fatalf("open ResponsesWS session: %v", errWithCode)
	}
	defer session.Abort("test_cleanup")

	result := session.SendClientWithResult(context.Background(), responsesws.SendRequest{
		AttemptID: "attempt-previous-default",
		Frame:     responsesws.NewTextFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`)),
	})
	if result.Status != responsesws.ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("send response.create: %+v", result)
	}

	select {
	case got := <-received:
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Fatalf("decode upstream payload: %v payload=%s", err, got)
		}
		if _, exists := decoded["previous_response_id"]; exists {
			t.Fatalf("expected native transport to preserve the client frame without relay continuation injection, payload=%s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream response.create")
	}
}

func TestCodexResponsesWSNativeRejectsMismatchedSubsequentMetadataBeforeWrite(t *testing.T) {
	received := make(chan []byte, 1)
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		go func() {
			for {
				_, payload, err := conn.ReadMessage()
				if err != nil {
					return
				}
				received <- payload
			}
		}()
		<-done
		conn.Close()
	}))
	defer func() {
		close(done)
		server.Close()
	}()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, nil)
	provider.Channel.BaseURL = stringPtr(server.URL)
	enableCodexResponsesWSSelfHostedForTest(t, provider)

	openFrame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{
		"type":"response.create",
		"model":"gpt-5",
		"input":"hello",
		"client_metadata":{"session_id":"sess-open"}
	}`))
	if err != nil {
		t.Fatalf("parse open frame: %v", err)
	}
	session, errWithCode := openCodexResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{
		UpstreamSessionID: "responses-ws-metadata-mismatch-test",
		FirstFrame:        openFrame,
	})
	if errWithCode != nil {
		t.Fatalf("open ResponsesWS session: %v", errWithCode)
	}
	defer session.Abort("test_cleanup")

	result := session.SendClientWithResult(context.Background(), responsesws.SendRequest{
		AttemptID: "attempt-bad-metadata",
		Frame: responsesws.NewTextFrame([]byte(`{
			"type":"response.create",
			"model":"gpt-5",
			"input":"hi",
			"client_metadata":{"session_id":"sess-frame-other"}
		}`)),
	})
	if result.Status != responsesws.ResponsesWSTransportSendNotAttempted || result.Err == nil {
		t.Fatalf("expected mismatched metadata to be rejected before upstream write, got %+v", result)
	}
	if !strings.Contains(result.Err.Error(), "client_metadata.session_id") {
		t.Fatalf("expected client_metadata.session_id rejection, got %v", result.Err)
	}

	select {
	case payload := <-received:
		t.Fatalf("expected rejected frame not to reach upstream, got %s", payload)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCodexResponsesWSNativeRewritesNestedPayloadAndPreservesUnknownFields(t *testing.T) {
	received := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read upstream response.create: %v", err)
			return
		}
		received <- payload
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, nil)
	provider.Channel.BaseURL = stringPtr(server.URL)
	enableCodexResponsesWSSelfHostedForTest(t, provider)

	session, errWithCode := openCodexResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{UpstreamSessionID: "responses-ws-rewrite-test"})
	if errWithCode != nil {
		t.Fatalf("open ResponsesWS session: %v", errWithCode)
	}
	defer session.Abort("test_cleanup")

	payload := []byte(`{"type":"response.create","event_id":"evt_raw","model":"gpt-5","input":"hi","temperature":0.7,"top_p":0.9,"unknown_number":12345678901234567890,"future_object":{"enabled":true}}`)
	result := session.SendClientWithResult(context.Background(), responsesws.SendRequest{
		AttemptID: "attempt-raw-payload",
		Frame:     responsesws.NewTextFrame(payload),
	})
	if result.Status != responsesws.ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("send response.create: %+v", result)
	}

	select {
	case got := <-received:
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Fatalf("decode upstream payload: %v payload=%s", err, got)
		}
		if string(decoded["type"]) != `"response.create"` {
			t.Fatalf("expected native adapter to preserve websocket event type, got %s", decoded["type"])
		}
		if string(decoded["event_id"]) != `"evt_raw"` {
			t.Fatalf("expected native adapter to preserve websocket event_id, got %s", decoded["event_id"])
		}
		if string(decoded["unknown_number"]) != `12345678901234567890` {
			t.Fatalf("expected unknown numeric field to be preserved, got %s", decoded["unknown_number"])
		}
		if string(decoded["future_object"]) != `{"enabled":true}` {
			t.Fatalf("expected unknown object field to be preserved, got %s", decoded["future_object"])
		}
		if string(decoded["top_p"]) != `0.9` {
			t.Fatalf("expected top_p to be preserved by raw WS planner, got %s", decoded["top_p"])
		}
		if _, ok := decoded["store"]; ok {
			t.Fatalf("expected Codex native adapter not to inject store, got %s", decoded["store"])
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream response.create")
	}
}

func codexTestUnknownKindFrame(payload []byte) runtimerealtime.Frame {
	frame := runtimerealtime.NewTextFrame(payload)
	field := reflect.ValueOf(&frame).Elem().FieldByName("kind")
	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().SetInt(99)
	return frame
}

func TestCodexRealtimeOutboundFromCloseInfoClassifiesPeerAndNonPeer(t *testing.T) {
	peer := codexRealtimeOutboundFromCloseInfo(wsconn.CloseInfo{
		Kind:   wsconn.CloseKindPeerClose,
		Code:   wsconn.ClosePolicyViolation,
		Reason: "quota exhausted",
	})
	if peer.providerClose == nil || peer.providerClose.Code != int(wsconn.ClosePolicyViolation) || peer.providerClose.Reason != "quota exhausted" {
		t.Fatalf("expected peer close to become ProviderClose, got %+v", peer)
	}
	if peer.payload != nil || peer.err != nil || peer.origin != runtimerealtime.RealtimePayloadOriginProvider {
		t.Fatalf("expected peer close to avoid payload/err, got %+v", peer)
	}

	normal := codexRealtimeOutboundFromCloseInfo(wsconn.CloseInfo{
		Kind:   wsconn.CloseKindNormal,
		Code:   wsconn.CloseNormalClosure,
		Reason: "normal",
	})
	if normal.providerClose == nil || normal.providerClose.Code != int(wsconn.CloseNormalClosure) || normal.providerClose.Reason != "normal" {
		t.Fatalf("expected normal close to become ProviderClose, got %+v", normal)
	}
	if normal.payload != nil || normal.err != nil || normal.origin != runtimerealtime.RealtimePayloadOriginProvider {
		t.Fatalf("expected normal close to avoid payload/err, got %+v", normal)
	}

	for _, kind := range []wsconn.CloseKind{
		wsconn.CloseKindReadError,
		wsconn.CloseKindBackpressure,
		wsconn.CloseKindPongMiss,
		wsconn.CloseKindHandlerPanic,
	} {
		t.Run(string(kind), func(t *testing.T) {
			outbound := codexRealtimeOutboundFromCloseInfo(wsconn.CloseInfo{Kind: kind, Reason: string(kind)})
			if outbound.providerClose != nil {
				t.Fatalf("expected non-peer close not to become ProviderClose, got %+v", outbound.providerClose)
			}
			if !strings.Contains(string(outbound.payload), "provider_connection_closed") {
				t.Fatalf("expected provider_connection_closed payload, got %+v", outbound)
			}
			if !errors.Is(outbound.err, runtimerealtime.ErrSessionClosed) {
				t.Fatalf("expected non-peer close to carry RecvEvent.Err source, got %v", outbound.err)
			}
			if outbound.origin != runtimerealtime.RealtimePayloadOriginProxyLocal {
				t.Fatalf("expected proxy-local origin for non-peer close, got %v", outbound.origin)
			}
		})
	}
}

func TestCodexTurnReadTimeoutClosesCurrentWebsocketWithAbort(t *testing.T) {
	originalTimeout := codexRealtimeTurnReadTimeout
	codexRealtimeTurnReadTimeout = 10 * time.Millisecond
	t.Cleanup(func() {
		codexRealtimeTurnReadTimeout = originalTimeout
	})

	conn, cleanup := newCodexRealtimeConnPair(t)
	defer cleanup()

	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "turn-read-timeout",
		SessionID: "turn-read-timeout",
		Model:     "gpt-5",
		Protocol:  codexRealtimeProtocolName,
	})
	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	state.wsConn = conn
	state.wsConnGeneration = 1
	state.turnSeq = 1
	exec.Inflight = true
	armCodexTurnReadTimeoutLocked(exec, state)
	exec.Unlock()

	select {
	case <-conn.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for turn read timeout to close websocket")
	}
	if info := conn.CloseInfo(); info.Kind != wsconn.CloseKindAbort || info.Reason != "turn_read_timeout" {
		t.Fatalf("expected turn read timeout to close with abort reason, got %+v", info)
	}
}

func TestCodexTurnReadTimeoutIgnoresStaleConnectionGeneration(t *testing.T) {
	originalTimeout := codexRealtimeTurnReadTimeout
	codexRealtimeTurnReadTimeout = 10 * time.Millisecond
	t.Cleanup(func() {
		codexRealtimeTurnReadTimeout = originalTimeout
	})

	oldConn, oldCleanup := newCodexRealtimeConnPair(t)
	defer oldCleanup()
	newConn, newCleanup := newCodexRealtimeConnPair(t)
	defer newCleanup()

	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "turn-read-timeout-stale",
		SessionID: "turn-read-timeout-stale",
		Model:     "gpt-5",
		Protocol:  codexRealtimeProtocolName,
	})
	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	state.wsConn = oldConn
	state.wsConnGeneration = 1
	state.turnSeq = 1
	exec.Inflight = true
	armCodexTurnReadTimeoutLocked(exec, state)
	state.wsConn = newConn
	state.wsConnGeneration = 2
	state.turnSeq = 2
	exec.Unlock()

	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-oldConn.Done():
		t.Fatalf("stale turn timer closed old websocket: %+v", oldConn.CloseInfo())
	case <-timer.C:
	}
	select {
	case <-newConn.Done():
		t.Fatalf("stale turn timer closed replacement websocket: %+v", newConn.CloseInfo())
	default:
	}
}

type admissionFailingCodexTurnObserver struct {
	recordingTurnObserver
	admitErr       error
	admitCount     int
	rollbackCount  int
	rollbackReason string
}

func (o *admissionFailingCodexTurnObserver) AdmitTurn() error {
	o.admitCount++
	return o.admitErr
}

func (o *admissionFailingCodexTurnObserver) RollbackTurnAdmission(reason string) error {
	o.rollbackCount++
	o.rollbackReason = reason
	return nil
}

func TestCodexRealtimeAttachmentTurnAndErrorHelpers(t *testing.T) {
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "channel:1/hash-a/session-a",
		SessionID: "session-a",
		Model:     "gpt-5",
	})
	state := getCodexManagedRuntimeStateLocked(exec)
	if state == nil {
		t.Fatal("expected managed runtime state to be created")
	}
	if getCodexManagedRuntimeStateLocked(exec) != state {
		t.Fatal("expected repeated state lookup to reuse managed runtime state")
	}

	conn, cleanupConn := newCodexRealtimeConnPair(t)
	defer cleanupConn()
	state.wsConn = conn
	state.wsReaderConn = conn
	state.skipBootstrapConn = conn
	if cleared := clearCodexManagedWebsocketLocked(state); cleared.conn != conn || state.wsConn != nil || state.wsReaderConn != nil || state.skipBootstrapConn != nil {
		t.Fatalf("expected clearCodexManagedWebsocketLocked to clear shared websocket references, cleared=%v state=%+v", cleared, state)
	}
	if cleared := clearCodexManagedWebsocketLocked(nil); cleared.conn != nil {
		t.Fatalf("expected nil managed websocket clear to return nil, got %v", cleared)
	}

	if err := writeCodexRealtimeWSMessage(nil, wsconn.TextMessage, []byte("hello")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("expected nil websocket write to return net.ErrClosed, got %v", err)
	}

	conn, cleanupWrite := newCodexRealtimeConnPair(t)
	defer cleanupWrite()
	state = &codexManagedRuntimeState{}
	if err := writeCodexRealtimeWSMessage(conn, wsconn.TextMessage, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("expected websocket helper write to succeed, got %v", err)
	}

	if attachment := newCodexAttachment(); attachment == nil || len(attachment.queue) != codexRealtimeAttachmentQueueCapacity {
		t.Fatalf("expected default attachment queue capacity %d, got %+v", codexRealtimeAttachmentQueueCapacity, attachment)
	}
	attachment := newCodexAttachmentWithCapacity(1)
	if attachment == nil || len(attachment.queue) != 1 {
		t.Fatalf("expected explicit attachment capacity, got %+v", attachment)
	}
	if ok := enqueueCodexOutbound(attachment, codexRealtimeOutbound{messageType: wsconn.TextMessage, payload: []byte("first")}); !ok {
		t.Fatal("expected first attachment enqueue to succeed")
	}
	if outbound := recvCodexAttachmentOutbound(t, attachment); outbound.messageType != wsconn.TextMessage || string(outbound.payload) != "first" {
		t.Fatalf("expected attachment recv to return queued payload, got %+v", outbound)
	}
	attachment.close()
	if !attachment.isClosed() {
		t.Fatal("expected attachment close to mark closed")
	}
	if outbound, err := attachment.recv(context.Background()); !errors.Is(err, runtimerealtime.ErrSessionClosed) || outbound.messageType != 0 {
		t.Fatalf("expected closed attachment recv to fail with session closed, outbound=%+v err=%v", outbound, err)
	}
	if outbound, err := (*codexAttachment)(nil).recv(context.Background()); !errors.Is(err, runtimerealtime.ErrSessionClosed) || outbound.messageType != 0 {
		t.Fatalf("expected nil attachment recv to fail with session closed, outbound=%+v err=%v", outbound, err)
	}
	if event, err := (&codexManagedRealtimeSession{attachment: attachment}).Recv(context.Background()); !errors.Is(err, runtimerealtime.ErrSessionClosed) || event != (runtimerealtime.RecvEvent{}) {
		t.Fatalf("expected top-level recv error to return zero RecvEvent, event=%+v err=%v", event, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if outbound, err := newCodexAttachmentWithCapacity(1).recv(ctx); !errors.Is(err, context.Canceled) || outbound.messageType != 0 {
		t.Fatalf("expected canceled attachment recv, outbound=%+v err=%v", outbound, err)
	}
	providerClose := &runtimerealtime.ProviderClose{Code: int(wsconn.ClosePolicyViolation), Reason: "quota exhausted", Err: runtimerealtime.ErrSessionClosed}
	providerCloseAttachment := newCodexAttachmentWithCapacity(1)
	if ok := enqueueCodexOutbound(providerCloseAttachment, codexRealtimeOutbound{
		providerClose: providerClose,
		origin:        runtimerealtime.RealtimePayloadOriginProvider,
	}); !ok {
		t.Fatal("expected provider close attachment enqueue to succeed")
	}
	providerCloseSession := &codexManagedRealtimeSession{attachment: providerCloseAttachment}
	if event, err := providerCloseSession.Recv(context.Background()); err != nil || event.ProviderClose != providerClose || event.Frame != nil || event.Usage != nil || event.Err != nil || event.Origin != runtimerealtime.RealtimePayloadOriginProvider {
		t.Fatalf("expected provider close only event, event=%+v err=%v", event, err)
	}

	providerErr := errors.New("provider business error")
	providerErrAttachment := newCodexAttachmentWithCapacity(1)
	if ok := enqueueCodexOutbound(providerErrAttachment, codexRealtimeOutbound{
		err:    providerErr,
		origin: runtimerealtime.RealtimePayloadOriginProvider,
	}); !ok {
		t.Fatal("expected provider error attachment enqueue to succeed")
	}
	providerErrSession := &codexManagedRealtimeSession{attachment: providerErrAttachment}
	if event, err := providerErrSession.Recv(context.Background()); err != nil || !errors.Is(event.Err, providerErr) || event.Frame != nil || event.Usage != nil || event.ProviderClose != nil || event.Origin != runtimerealtime.RealtimePayloadOriginProvider {
		t.Fatalf("expected provider error event without top-level error, event=%+v err=%v", event, err)
	}

	frameUsageAttachment := newCodexAttachmentWithCapacity(1)
	if ok := enqueueCodexOutbound(frameUsageAttachment, codexRealtimeOutbound{
		messageType: wsconn.TextMessage,
		payload:     []byte("queued"),
		usage:       &types.UsageEvent{TotalTokens: 1},
		origin:      runtimerealtime.RealtimePayloadOriginProvider,
	}); !ok {
		t.Fatal("expected frame+usage attachment enqueue to succeed")
	}
	frameUsageSession := &codexManagedRealtimeSession{attachment: frameUsageAttachment}
	if event, err := frameUsageSession.Recv(context.Background()); err != nil || event.Frame == nil || event.Frame.Kind() != runtimerealtime.FrameKindText || event.Usage == nil || event.Usage.TotalTokens != 1 || event.ProviderClose != nil || event.Err != nil || event.Origin != runtimerealtime.RealtimePayloadOriginProvider {
		t.Fatalf("expected frame+usage event without top-level error, event=%+v err=%v", event, err)
	}
	observerErr := runtimerealtime.NewClientPayloadError(errors.New("quota"), []byte(`{"type":"error","error":{"message":"quota"}}`))
	frameUsageErrAttachment := newCodexAttachmentWithCapacity(2)
	if ok := enqueueCodexOutbound(frameUsageErrAttachment, codexRealtimeOutbound{
		messageType: wsconn.TextMessage,
		payload:     []byte(`{"type":"response.done","response":{"usage":{"total_tokens":1}}}`),
		usage:       &types.UsageEvent{TotalTokens: 1},
		origin:      runtimerealtime.RealtimePayloadOriginProvider,
		err:         observerErr,
	}); !ok {
		t.Fatal("expected frame+usage+err attachment enqueue to split events")
	}
	frameUsageErrSession := &codexManagedRealtimeSession{attachment: frameUsageErrAttachment}
	if event, err := frameUsageErrSession.Recv(context.Background()); err != nil || event.Frame == nil || event.Usage == nil || event.Err != nil || event.ProviderClose != nil || event.Origin != runtimerealtime.RealtimePayloadOriginProvider {
		t.Fatalf("expected first split event to preserve frame+usage without err, event=%+v err=%v", event, err)
	}
	if event, err := frameUsageErrSession.Recv(context.Background()); err != nil || event.Frame == nil || event.Usage != nil || !errors.Is(event.Err, observerErr) || event.ProviderClose != nil || event.Origin != runtimerealtime.RealtimePayloadOriginProxyLocal {
		t.Fatalf("expected second split event to carry client payload error without usage, event=%+v err=%v", event, err)
	}
	if normalized := normalizeCodexRealtimeOutbound(codexRealtimeOutbound{
		messageType:   wsconn.TextMessage,
		payload:       []byte("ignored"),
		providerClose: providerClose,
		usage:         &types.UsageEvent{TotalTokens: 1},
		origin:        runtimerealtime.RealtimePayloadOriginProvider,
		err:           errors.New("ignored"),
	}); len(normalized) != 1 || normalized[0].providerClose != providerClose || len(normalized[0].payload) != 0 || normalized[0].usage != nil || normalized[0].err != nil {
		t.Fatalf("expected provider close normalization to keep only ProviderClose, got %+v", normalized)
	}
	if normalized := normalizeCodexRealtimeOutbound(codexRealtimeOutbound{
		usage:  &types.UsageEvent{TotalTokens: 2},
		origin: runtimerealtime.RealtimePayloadOriginProvider,
		err:    observerErr,
	}); len(normalized) != 2 || normalized[0].usage == nil || normalized[0].err != nil || normalized[1].usage != nil || !errors.Is(normalized[1].err, observerErr) {
		t.Fatalf("unexpected normalized codex usage+err shape: %+v", normalized)
	}

	originalBackpressure := codexRealtimeOutboundBackpressureTimeout
	codexRealtimeOutboundBackpressureTimeout = 10 * time.Millisecond
	t.Cleanup(func() {
		codexRealtimeOutboundBackpressureTimeout = originalBackpressure
	})
	backpressured := newCodexAttachmentWithCapacity(1)
	if ok := enqueueCodexOutbound(backpressured, codexRealtimeOutbound{messageType: wsconn.TextMessage}); !ok {
		t.Fatal("expected initial backpressure queue fill to succeed")
	}
	if ok := enqueueCodexOutbound(backpressured, codexRealtimeOutbound{messageType: wsconn.TextMessage}); ok {
		t.Fatal("expected timed-out backpressure enqueue to fail")
	}
	if !backpressured.isClosed() {
		t.Fatal("expected timed-out backpressure enqueue to close attachment")
	}
	if ok := enqueueCodexOutbound(nil, codexRealtimeOutbound{}); ok {
		t.Fatal("expected nil attachment enqueue to fail")
	}

	if !isCodexRealtimeBootstrapMessage(wsconn.TextMessage, []byte(`{"type":"session.created"}`)) {
		t.Fatal("expected session.created to be treated as bootstrap message")
	}
	if !isCodexRealtimeBootstrapMessage(wsconn.TextMessage, []byte(`{"type":"session.created","session":"opaque"}`)) {
		t.Fatal("expected opaque session.created to be treated as bootstrap message")
	}
	if isCodexRealtimeBootstrapMessage(wsconn.BinaryMessage, []byte(`{"type":"session.created"}`)) {
		t.Fatal("expected non-text realtime bootstrap message to be ignored")
	}
	if isCodexRealtimeBootstrapMessage(wsconn.TextMessage, []byte("not-json")) {
		t.Fatal("expected invalid bootstrap json to be ignored")
	}

	state = &codexManagedRuntimeState{}
	beginCodexTurnLocked(state, time.Time{})
	if state.turnSeq != 1 || state.turnUsage == nil || state.turnAccumulator == nil {
		t.Fatalf("expected beginCodexTurnLocked to initialize turn state, got %+v", state)
	}
	mergeCodexTurnUsageLocked(state, &types.UsageEvent{InputTokens: 3, TotalTokens: 3})
	markCodexTurnFirstResponseLocked(state, time.Time{})
	observer, finalizePayload := finalizeCodexTurnLocked(exec, state, "response.done", time.Time{})
	if observer != nil {
		t.Fatalf("expected finalizeCodexTurnLocked without observer factory to return nil observer, got %+v", observer)
	}
	if finalizePayload.TurnSeq != 1 || finalizePayload.TerminationReason != "response.done" || finalizePayload.Usage == nil || finalizePayload.Usage.InputTokens != 3 {
		t.Fatalf("expected finalized turn payload to preserve state, got %+v", finalizePayload)
	}
	resetCodexTurnLocked(state)
	if state.turnUsage != nil || state.turnAccumulator != nil || state.turnSeq != 1 {
		t.Fatalf("expected resetCodexTurnLocked to clear active turn state, got %+v", state)
	}
	mergeCodexTurnUsageLocked(nil, &types.UsageEvent{InputTokens: 1})
	markCodexTurnFirstResponseLocked(&codexManagedRuntimeState{turnSeq: 1, turnFinalized: true}, time.Now())
	if observer, payload := finalizeCodexTurnLocked(nil, state, "ignored", time.Now()); observer != nil || payload.TurnSeq != 0 {
		t.Fatalf("expected finalizeCodexTurnLocked nil guard, observer=%+v payload=%+v", observer, payload)
	}

	guardedRecorder := &recordingTurnObserver{}
	state = &codexManagedRuntimeState{
		turnObserverFactory: func() runtimesession.TurnObserver { return guardedRecorder },
	}
	beginCodexTurnLocked(state, time.Now())
	if state.turnObserver == nil {
		t.Fatal("expected beginCodexTurnLocked to wrap a factory-produced turn observer")
	}
	if err := state.turnObserver.ObserveTurnUsage(&types.UsageEvent{TotalTokens: 1}); err != nil {
		t.Fatalf("expected guarded begin-turn observer to pass through usage, got %v", err)
	}
	state.turnObserver.FinalizeTurn(runtimesession.TurnFinalizePayload{SessionID: "session-guarded", TurnSeq: 1})
	state.turnObserver.FinalizeTurn(runtimesession.TurnFinalizePayload{SessionID: "session-guarded", TurnSeq: 2})
	if err := state.turnObserver.ObserveTurnUsage(&types.UsageEvent{TotalTokens: 2}); err != nil {
		t.Fatalf("expected guarded begin-turn observer to no-op after finalize, got %v", err)
	}
	if got := guardedRecorder.observeCount(); got != 1 {
		t.Fatalf("expected guarded begin-turn observer to suppress post-finalize usage, got %d observations", got)
	}
	if got := guardedRecorder.finalizeCount(); got != 1 {
		t.Fatalf("expected guarded begin-turn observer to finalize once, got %d", got)
	}

	recorder := &recordingTurnObserver{}
	if err := observeCodexTurnUsage(nil, &types.UsageEvent{InputTokens: 1}); err != nil {
		t.Fatalf("expected nil turn observer usage observe to be ignored, got %v", err)
	}
	if err := observeCodexTurnUsage(recorder, &types.UsageEvent{InputTokens: 2}); err != nil || recorder.observeCount() != 1 {
		t.Fatalf("expected observer usage helper to clone and forward usage, err=%v observed=%d", err, recorder.observeCount())
	}

	eventErr := types.NewErrorEvent("evt_usage", "invalid_request_error", "bad_request", "boom")
	if err := codexRealtimeTurnUsageError(eventErr); err == nil || !errors.As(err, &eventErr) {
		t.Fatalf("expected event-backed turn usage error to be wrapped as client payload, got %v", err)
	}
	if err := codexRealtimeTurnUsageError(errors.New("quota")); err == nil {
		t.Fatal("expected generic turn usage error to be wrapped")
	}
	if err := codexRealtimeTurnUsageError(nil); err != nil {
		t.Fatalf("expected nil turn usage error input to stay nil, got %v", err)
	}

	if got := codexSupplierTerminationReason("", &types.OpenAIResponsesResponses{Status: "completed"}); got != "response.completed" {
		t.Fatalf("expected response status to drive termination reason, got %q", got)
	}
	if got := codexSupplierTerminationReason("response.failed", nil); got != "response.failed" {
		t.Fatalf("expected event type fallback termination reason, got %q", got)
	}

	if terminal, responseID, reason := inspectCodexSupplierMessage(wsconn.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_supplier","status":"completed"}}`)); !terminal || responseID != "resp_supplier" || reason != "response.completed" {
		t.Fatalf("expected supplier terminal event inspection, terminal=%v response_id=%q reason=%q", terminal, responseID, reason)
	}
	if terminal, responseID, reason := inspectCodexSupplierMessage(wsconn.TextMessage, []byte("bad-json")); terminal || responseID != "" || reason != "" {
		t.Fatalf("expected invalid supplier payload inspection fallback, terminal=%v response_id=%q reason=%q", terminal, responseID, reason)
	}
	if terminal, responseID, reason := inspectCodexSupplierEvent(nil); terminal || responseID != "" || reason != "" {
		t.Fatalf("expected nil realtime event inspection fallback, terminal=%v response_id=%q reason=%q", terminal, responseID, reason)
	}
	if terminal, responseID, reason := inspectCodexSupplierEvent(&types.OpenAIResponsesStreamResponses{Type: "error", Response: &types.OpenAIResponsesResponses{ID: "resp_error"}}); !terminal || responseID != "resp_error" || reason != "error" {
		t.Fatalf("expected error realtime event inspection, terminal=%v response_id=%q reason=%q", terminal, responseID, reason)
	}
	if terminal, responseID, reason := inspectCodexSupplierEvent(&types.OpenAIResponsesStreamResponses{Type: "error"}); terminal || responseID != "" || reason != "" {
		t.Fatalf("uncorrelated top-level error became a turn terminal: terminal=%v response_id=%q reason=%q", terminal, responseID, reason)
	}

	if err := newCodexRealtimeClientError("evt_client", "bad_request", "boom"); err == nil {
		t.Fatal("expected realtime client helper to build typed error")
	}
	if err := newCodexRealtimeProviderError("evt_provider", "provider_failed", "boom"); err == nil {
		t.Fatal("expected realtime provider helper to build typed error")
	}
	if got := codexRealtimeErrorCodeString("", "fallback"); got != "fallback" {
		t.Fatalf("expected empty realtime error code fallback, got %q", got)
	}
	if got := codexRealtimeErrorCodeString(123, "fallback"); got != "123" {
		t.Fatalf("expected fmt-based realtime error code fallback, got %q", got)
	}
	if err := codexRealtimeErrorFromOpenAIError("evt_nil", nil); err == nil {
		t.Fatal("expected nil OpenAI error wrapper input to produce provider error")
	}
	if err := codexRealtimeErrorFromOpenAIError("evt_with_code", &types.OpenAIErrorWithStatusCode{OpenAIError: types.OpenAIError{Code: "quota_exhausted", Message: "quota"}}); err == nil {
		t.Fatal("expected OpenAI error wrapper input to produce provider error")
	}

}

func TestCodexRealtimeWSReaderForwardsProviderCloseCode(t *testing.T) {
	closeSent := make(chan struct{})
	wsURL, cleanupServer := wstest.Server(t, func(conn *wsconn.ManagedConn) {
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindGracefulShutdown, Code: wsconn.CloseCode(4408), Reason: "quota exhausted"})
		close(closeSent)
		<-conn.Done()
	})
	defer cleanupServer()

	conn, cleanup := newCodexRealtimeConnPairFromURL(t, wsURL)
	defer cleanup()

	provider := &CodexProvider{}
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "provider-close-code",
		SessionID: "provider-close-code",
		Model:     "gpt-5",
		Protocol:  codexRealtimeProtocolName,
	})
	attachment := newCodexAttachment()

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	assignCodexAttachmentOwnerLocked(state, attachment)
	state.wsConn = conn
	exec.Attached = true
	exec.Inflight = true
	provider.startRealtimeWSReaderLocked(exec, state)
	exec.Unlock()

	select {
	case <-closeSent:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for test server to send close frame")
	}

	outbound := recvCodexAttachmentOutbound(t, attachment)
	if outbound.messageType != 0 || len(outbound.payload) != 0 {
		t.Fatalf("expected provider close not to be forwarded as data frame, got message_type=%d payload=%q", outbound.messageType, outbound.payload)
	}
	if outbound.origin != runtimerealtime.RealtimePayloadOriginProvider {
		t.Fatalf("expected provider close origin, got %v", outbound.origin)
	}
	if outbound.providerClose == nil {
		t.Fatalf("expected provider close event, got outbound=%+v", outbound)
	}
	if outbound.providerClose.Code != 4408 {
		t.Fatalf("expected provider close code 4408, got %d", outbound.providerClose.Code)
	}
	if outbound.providerClose.Reason != "quota exhausted" {
		t.Fatalf("expected provider close reason to be preserved, got %q", outbound.providerClose.Reason)
	}
	if !errors.Is(outbound.providerClose.Err, runtimerealtime.ErrSessionClosed) {
		t.Fatalf("expected provider close to carry session closed error, got %v", outbound.providerClose.Err)
	}

	exec.Lock()
	defer exec.Unlock()
	state = getCodexManagedRuntimeStateLocked(exec)
	if state.wsConn != nil || exec.Inflight || exec.State != runtimesession.SessionStateIdle {
		t.Fatalf("expected Pump.OnClose path to clear websocket state and mark idle, exec=%+v state=%+v", exec, state)
	}
}

func TestPrepareCodexRealtimeCreatePayloadPreservesUnknownResponseFields(t *testing.T) {
	provider := &CodexProvider{}
	eventID, request, encoded, err := provider.prepareCodexRealtimeCreatePayload([]byte(`{"type":"response.create","event_id":"evt_raw","model":"gpt-5","input":"hi","temperature":0.7,"top_p":0.9,"context_management":{"mode":"unsupported"},"truncation":"auto","unknown_number":12345678901234567890,"future_object":{"enabled":true}}`), runtimesession.ModelBinding{RequestedModel: "gpt-5", ProviderModel: "gpt-5", BillingModel: "gpt-5"})
	if err != nil {
		t.Fatalf("expected realtime create payload prepare to succeed, got %v", err)
	}
	if eventID != "evt_raw" || request == nil || request.Model != "gpt-5" {
		t.Fatalf("unexpected prepared event id/request: event_id=%q request=%+v", eventID, request)
	}

	var response map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatalf("decode prepared response.create payload: %v", err)
	}
	if string(response["unknown_number"]) != `12345678901234567890` {
		t.Fatalf("expected unknown numeric field to be preserved, got %s", response["unknown_number"])
	}
	if string(response["future_object"]) != `{"enabled":true}` {
		t.Fatalf("expected unknown object field to be preserved, got %s", response["future_object"])
	}
	for _, field := range []string{"temperature", "top_p", "context_management", "truncation"} {
		if _, ok := response[field]; !ok {
			t.Fatalf("expected Codex adapter to preserve upstream field %s, payload=%s", field, encoded)
		}
	}
	if _, ok := response["store"]; ok {
		t.Fatal("unexpected store patch")
	}
	if _, ok := response["include"]; ok {
		t.Fatal("unexpected include patch")
	}

}

func TestPrepareCodexRealtimeCreatePayloadRejectsAccountScopedResources(t *testing.T) {
	provider := &CodexProvider{}
	for _, payload := range []string{
		`{"type":"response.create","event_id":"evt_file","model":"gpt-5","input":[{"type":"input_file","file_id":"file_shared"}]}`,
		`{"type":"response.create","event_id":"evt_tool","model":"gpt-5","tools":[{"type":"file_search","vector_store_ids":["vs_shared"]}]}`,
		`{"type":"response.create","event_id":"evt_skill","model":"gpt-5","tools":[{"type":"function","skill_reference":"skill_shared"}]}`,
	} {
		if _, _, _, err := provider.prepareCodexRealtimeCreatePayload([]byte(payload), runtimesession.ModelBinding{RequestedModel: "gpt-5", ProviderModel: "gpt-5", BillingModel: "gpt-5"}); err == nil || !strings.Contains(err.Error(), "unsupported_resource_reference") {
			t.Fatalf("expected realtime resource gate before provider work, payload=%s err=%v", payload, err)
		}
	}
}

func TestConfigureCodexRealtimeConnAppliesReadLimit(t *testing.T) {
	const limit = int64(64)
	const oversizedPayloadBytes = 512

	previousLimit := viper.Get("realtime.websocket_read_limit")
	viper.Set("realtime.websocket_read_limit", limit)
	t.Cleanup(func() {
		viper.Set("realtime.websocket_read_limit", previousLimit)
	})

	releaseWrite := make(chan struct{})
	wsURL, cleanupServer := wstest.Server(t, func(conn *wsconn.ManagedConn) {
		<-releaseWrite
		if err := conn.WriteMessage(wsconn.TextMessage, []byte(strings.Repeat("x", oversizedPayloadBytes))); err != nil {
			t.Errorf("failed to write oversized websocket frame: %v", err)
		}
		<-conn.Done()
	})
	defer cleanupServer()

	conn, cleanup := newCodexRealtimeConnPairFromURL(t, wsURL)
	defer cleanup()

	provider := &CodexProvider{}
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "read-limit",
		SessionID: "read-limit",
		Model:     "gpt-5",
		Protocol:  codexRealtimeProtocolName,
	})
	attachment := newCodexAttachment()
	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	assignCodexAttachmentOwnerLocked(state, attachment)
	state.wsConn = conn
	exec.Attached = true
	exec.Inflight = true
	provider.startRealtimeWSReaderLocked(exec, state)
	exec.Unlock()
	close(releaseWrite)

	outbound := recvCodexAttachmentOutbound(t, attachment)
	if !strings.Contains(string(outbound.payload), "provider_connection_closed") {
		t.Fatalf("expected read limit to produce provider_connection_closed payload, got %+v", outbound)
	}
	if !errors.Is(outbound.err, runtimerealtime.ErrSessionClosed) {
		t.Fatalf("expected read limit to carry RecvEvent.Err source, got %v", outbound.err)
	}
}

func TestCodexManagedRealtimeSessionGuardBranches(t *testing.T) {
	if err := (*codexManagedRealtimeSession)(nil).SendClient(context.Background(), codexTestTextFrame([]byte(`{}`))); !errors.Is(err, runtimerealtime.ErrSessionClosed) {
		t.Fatalf("expected nil managed realtime session SendClient to report session closed, got %v", err)
	}

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, nil)
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "channel:1/hash-a/session-managed",
		SessionID: "session-managed",
		Model:     "gpt-5",
		IdleTTL:   time.Minute,
	})
	state := getCodexManagedRuntimeStateLocked(exec)
	attachment := newCodexAttachmentWithCapacity(2)
	state.attachment = attachment
	state.ownerSeq = 1
	exec.Attached = true

	session := &codexManagedRealtimeSession{
		provider:   provider,
		exec:       exec,
		attachment: attachment,
		ownerSeq:   1,
	}

	if err := session.SendClient(context.Background(), codexTestBinaryFrame([]byte("binary"))); err == nil {
		t.Fatal("expected binary realtime client payload to be rejected")
	}
	if err := session.SendClient(context.Background(), codexTestTextFrame([]byte("not-json"))); err == nil {
		t.Fatal("expected invalid realtime client json to be rejected")
	}

	foreignSession := &codexManagedRealtimeSession{
		provider:   provider,
		exec:       exec,
		attachment: attachment,
		ownerSeq:   2,
	}
	if err := foreignSession.SendClient(context.Background(), codexTestTextFrame([]byte(`{"type":"response.cancel"}`))); !errors.Is(err, runtimerealtime.ErrSessionClosed) {
		t.Fatalf("expected foreign attachment owner SendClient to be rejected, got %v", err)
	}

	if err := session.SendClient(context.Background(), codexTestTextFrame([]byte(`{"type":"response.create","event_id":"evt_missing"}`))); err == nil {
		t.Fatal("expected missing response.create model to be rejected")
	}

	exec.Inflight = true
	if err := session.SendClient(context.Background(), codexTestTextFrame([]byte(`{"type":"response.create","event_id":"evt_busy","input":[]}`))); err == nil {
		t.Fatal("expected inflight response.create to be rejected as busy")
	}
	exec.Inflight = false

	if err := session.SendClient(context.Background(), codexTestTextFrame([]byte(`{"type":"response.create","event_id":"evt_mismatch","model":"o4-mini","input":[]}`))); err == nil {
		t.Fatal("expected mismatched response.create model to be rejected")
	}

	beginCodexTurnLocked(state, time.Now())
	if err := session.SendClient(context.Background(), codexTestTextFrame([]byte(`{"type":"response.cancel","event_id":"evt_cancel"}`))); err != nil {
		t.Fatalf("expected response.cancel without wsConn to finalize locally, got %v", err)
	}
	if exec.Inflight || exec.State != runtimesession.SessionStateIdle {
		t.Fatalf("expected local response.cancel to reset inflight state, exec=%+v", exec)
	}

	if err := session.SendClient(context.Background(), codexTestTextFrame([]byte(`{"type":"unsupported","event_id":"evt_unsupported"}`))); err == nil {
		t.Fatal("expected unsupported realtime client event to be rejected")
	}
	if _, _, _, _, err := codexTestRecv(context.Background(), (*codexManagedRealtimeSession)(nil)); !errors.Is(err, runtimerealtime.ErrSessionClosed) {
		t.Fatalf("expected nil managed realtime session Recv to report session closed, got %v", err)
	}

	state.turnObserver = nil
	state.turnObserverFactory = nil
	exec.Inflight = true
	seededRecorder := &recordingTurnObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return seededRecorder })
	if state.turnObserverFactory == nil || state.turnObserver == nil {
		t.Fatalf("expected SetTurnObserverFactory to seed observer for inflight owned session, state=%+v", state)
	}
	if err := state.turnObserver.ObserveTurnUsage(&types.UsageEvent{TotalTokens: 1}); err != nil {
		t.Fatalf("expected seeded turn observer to pass through usage, got %v", err)
	}
	state.turnObserver.FinalizeTurn(runtimesession.TurnFinalizePayload{SessionID: "session-managed", TurnSeq: 1})
	state.turnObserver.FinalizeTurn(runtimesession.TurnFinalizePayload{SessionID: "session-managed", TurnSeq: 2})
	if err := state.turnObserver.ObserveTurnUsage(&types.UsageEvent{TotalTokens: 2}); err != nil {
		t.Fatalf("expected seeded turn observer to no-op after finalize, got %v", err)
	}
	if got := seededRecorder.observeCount(); got != 1 {
		t.Fatalf("expected seeded turn observer to suppress post-finalize usage, got %d observations", got)
	}
	if got := seededRecorder.finalizeCount(); got != 1 {
		t.Fatalf("expected seeded turn observer to finalize once, got %d", got)
	}
	exec.Inflight = false

	session.Detach("test_detach")
	if !attachment.isClosed() || exec.Attached {
		t.Fatalf("expected Detach to close attachment and mark exec detached, exec=%+v closed=%v", exec, attachment.isClosed())
	}

	manager := runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{DefaultTTL: time.Minute})
	replaceCodexExecutionSessionsForTest(t, manager)

	abortExec, created, releaseLease, err := manager.AcquireOrCreate(runtimesession.Metadata{
		Key:       "channel:1/hash-a/session-abort",
		SessionID: "session-abort",
		Model:     "gpt-5",
		IdleTTL:   time.Minute,
	})
	if err != nil || !created || releaseLease == nil {
		t.Fatalf("expected abort fixture execution session, created=%v release_nil=%v err=%v", created, releaseLease == nil, err)
	}
	releaseLease()
	abortState := getCodexManagedRuntimeStateLocked(abortExec)
	abortAttachment := newCodexAttachmentWithCapacity(1)
	abortConn, abortConnCleanup := newCodexRealtimeConnPair(t)
	defer abortConnCleanup()
	abortState.attachment = abortAttachment
	abortState.wsConn = abortConn
	abortState.ownerSeq = 9
	abortExec.Attached = true
	abortSession := &codexManagedRealtimeSession{
		provider:   provider,
		exec:       abortExec,
		attachment: abortAttachment,
		ownerSeq:   9,
	}
	abortSession.Abort("manual_abort")
	if !abortExec.IsClosed() || !abortAttachment.isClosed() {
		t.Fatalf("expected Abort to close owned execution session and attachment, exec_closed=%v attachment_closed=%v", abortExec.IsClosed(), abortAttachment.isClosed())
	}
	<-abortConn.Done()
	if info := abortConn.CloseInfo(); info.Kind != wsconn.CloseKindAbort {
		t.Fatalf("expected Abort to close owned websocket with CloseKindAbort, got %+v", info)
	}
	if removed := manager.DeleteIf(abortExec.Key, abortExec); removed != nil {
		t.Fatalf("expected aborted execution session to be removed from manager, removed=%+v", removed)
	}
}

func TestCodexRealtimeMetadataCompatibilityAndNamespaceBranches(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, nil)
	provider.Context.Set("id", 7001)
	provider.Context.Request.Header.Set("Authorization", "Bearer sk-session-auth")

	if _, errWithCode := provider.buildExecutionSessionMetadata("gpt-5", runtimerealtime.RealtimeOpenOptions{ClientSessionID: "bad/session"}); errWithCode == nil || errWithCode.Code != "invalid_session_id" {
		t.Fatalf("expected invalid client session id metadata error, got %+v", errWithCode)
	}
	provider.Context.Request.Header.Set("X-Session-Id", "bad/session")
	if _, _, errWithCode := provider.readRealtimeClientSessionID(runtimerealtime.RealtimeOpenOptions{}); errWithCode == nil || errWithCode.Code != "invalid_session_id" {
		t.Fatalf("expected invalid request session id to fail validation, got %+v", errWithCode)
	}
	provider.Context.Request.Header.Set("X-Session-Id", "client-session")
	if sessionID, clientSupplied, errWithCode := provider.readRealtimeClientSessionID(runtimerealtime.RealtimeOpenOptions{}); errWithCode != nil || !clientSupplied || sessionID != "client-session" {
		t.Fatalf("expected valid request session id to be returned, session=%q supplied=%v err=%v", sessionID, clientSupplied, errWithCode)
	}
	if _, _, _, ok := parseCodexExecutionSessionKey("wrong-prefix/hash/session"); ok {
		t.Fatal("expected invalid execution session key prefix to fail parsing")
	}
	if _, _, _, ok := parseCodexExecutionSessionKey("channel:0/hash/session"); ok {
		t.Fatal("expected zero channel execution session key to fail parsing")
	}
	if _, _, _, ok := parseCodexExecutionSessionKey("channel:1/hash/"); ok {
		t.Fatal("expected blank session execution session key to fail parsing")
	}

	if got := normalizeCodexRealtimeBaseURL("https://Example.COM:443/path/?q=1#fragment"); got != "https://example.com/path" {
		t.Fatalf("expected base url normalization to strip defaults and fragments, got %q", got)
	}
	if got := normalizeCodexRealtimeBaseURL("http://Example.COM:80/path/"); got != "http://example.com/path" {
		t.Fatalf("expected http base url normalization to strip default port, got %q", got)
	}
	if got := normalizeCodexRealtimeBaseURL("://bad url"); got != "://bad url" {
		t.Fatalf("expected invalid base url to fall back to trimmed input, got %q", got)
	}
	if got := normalizeCodexRealtimeBaseURL(""); got != "" {
		t.Fatalf("expected blank base url to remain blank, got %q", got)
	}

	if err := validateCodexRealtimeExecutionSessionID(strings.Repeat("a", runtimesession.ClientSessionIDMaxLen+1)); err == nil {
		t.Fatal("expected oversized realtime session id to fail validation")
	}
	if err := validateCodexRealtimeExecutionSessionID("bad/session"); err == nil {
		t.Fatal("expected unsupported realtime session id character to fail validation")
	}

	if got := readCodexRealtimeCallerNamespace(provider.Context); got != "user:7001" {
		t.Fatalf("expected caller namespace to prefer user id, got %q", got)
	}
	provider.Context.Set("id", 0)
	provider.Context.Set("token_id", 0)
	if got := readCodexRealtimeCallerNamespace(provider.Context); got != authutil.StableRequestCredentialNamespace(provider.Context.Request) {
		t.Fatalf("expected caller namespace auth fallback, got %q", got)
	}
	if got := readCodexRealtimeCapacityNamespace(provider.Context); got != authutil.StableRequestCredentialNamespace(provider.Context.Request) {
		t.Fatalf("expected capacity namespace auth fallback, got %q", got)
	}
	if got := readCodexRealtimeCallerNamespace(nil); got != "anonymous" {
		t.Fatalf("expected nil caller namespace fallback, got %q", got)
	}
	if got := readCodexRealtimeCapacityNamespace(nil); got != "anonymous" {
		t.Fatalf("expected nil capacity namespace fallback, got %q", got)
	}

	badHeaders := "{"
	provider.Channel.ModelHeaders = &badHeaders
	if headers := provider.buildRealtimeChannelCompatibilityHeaders(); len(headers) != 0 {
		t.Fatalf("expected invalid model headers to produce empty compatibility headers, got %+v", headers)
	}

	modelHeaders := `{"Authorization":"ignored","Connection":"ignored","X-Session-Id":"ignored","Originator":"codex-tui","User-Agent":"channel-ua","X-Trace":"trace"}`
	provider.Channel.ModelHeaders = &modelHeaders
	channelHeaders := provider.buildRealtimeChannelCompatibilityHeaders()
	if _, exists := channelHeaders["authorization"]; exists {
		t.Fatalf("expected authorization header to be filtered, got %+v", channelHeaders)
	}
	if channelHeaders["x-trace"] != "trace" || channelHeaders["originator"] != "codex-tui" {
		t.Fatalf("expected filtered compatibility headers to preserve x-trace/originator, got %+v", channelHeaders)
	}

	signature := requireRealtimeHandshakeSignature(t, provider)
	if !strings.Contains(signature, DefaultUserAgent()) || !strings.Contains(signature, `"originator":"pi"`) {
		t.Fatalf("expected handshake signature to use shared defaults rather than model_headers identity, got %q", signature)
	}
	if got := requireRealtimeCompatibilityHash(t, provider, "gpt-5", provider.readRealtimeUpstreamIdentity()); got == "" {
		t.Fatal("expected compatibility hash to be populated")
	}
	if got := provider.readRealtimeUpstreamIdentity(); !strings.Contains(got, "credential:account:acct-123") {
		t.Fatalf("expected upstream identity to include credential identity, got %q", got)
	}
}

func TestCodexRealtimeCompatibilityHashSeparatesClientUserAgents(t *testing.T) {
	key := `{"access_token":"access-token","account_id":"acct-123"}`
	officialProvider := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"User-Agent": "codex_cli_rs/0.116.0",
	})
	nonOfficialProvider := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"User-Agent": "curl/8.0",
	})

	officialSignature := requireRealtimeHandshakeSignature(t, officialProvider)
	if !strings.Contains(officialSignature, "codex_cli_rs/0.116.0") || strings.Contains(officialSignature, `"originator"`) {
		t.Fatalf("expected client user agent without synthesized originator in signature, got %q", officialSignature)
	}

	nonOfficialSignature := requireRealtimeHandshakeSignature(t, nonOfficialProvider)
	if !strings.Contains(nonOfficialSignature, `"originator":"pi"`) {
		t.Fatalf("expected PI fallback for non-Codex caller, got %q", nonOfficialSignature)
	}

	upstreamIdentity := officialProvider.readRealtimeUpstreamIdentity()
	if upstreamIdentity != nonOfficialProvider.readRealtimeUpstreamIdentity() {
		t.Fatalf("test setup expected matching upstream identity")
	}
	officialHash := requireRealtimeCompatibilityHash(t, officialProvider, "gpt-5", upstreamIdentity)
	nonOfficialHash := requireRealtimeCompatibilityHash(t, nonOfficialProvider, "gpt-5", upstreamIdentity)
	if officialHash == "" || nonOfficialHash == "" || officialHash == nonOfficialHash {
		t.Fatalf("expected different client user agents to produce different hashes, official=%q non_official=%q", officialHash, nonOfficialHash)
	}
}

func TestCodexRealtimeCompatibilityHashKeepsClientIdentityAheadOfConfiguration(t *testing.T) {
	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"User-Agent": "codex-tui/1.0",
	})
	provider.Channel.Other = `{"codex":{"default_user_agent":"channel-ua","default_originator":"configured"}}`

	signature := requireRealtimeHandshakeSignature(t, provider)
	if !strings.Contains(signature, "codex-tui/1.0") || strings.Contains(signature, `"originator"`) {
		t.Fatalf("expected client UA to suppress configured identity fallbacks, got %q", signature)
	}
}

func TestCodexRealtimeCompatibilityHashDistinguishesPartialClientIdentity(t *testing.T) {
	key := `{"access_token":"access-token","account_id":"acct-123"}`
	implicitProvider := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"User-Agent": "codex-tui/1.0",
	})
	explicitProvider := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"User-Agent": "codex-tui/1.0",
		"Originator": "pi",
	})

	implicitSignature := requireRealtimeHandshakeSignature(t, implicitProvider)
	if strings.Contains(implicitSignature, `"originator"`) {
		t.Fatalf("expected missing originator to stay absent in signature, got %q", implicitSignature)
	}

	explicitSignature := requireRealtimeHandshakeSignature(t, explicitProvider)
	if !strings.Contains(explicitSignature, `"originator":"pi"`) {
		t.Fatalf("expected explicit default originator to remain in signature, got %q", explicitSignature)
	}

	upstreamIdentity := implicitProvider.readRealtimeUpstreamIdentity()
	if upstreamIdentity != explicitProvider.readRealtimeUpstreamIdentity() {
		t.Fatalf("test setup expected matching upstream identity")
	}
	implicitHash := requireRealtimeCompatibilityHash(t, implicitProvider, "gpt-5", upstreamIdentity)
	explicitHash := requireRealtimeCompatibilityHash(t, explicitProvider, "gpt-5", upstreamIdentity)
	if implicitHash == "" || explicitHash == "" || implicitHash == explicitHash {
		t.Fatalf("expected absent and explicit originators to produce different compatibility hashes, implicit=%q explicit=%q", implicitHash, explicitHash)
	}
}

func TestCodexRealtimeCompatibilityHashSeparatesDifferentOfficialUserAgents(t *testing.T) {
	key := `{"access_token":"access-token","account_id":"acct-123"}`
	providerA := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"User-Agent": "codex-tui/1.0",
	})
	providerB := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"User-Agent": "Codex Desktop/1.0",
	})

	signatureA := requireRealtimeHandshakeSignature(t, providerA)
	if !strings.Contains(signatureA, "codex-tui/1.0") || strings.Contains(signatureA, `"originator"`) {
		t.Fatalf("expected client user agent without synthesized originator in signature, got %q", signatureA)
	}
	signatureB := requireRealtimeHandshakeSignature(t, providerB)
	if !strings.Contains(signatureB, "Codex Desktop/1.0") || strings.Contains(signatureB, `"originator"`) {
		t.Fatalf("expected client user agent without synthesized originator in signature, got %q", signatureB)
	}

	upstreamIdentity := providerA.readRealtimeUpstreamIdentity()
	if upstreamIdentity != providerB.readRealtimeUpstreamIdentity() {
		t.Fatalf("test setup expected matching upstream identity")
	}
	hashA := requireRealtimeCompatibilityHash(t, providerA, "gpt-5", upstreamIdentity)
	hashB := requireRealtimeCompatibilityHash(t, providerB, "gpt-5", upstreamIdentity)
	if hashA == "" || hashB == "" || hashA == hashB {
		t.Fatalf("expected different official user agents to produce different hashes, a=%q b=%q", hashA, hashB)
	}
}

func TestCodexRealtimeTransportAndDetachedSessionHelpers(t *testing.T) {
	providerForce := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, nil)
	wsExec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "channel:1/hash-a/session-ws",
		SessionID: "session-ws",
		Model:     "gpt-5",
		IdleTTL:   time.Minute,
	})
	wsConn, cleanupWSConn := newCodexRealtimeConnPair(t)
	defer cleanupWSConn()
	wsExec.Lock()
	wsState := getCodexManagedRuntimeStateLocked(wsExec)
	wsState.wsConn = wsConn
	if errWithCode := providerForce.ensureRealtimeTransportLocked(context.Background(), wsExec, wsState); errWithCode != nil {
		wsExec.Unlock()
		t.Fatalf("expected existing websocket transport path to succeed, got %v", errWithCode)
	}
	if wsState.wsReaderConn != wsConn {
		wsExec.Unlock()
		t.Fatalf("expected existing websocket transport to set reader conn, exec=%+v state=%+v", wsExec, wsState)
	}
	clearCodexManagedWebsocketLocked(wsState)
	wsExec.Unlock()

	providerForce.startRealtimeWSReaderLocked(wsExec, &codexManagedRuntimeState{})

	if !codexShouldDeleteDetachedExecutionSessionLocked(&runtimesession.ExecutionSession{ClientSuppliedID: false, Attached: false, Inflight: false}) {
		t.Fatal("expected detached ephemeral execution session to be deletable")
	}
	if codexShouldDeleteDetachedExecutionSessionLocked(&runtimesession.ExecutionSession{ClientSuppliedID: true}) {
		t.Fatal("expected client-supplied execution session not to be deleted eagerly")
	}
	deleteExec := &runtimesession.ExecutionSession{}
	if !codexMarkDetachedExecutionSessionClosedLocked(deleteExec, "detached") || !deleteExec.IsClosed() {
		t.Fatalf("expected markDetachedExecutionSessionClosedLocked to close eligible session, exec=%+v", deleteExec)
	}
	codexMaybeDeleteDetachedExecutionSession(nil, "ignored")

	if attachment := newCodexAttachmentWithCapacity(0); attachment == nil || len(attachment.queue) != codexRealtimeAttachmentQueueCapacity {
		t.Fatalf("expected zero-capacity attachment to fall back to default queue length, got %+v", attachment)
	}
	var nilAttachment *codexAttachment
	nilAttachment.close()
	if !nilAttachment.isClosed() {
		t.Fatal("expected nil attachment to report closed")
	}

	beginCodexTurnLocked(nil, time.Now())
	resetCodexTurnLocked(nil)
	state := &codexManagedRuntimeState{turnSeq: 1}
	markCodexTurnFirstResponseLocked(state, time.Time{})
	if state.turnStartedAt.IsZero() || state.turnFirstResponseAt.IsZero() {
		t.Fatalf("expected markCodexTurnFirstResponseLocked to seed timestamps, got %+v", state)
	}
	state.turnFinalized = true
	markCodexTurnFirstResponseLocked(state, time.Now().Add(time.Minute))
	if state.turnFirstResponseAt.After(time.Now().Add(30 * time.Second)) {
		t.Fatalf("expected finalized turn not to update first response timestamp, got %+v", state)
	}

	if terminal, responseID, reason := inspectCodexSupplierMessage(wsconn.BinaryMessage, []byte(`{"type":"response.completed"}`)); terminal || responseID != "" || reason != "" {
		t.Fatalf("expected non-text supplier payload to be ignored, terminal=%v response_id=%q reason=%q", terminal, responseID, reason)
	}

	err := codexRealtimeErrorFromOpenAIError("evt_blank_message", &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Code:    "provider_failed",
			Message: "   ",
		},
	})
	event, ok := err.(*types.Event)
	if !ok || event.ErrorDetail == nil || event.ErrorDetail.Message != "provider error" {
		t.Fatalf("expected blank provider message fallback, err=%v", err)
	}
}

func TestCodexRealtimeWSWriteFailureUsesAmbiguousWriteCode(t *testing.T) {
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "channel:1/hash-a/session-write-fail",
		SessionID: "session-write-fail",
		Model:     "gpt-5",
		IdleTTL:   time.Minute,
	})
	conn, cleanup := newCodexRealtimeConnPair(t)
	cleanup()

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	state.wsConn = conn
	err := sendCodexRealtimeWSEventLocked(
		exec,
		state,
		[]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`),
		"evt_write_fail",
		0,
		nil,
	)
	exec.Unlock()

	var event *types.Event
	if !errors.As(err, &event) || event.ErrorDetail == nil {
		t.Fatalf("expected codex provider error event, got %v", err)
	}
	if got := codexRealtimeErrorCodeString(event.ErrorDetail.Code, ""); got != "ws_write_failed" {
		t.Fatalf("expected websocket write failure to use ambiguous write code, got %q", got)
	}
}

func TestCodexRealtimeWSWriteFailureDoesNotReplayOrSwitchTransport(t *testing.T) {
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "channel:1/hash-a/session-write-fail-auto",
		SessionID: "session-write-fail-auto",
		Model:     "gpt-5",
		IdleTTL:   time.Minute,
	})
	conn, cleanup := newCodexRealtimeConnPair(t)
	cleanup()

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	state.wsConn = conn
	err := sendCodexRealtimeWSEventLocked(
		exec,
		state,
		[]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`),
		"evt_write_fail_auto",
		0,
		nil,
	)
	remainingConn := state.wsConn
	exec.Unlock()

	var event *types.Event
	if !errors.As(err, &event) || event.ErrorDetail == nil || codexRealtimeErrorCodeString(event.ErrorDetail.Code, "") != "ws_write_failed" {
		t.Fatalf("expected ambiguous websocket write failure, got %v", err)
	}
	if remainingConn != nil {
		t.Fatal("failed websocket connection was retained")
	}
}

func TestCodexRealtimeWSAmbiguousCreateWriteFinalizesAdmittedTurn(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, nil)
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "channel:1/hash-a/session-write-finalize",
		SessionID: "session-write-finalize",
		Model:     "gpt-5",
		IdleTTL:   time.Minute,
	})
	conn, cleanup := newCodexRealtimeConnPair(t)
	cleanup()

	attachment := newCodexAttachment()
	recorder := &recordingTurnObserver{}
	session := &codexManagedRealtimeSession{
		provider:   provider,
		exec:       exec,
		attachment: attachment,
		ownerSeq:   1,
	}
	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	state.attachment = attachment
	state.ownerSeq = 1
	state.deferWSReader = true
	state.wsConn = conn
	state.turnObserverFactory = func() runtimesession.TurnObserver { return recorder }
	exec.Attached = true
	exec.Unlock()

	err := session.SendClient(context.Background(), codexTestTextFrame([]byte(`{"type":"response.create","event_id":"evt_write_finalize","model":"gpt-5","input":"hi"}`)))
	var event *types.Event
	if !errors.As(err, &event) || event.ErrorDetail == nil || codexRealtimeErrorCodeString(event.ErrorDetail.Code, "") != "ws_write_failed" {
		t.Fatalf("expected ambiguous websocket write failure, got %v", err)
	}
	if got := recorder.finalizeCount(); got != 1 {
		t.Fatalf("ambiguous write must finalize the admitted turn exactly once, got %d", got)
	}
	payload := recorder.lastPayload()
	if payload.TerminationReason != "ws_write_failed" || payload.TurnSeq != 1 {
		t.Fatalf("ambiguous write finalization lost settlement identity, payload=%+v", payload)
	}

	exec.Lock()
	defer exec.Unlock()
	state = getCodexManagedRuntimeStateLocked(exec)
	if exec.Inflight || exec.State != runtimesession.SessionStateIdle || state.turnObserver != nil || state.turnUsage != nil {
		t.Fatalf("ambiguous write must clear active runtime state after finalization, exec=%+v state=%+v", exec, state)
	}
}

func TestCodexResponsesWSCreateWriteDoesNotHoldExecLock(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, nil)
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "channel:1/hash-a/session-write-lock",
		SessionID: "session-write-lock",
		Model:     "gpt-5",
		IdleTTL:   time.Minute,
	})
	conn, cleanup := newCodexRealtimeConnPair(t)
	defer cleanup()

	attachment := newCodexAttachment()
	session := &codexManagedRealtimeSession{
		provider:   provider,
		exec:       exec,
		attachment: attachment,
		ownerSeq:   1,
	}

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	state.attachment = attachment
	state.ownerSeq = 1
	state.deferWSReader = true
	state.wsConn = conn
	exec.Attached = true
	exec.Unlock()

	done := make(chan error, 1)
	go func() {
		done <- session.SendClient(context.Background(), codexTestTextFrame([]byte(`{"type":"response.create","event_id":"evt_write_lock","model":"gpt-5","input":"hi"}`)))
	}()

	deadline := time.After(time.Second)
	for {
		if exec.TryLock() {
			inflight := exec.Inflight
			exec.Unlock()
			if inflight {
				break
			}
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("SendClient returned error while checking exec lock release: %v", err)
			}
			return
		case <-deadline:
			t.Fatal("expected SendClient to release exec lock while waiting for websocket write serialization")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func waitCodexRealtimeTestSignal(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitCodexRealtimeSendDone(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SendClient")
		return nil
	}
}

func TestCodexManagedRealtimeSessionAdmitsTurnBeforeUpstreamWrite(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, nil)
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "channel:1/hash-a/session-admit-before-write",
		SessionID: "session-admit-before-write",
		Model:     "gpt-5",
		IdleTTL:   time.Minute,
	})
	conn, cleanup := newCodexRealtimeConnPair(t)
	defer cleanup()

	observer := &admissionFailingCodexTurnObserver{admitErr: errors.New("quota denied")}
	attachment := newCodexAttachment()
	session := &codexManagedRealtimeSession{
		provider:   provider,
		exec:       exec,
		attachment: attachment,
		ownerSeq:   1,
	}

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	state.attachment = attachment
	state.ownerSeq = 1
	state.deferWSReader = true
	state.wsConn = conn
	state.turnObserverFactory = func() runtimesession.TurnObserver { return observer }
	exec.Attached = true
	exec.Unlock()

	done := make(chan error, 1)
	go func() {
		done <- session.SendClient(context.Background(), codexTestTextFrame([]byte(`{"type":"response.create","event_id":"evt_admit_first","model":"gpt-5","input":"hi"}`)))
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("expected admission failure to return before attempting upstream websocket write")
	}

	if observer.admitCount != 1 {
		t.Fatalf("expected admission observer to be called once, got %d", observer.admitCount)
	}
	if observer.rollbackCount != 0 {
		t.Fatalf("expected failed admission not to need a rollback, got count=%d reason=%q", observer.rollbackCount, observer.rollbackReason)
	}
	var event *types.Event
	if !errors.As(err, &event) || event.ErrorDetail == nil || codexRealtimeErrorCodeString(event.ErrorDetail.Code, "") != "quota_exhausted" {
		t.Fatalf("expected quota_exhausted client payload error, got %v", err)
	}

	exec.Lock()
	if exec.Inflight || exec.State != runtimesession.SessionStateIdle || state.turnObserver != nil {
		exec.Unlock()
		t.Fatalf("expected failed admission to reset turn state, inflight=%v state=%s observer_nil=%v", exec.Inflight, exec.State, state.turnObserver == nil)
	}
	exec.Unlock()
}

func TestCodexWebsocketOnlyDeferredReaderStartsReconnectAfterRecvArmed(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, nil)
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "channel:1/hash-a/session-reconnect-reader",
		SessionID: "session-reconnect-reader",
		Model:     "gpt-5",
		IdleTTL:   time.Minute,
	})
	firstConn, cleanupFirst := newCodexRealtimeConnPair(t)
	defer cleanupFirst()
	secondConn, cleanupSecond := newCodexRealtimeConnPair(t)
	defer cleanupSecond()

	attachment := newCodexAttachment()
	session := &codexManagedRealtimeSession{
		provider:   provider,
		exec:       exec,
		attachment: attachment,
		ownerSeq:   1,
	}

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	state.attachment = attachment
	state.ownerSeq = 1
	state.deferWSReader = true
	state.wsConn = firstConn
	exec.Unlock()

	session.startDeferredRealtimeWSReader()

	exec.Lock()
	if state.deferWSReader {
		exec.Unlock()
		t.Fatal("expected first deferred reader start to clear defer flag")
	}
	if state.wsReaderConn != firstConn {
		exec.Unlock()
		t.Fatalf("expected first websocket reader to be armed, got %#v", state.wsReaderConn)
	}

	state.wsConn = secondConn
	state.wsReaderConn = nil
	state.wsConnGeneration++
	if errWithCode := provider.ensureRealtimeTransportLocked(context.Background(), exec, state); errWithCode != nil {
		exec.Unlock()
		t.Fatalf("expected websocket-only reconnect transport to stay websocket, got %v", errWithCode)
	}
	if state.wsReaderConn != secondConn {
		exec.Unlock()
		t.Fatalf("expected websocket-only reconnect to start a new reader after recv pump was armed, got %#v", state.wsReaderConn)
	}
	exec.Unlock()
}
