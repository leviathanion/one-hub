package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	"one-api/common/requester"
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
	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"` + model + `","input":"hello"}`))
	if err != nil {
		panic(err)
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
	req.FirstFrame = frame
	req.Principal = principal
	req.SelectedModel = model
	return provider.OpenResponsesWS(ctx, &req)
}

func TestLogCodexRealtimeInternalErrorRedactsAndIncludesCaller(t *testing.T) {
	core, observedLogs := observer.New(zapcore.ErrorLevel)
	originalLogger := logger.Logger
	logger.Logger = zap.New(core)
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	logCodexRealtimeInternalError("codex realtime failed Authorization: Bearer abcdefghij.klmnopqrst.uvwxyzabcd upstream-url https://provider.example/v1?token=secret")

	logs := observedLogs.All()
	if len(logs) != 1 {
		t.Fatalf("expected one log entry, got %d", len(logs))
	}
	message := logs[0].Message
	for _, forbidden := range []string{"abcdefghij.klmnopqrst.uvwxyzabcd", "provider.example", "token=secret"} {
		if strings.Contains(message, forbidden) {
			t.Fatalf("expected sensitive value %q to be redacted, got %q", forbidden, message)
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

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, map[string]string{
		"X-Session-Id": "client-supplied-realtime-session",
	})
	provider.Context.Set("token_id", 9201)
	provider.Channel.BaseURL = stringPtr(server.URL)

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

func TestCodexResponsesWSHTTPBridgeTransportIsRejected(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, nil)
	before := ExecutionSessionStats()
	session, errWithCode := openCodexResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{
		Transport: runtimesession.TransportModeResponsesHTTPBridge,
	})
	if session != nil {
		session.Abort("test_cleanup")
	}
	if errWithCode == nil || errWithCode.StatusCode != http.StatusUpgradeRequired || errWithCode.Code != "responses_ws_unsupported_for_channel" {
		t.Fatalf("expected Codex Official http_bridge transport to be rejected, session=%T err=%+v", session, errWithCode)
	}
	after := ExecutionSessionStats()
	if after.LocalSessions != before.LocalSessions || after.LocalBindings != before.LocalBindings {
		t.Fatalf("expected rejected bridge open not to touch execution session manager, before=%+v after=%+v", before, after)
	}
}

func TestCodexResponsesWSHTTPBridgeDoesNotCreateBridgeSession(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force","responses_ws_self_hosted":true}`, nil)
	provider.Channel.BaseURL = stringPtr("http://127.0.0.1:1")
	session, errWithCode := openCodexResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{
		Transport:          runtimesession.TransportModeResponsesHTTPBridge,
		PreviousResponseID: "resp_open_default",
	})
	if session != nil {
		session.Abort("test_cleanup")
	}
	if errWithCode == nil || errWithCode.StatusCode != http.StatusUpgradeRequired || errWithCode.Code != "responses_ws_unsupported_for_channel" {
		t.Fatalf("expected Codex Official to reject bridge before lazy stream creation, session=%T err=%+v", session, errWithCode)
	}
}

func TestCodexResponsesWSNativeUsesIndependentConnectionsForSameClientSession(t *testing.T) {
	var connections atomic.Int32
	server := newCodexRealtimeCountingServer(t, &connections)
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, map[string]string{
		"X-Session-Id": "same-client-session",
	})
	provider.Channel.BaseURL = stringPtr(server.URL)

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

func TestCodexResponsesWSNativeInjectsOpenPreviousResponseIDDefault(t *testing.T) {
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

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, nil)
	provider.Channel.BaseURL = stringPtr(server.URL)

	session, errWithCode := openCodexResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{
		UpstreamSessionID:  "responses-ws-previous-default-test",
		PreviousResponseID: " resp_open_default ",
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
		if string(decoded["previous_response_id"]) != `"resp_open_default"` {
			t.Fatalf("expected open previous_response_id default to be injected, got %s payload=%s", decoded["previous_response_id"], got)
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

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, nil)
	provider.Channel.BaseURL = stringPtr(server.URL)

	session, errWithCode := openCodexResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{
		UpstreamSessionID: "responses-ws-metadata-mismatch-test",
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

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, nil)
	provider.Channel.BaseURL = stringPtr(server.URL)

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

type panicCodexBridgeStream struct {
	closed bool
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

func (s *panicCodexBridgeStream) Recv() (<-chan string, <-chan error) {
	panic("bridge panic")
}

func (s *panicCodexBridgeStream) Close() {
	s.closed = true
}

func TestCodexRealtimeHTTPBridgePumpRecoversPanicAndClosesSession(t *testing.T) {
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	exec := &runtimesession.ExecutionSession{
		SessionID: "codex-panic-session",
		Inflight:  true,
		Attached:  true,
		State:     runtimesession.SessionStateActive,
	}
	stream := &panicCodexBridgeStream{}

	(&CodexProvider{}).pumpRealtimeHTTPBridge(exec, stream)

	if !exec.IsClosed() || exec.CloseReason != "session_aborted" || exec.Inflight {
		t.Fatalf("expected panic recovery to close and clean up session, closed=%v reason=%q inflight=%v", exec.IsClosed(), exec.CloseReason, exec.Inflight)
	}
	if !stream.closed {
		t.Fatal("expected panic recovery to close the bridge stream")
	}
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

	withID := string(buildCodexRealtimeCancelledPayload("resp_cancel"))
	if withID == "" || !json.Valid([]byte(withID)) {
		t.Fatalf("expected cancelled payload with response id to be valid json, got %q", withID)
	}
	withoutID := string(buildCodexRealtimeCancelledPayload(""))
	if withoutID == "" || !json.Valid([]byte(withoutID)) {
		t.Fatalf("expected cancelled payload without response id to be valid json, got %q", withoutID)
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

	if got := codexTurnTerminationReason("", &types.OpenAIResponsesResponses{Status: "completed"}); got != "response.completed" {
		t.Fatalf("expected response status to drive termination reason, got %q", got)
	}
	if got := codexTurnTerminationReason("response.failed", nil); got != "response.failed" {
		t.Fatalf("expected event type fallback termination reason, got %q", got)
	}
	if got := bridgeTerminationReason(nil, false); got != "bridge_stream_closed" {
		t.Fatalf("expected clean bridge close reason, got %q", got)
	}
	if got := bridgeTerminationReason(errors.New("boom"), false); got != "bridge_stream_failed" {
		t.Fatalf("expected failed bridge reason, got %q", got)
	}
	if got := bridgeTerminationReason(errors.New("boom"), true); got != "bridge_stream_truncated" {
		t.Fatalf("expected truncated bridge reason, got %q", got)
	}

	if terminal, responseID, reason := inspectCodexRealtimeSupplierEvent(wsconn.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_supplier","status":"completed"}}`)); !terminal || responseID != "resp_supplier" || reason != "response.completed" {
		t.Fatalf("expected supplier terminal event inspection, terminal=%v response_id=%q reason=%q", terminal, responseID, reason)
	}
	if terminal, responseID, reason := inspectCodexRealtimeSupplierEvent(wsconn.TextMessage, []byte("bad-json")); terminal || responseID != "" || reason != "" {
		t.Fatalf("expected invalid supplier payload inspection fallback, terminal=%v response_id=%q reason=%q", terminal, responseID, reason)
	}
	if terminal, responseID, reason := inspectCodexRealtimeEvent(nil); terminal || responseID != "" || reason != "" {
		t.Fatalf("expected nil realtime event inspection fallback, terminal=%v response_id=%q reason=%q", terminal, responseID, reason)
	}
	if terminal, responseID, reason := inspectCodexRealtimeEvent(&types.OpenAIResponsesStreamResponses{Type: "error", Response: &types.OpenAIResponsesResponses{ID: "resp_error"}}); !terminal || responseID != "resp_error" || reason != "error" {
		t.Fatalf("expected error realtime event inspection, terminal=%v response_id=%q reason=%q", terminal, responseID, reason)
	}

	accumulator := newCodexTurnUsageAccumulator()
	usage, terminal, responseID, reason := inspectCodexBridgePayload(`{"type":"response.done","response":{"id":"resp_bridge","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`, accumulator, "gpt-5")
	if !terminal || responseID != "resp_bridge" || reason != "response.completed" || usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("expected bridge payload inspection to surface terminal usage, usage=%+v terminal=%v response_id=%q reason=%q", usage, terminal, responseID, reason)
	}
	if usage, terminal, responseID, reason := inspectCodexBridgePayload("bad-json", accumulator, "gpt-5"); usage != nil || terminal || responseID != "" || reason != "" {
		t.Fatalf("expected invalid bridge payload inspection fallback, usage=%+v terminal=%v response_id=%q reason=%q", usage, terminal, responseID, reason)
	}

	request := &types.OpenAIResponsesRequest{
		Metadata: map[string]string{"trace_id": "trace-123"},
		Tools:    []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview}},
		Include:  []string{"output_text.annotations"},
		ToolChoice: map[string]any{
			"type": "web_search_preview_2025_03_11",
		},
	}
	clonedRequest, err := cloneCodexResponsesRequest(request)
	if err != nil {
		t.Fatalf("expected request clone to succeed, got %v", err)
	}
	clonedRequest.Metadata["trace_id"] = "changed"
	clonedRequest.Tools[0].Type = "modified"
	if request.Metadata["trace_id"] != "trace-123" || request.Tools[0].Type != types.APIToolTypeWebSearchPreview {
		t.Fatalf("expected cloned request to be detached from source, request=%+v", request)
	}
	if _, err := cloneCodexResponsesRequest(nil); err == nil {
		t.Fatal("expected nil request clone to fail")
	}

	mutable := cloneCodexMutableValue(map[string]any{
		"items": []any{
			map[string]any{"tool": "web_search_preview_2025_03_11"},
		},
	})
	mutableMap, _ := mutable.(map[string]any)
	mutableItems, _ := mutableMap["items"].([]any)
	itemMap, _ := mutableItems[0].(map[string]any)
	itemMap["tool"] = "changed"
	originalItems, _ := request.ToolChoice.(map[string]any)
	if originalItems["type"] != "web_search_preview_2025_03_11" {
		t.Fatalf("expected cloneCodexMutableValue to deep-copy nested maps, got %#v", request.ToolChoice)
	}
	if raw := cloneCodexMutableValue(json.RawMessage(`{"ok":true}`)); string(raw.(json.RawMessage)) != `{"ok":true}` {
		t.Fatalf("expected json.RawMessage clone, got %#v", raw)
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
	exec.Transport = runtimesession.TransportModeRealtimeWS
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
		t.Fatalf("expected provider close origin, got %q", outbound.origin)
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
	eventID, request, encoded, err := provider.prepareCodexRealtimeCreatePayload([]byte(`{"type":"response.create","event_id":"evt_raw","model":"gpt-5","input":"hi","temperature":0.7,"top_p":0.9,"context_management":{"mode":"unsupported"},"truncation":"auto","unknown_number":12345678901234567890,"future_object":{"enabled":true}}`), "gpt-5")
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
	if _, ok := response["context_management"]; ok {
		t.Fatalf("expected Codex adapter to remove unsupported context_management, got %s", response["context_management"])
	}
	if _, ok := response["truncation"]; ok {
		t.Fatalf("expected Codex adapter to remove unsupported truncation, got %s", response["truncation"])
	}
	if _, ok := response["top_p"]; ok {
		t.Fatalf("expected Codex adapter to remove top_p when temperature is set, got %s", response["top_p"])
	}
	if string(response["store"]) != `false` {
		t.Fatalf("expected Codex adapter to force store=false, got %s", response["store"])
	}
	if !strings.Contains(string(response["include"]), codexRealtimeBridgeReasoningEncryptedContentInclude) {
		t.Fatalf("expected Codex reasoning include to be present, got %s", response["include"])
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
	exec.Transport = runtimesession.TransportModeRealtimeWS
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

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, nil)
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
	exec.Transport = runtimesession.TransportModeRealtimeWS
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
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, nil)
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
	if channelHeaders["x-trace"] != "trace" || channelHeaders["originator"] != defaultOfficialCodexOriginator {
		t.Fatalf("expected filtered compatibility headers to preserve x-trace/originator, got %+v", channelHeaders)
	}

	signature := provider.buildRealtimeHandshakePolicySignature()
	if !strings.Contains(signature, "channel-ua") || !strings.Contains(signature, defaultOfficialCodexOriginator) {
		t.Fatalf("expected handshake signature to use channel user agent and preserve explicit channel originator, got %q", signature)
	}
	if got := provider.buildRealtimeCompatibilityHash("gpt-5", provider.readRealtimeUpstreamIdentity()); got == "" {
		t.Fatal("expected compatibility hash to be populated")
	}
	if got := provider.readRealtimeUpstreamIdentity(); !strings.Contains(got, "credential:account:acct-123") {
		t.Fatalf("expected upstream identity to include credential identity, got %q", got)
	}
}

func TestCodexRealtimeCompatibilityHashSeparatesSmartOriginatorFallbacks(t *testing.T) {
	key := `{"access_token":"access-token","account_id":"acct-123"}`
	officialProvider := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"User-Agent": "codex_cli_rs/0.116.0",
	})
	nonOfficialProvider := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"User-Agent": "curl/8.0",
	})

	officialSignature := officialProvider.buildRealtimeHandshakePolicySignature()
	if !strings.Contains(officialSignature, "codex_cli_rs/0.116.0") || !strings.Contains(officialSignature, `"originator":"codex-tui"`) {
		t.Fatalf("expected official user agent and synthesized codex-tui originator to remain in signature, got %q", officialSignature)
	}

	nonOfficialSignature := nonOfficialProvider.buildRealtimeHandshakePolicySignature()
	if !strings.Contains(nonOfficialSignature, `"originator":"pi"`) {
		t.Fatalf("expected non-official pi originator to remain in handshake signature, got %q", nonOfficialSignature)
	}

	upstreamIdentity := officialProvider.readRealtimeUpstreamIdentity()
	if upstreamIdentity != nonOfficialProvider.readRealtimeUpstreamIdentity() {
		t.Fatalf("test setup expected matching upstream identity")
	}
	officialHash := officialProvider.buildRealtimeCompatibilityHash("gpt-5", upstreamIdentity)
	nonOfficialHash := nonOfficialProvider.buildRealtimeCompatibilityHash("gpt-5", upstreamIdentity)
	if officialHash == "" || nonOfficialHash == "" || officialHash == nonOfficialHash {
		t.Fatalf("expected official and non-official smart originators to produce different hashes, official=%q non_official=%q", officialHash, nonOfficialHash)
	}
}

func TestCodexRealtimeCompatibilityHashSmartOriginatorUsesEffectiveChannelUserAgent(t *testing.T) {
	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"User-Agent": "Mozilla/5.0",
	})
	provider.Channel.ModelHeaders = stringPtr(`{"User-Agent":"codex-tui/1.0"}`)

	signature := provider.buildRealtimeHandshakePolicySignature()
	if !strings.Contains(signature, "codex-tui/1.0") || !strings.Contains(signature, `"originator":"codex-tui"`) {
		t.Fatalf("expected smart originator to follow effective channel user agent in signature, got %q", signature)
	}
}

func TestCodexRealtimeCompatibilityHashIncludesDefaultOriginator(t *testing.T) {
	key := `{"access_token":"access-token","account_id":"acct-123"}`
	implicitProvider := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"User-Agent": defaultUserAgent,
	})
	explicitProvider := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"User-Agent": defaultUserAgent,
		"Originator": defaultOfficialCodexOriginator,
	})

	implicitSignature := implicitProvider.buildRealtimeHandshakePolicySignature()
	if !strings.Contains(implicitSignature, `"originator":"codex-tui"`) {
		t.Fatalf("expected synthesized default originator to remain in signature, got %q", implicitSignature)
	}

	explicitSignature := explicitProvider.buildRealtimeHandshakePolicySignature()
	if !strings.Contains(explicitSignature, `"originator":"codex-tui"`) {
		t.Fatalf("expected explicit default originator to remain in signature, got %q", explicitSignature)
	}

	upstreamIdentity := implicitProvider.readRealtimeUpstreamIdentity()
	if upstreamIdentity != explicitProvider.readRealtimeUpstreamIdentity() {
		t.Fatalf("test setup expected matching upstream identity")
	}
	implicitHash := implicitProvider.buildRealtimeCompatibilityHash("gpt-5", upstreamIdentity)
	explicitHash := explicitProvider.buildRealtimeCompatibilityHash("gpt-5", upstreamIdentity)
	if implicitHash == "" || explicitHash == "" || implicitHash != explicitHash {
		t.Fatalf("expected identical default originators to produce same compatibility hash, implicit=%q explicit=%q", implicitHash, explicitHash)
	}
}

func TestCodexRealtimeCompatibilityHashSeparatesDifferentOfficialUserAgents(t *testing.T) {
	key := `{"access_token":"access-token","account_id":"acct-123"}`
	providerA := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"User-Agent": "codex-tui/1.0",
	})
	providerB := newTestCodexProviderWithContext(t, key, "", map[string]string{
		"User-Agent": "CodexCanary/1.0",
	})

	signatureA := providerA.buildRealtimeHandshakePolicySignature()
	if !strings.Contains(signatureA, "codex-tui/1.0") || !strings.Contains(signatureA, `"originator":"codex-tui"`) {
		t.Fatalf("expected official user agent and synthesized codex-tui originator to remain in signature, got %q", signatureA)
	}
	signatureB := providerB.buildRealtimeHandshakePolicySignature()
	if !strings.Contains(signatureB, "CodexCanary/1.0") || !strings.Contains(signatureB, `"originator":"codex-tui"`) {
		t.Fatalf("expected official user agent and synthesized codex-tui originator to remain in signature, got %q", signatureB)
	}

	upstreamIdentity := providerA.readRealtimeUpstreamIdentity()
	if upstreamIdentity != providerB.readRealtimeUpstreamIdentity() {
		t.Fatalf("test setup expected matching upstream identity")
	}
	hashA := providerA.buildRealtimeCompatibilityHash("gpt-5", upstreamIdentity)
	hashB := providerB.buildRealtimeCompatibilityHash("gpt-5", upstreamIdentity)
	if hashA == "" || hashB == "" || hashA == hashB {
		t.Fatalf("expected different official user agents to produce different hashes, a=%q b=%q", hashA, hashB)
	}
}

func TestCodexRealtimeTransportAndDetachedSessionHelpers(t *testing.T) {
	providerOff := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, nil)
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "channel:1/hash-a/session-transport",
		SessionID: "session-transport",
		Model:     "gpt-5",
		IdleTTL:   time.Minute,
	})
	conn, cleanupConn := newCodexRealtimeConnPair(t)
	defer cleanupConn()

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	state.wsConn = conn
	exec.Inflight = true
	exec.State = runtimesession.SessionStateActive
	if errWithCode := providerOff.ensureRealtimeTransportLocked(exec, state, time.Now()); errWithCode != nil {
		exec.Unlock()
		t.Fatalf("expected websocket-off transport path to succeed, got %v", errWithCode)
	}
	if exec.Transport != runtimesession.TransportModeResponsesHTTPBridge || exec.Inflight || exec.State != runtimesession.SessionStateIdle || state.wsConn != nil {
		exec.Unlock()
		t.Fatalf("expected websocket-off transport path to clear ws and fall back to bridge, exec=%+v state=%+v", exec, state)
	}
	exec.Unlock()

	providerAuto := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"auto"}`, nil)
	bridgeExec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "channel:1/hash-a/session-bridge",
		SessionID: "session-bridge",
		Model:     "gpt-5",
		IdleTTL:   time.Minute,
	})
	bridgeExec.Transport = runtimesession.TransportModeResponsesHTTPBridge
	bridgeExec.FallbackUntil = time.Now().Add(time.Minute)
	bridgeExec.Lock()
	bridgeState := getCodexManagedRuntimeStateLocked(bridgeExec)
	if errWithCode := providerAuto.ensureRealtimeTransportLocked(bridgeExec, bridgeState, time.Now()); errWithCode != nil {
		bridgeExec.Unlock()
		t.Fatalf("expected bridge cooldown path to keep bridge transport, got %v", errWithCode)
	}
	if bridgeExec.Transport != runtimesession.TransportModeResponsesHTTPBridge {
		bridgeExec.Unlock()
		t.Fatalf("expected bridge cooldown path to preserve bridge transport, got %q", bridgeExec.Transport)
	}
	bridgeState.bridgeStream = &fakeStringStream{dataChan: make(chan string), errChan: make(chan error, 1)}
	if errWithCode := providerAuto.ensureRealtimeTransportLocked(bridgeExec, bridgeState, time.Now()); errWithCode != nil || bridgeExec.Transport != runtimesession.TransportModeResponsesHTTPBridge {
		bridgeExec.Unlock()
		t.Fatalf("expected existing bridge stream to preserve bridge transport, exec=%+v err=%v", bridgeExec, errWithCode)
	}
	bridgeState.requireWS = true
	if errWithCode := providerAuto.ensureRealtimeTransportLocked(bridgeExec, bridgeState, time.Now()); errWithCode == nil || codexRealtimeErrorCodeString(errWithCode.Code, "") != "responses_ws_unsupported_for_channel" {
		bridgeExec.Unlock()
		t.Fatalf("expected websocket-only state to reject existing HTTP bridge transport, got %v", errWithCode)
	}
	bridgeExec.Unlock()

	providerForce := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, nil)
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
	if errWithCode := providerForce.ensureRealtimeTransportLocked(wsExec, wsState, time.Now()); errWithCode != nil {
		wsExec.Unlock()
		t.Fatalf("expected existing websocket transport path to succeed, got %v", errWithCode)
	}
	if wsExec.Transport != runtimesession.TransportModeRealtimeWS || wsState.wsReaderConn != wsConn {
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
	state = &codexManagedRuntimeState{turnSeq: 1}
	markCodexTurnFirstResponseLocked(state, time.Time{})
	if state.turnStartedAt.IsZero() || state.turnFirstResponseAt.IsZero() {
		t.Fatalf("expected markCodexTurnFirstResponseLocked to seed timestamps, got %+v", state)
	}
	state.turnFinalized = true
	markCodexTurnFirstResponseLocked(state, time.Now().Add(time.Minute))
	if state.turnFirstResponseAt.After(time.Now().Add(30 * time.Second)) {
		t.Fatalf("expected finalized turn not to update first response timestamp, got %+v", state)
	}

	if got := bridgeTerminationReason(io.EOF, false); got != "bridge_stream_closed" {
		t.Fatalf("expected EOF bridge termination to be treated as clean close, got %q", got)
	}
	if terminal, responseID, reason := inspectCodexRealtimeSupplierEvent(wsconn.BinaryMessage, []byte(`{"type":"response.completed"}`)); terminal || responseID != "" || reason != "" {
		t.Fatalf("expected non-text supplier payload to be ignored, terminal=%v response_id=%q reason=%q", terminal, responseID, reason)
	}

	clonedStrings := cloneCodexMutableValue([]string{"one", "two"}).([]string)
	clonedStrings[0] = "changed"
	if clonedStrings[0] != "changed" {
		t.Fatal("expected []string mutable clone to be writable")
	}
	clonedMap := cloneCodexMutableValue(map[string]string{"trace": "one"}).(map[string]string)
	clonedMap["trace"] = "two"
	if clonedMap["trace"] != "two" {
		t.Fatal("expected map[string]string mutable clone to be writable")
	}
	clonedMaps := cloneCodexMutableValue([]map[string]any{{"trace": "one"}}).([]map[string]any)
	clonedMaps[0]["trace"] = "two"
	if clonedMaps[0]["trace"] != "two" {
		t.Fatal("expected []map mutable clone to be writable")
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

func TestCodexResponsesWSUnsupportedOpenDoesNotMutateRealtimeSession(t *testing.T) {
	manager := runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{
		DefaultTTL: time.Minute,
		Cleanup:    cleanupCodexExecutionSession,
	})
	replaceCodexExecutionSessionsForTest(t, manager)

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{
		"X-Session-Id": "require-ws-rollback-session",
	})
	provider.Context.Set("token_id", 501)

	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected baseline HTTP-bridge session to open, got %v", errWithCode)
	}
	managed, ok := session.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session, got %T", session)
	}
	defer managed.Detach("test_cleanup")

	managed.exec.Lock()
	state := getCodexManagedRuntimeStateLocked(managed.exec)
	staleAttachment := state.attachment
	staleOwnerSeq := state.ownerSeq
	if state.requireWS || state.deferWSReader {
		managed.exec.Unlock()
		t.Fatalf("expected baseline HTTP-bridge session to start without websocket-only flags, state=%+v", state)
	}
	managed.exec.Unlock()

	failedUpstream, errWithCode := openCodexResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{
		UpstreamSessionID: "responses-ws-off-test",
	})
	if failedUpstream != nil {
		failedUpstream.Abort("unexpected_open")
	}
	if errWithCode == nil || codexRealtimeErrorCodeString(errWithCode.Code, "") != "responses_ws_unsupported_for_channel" {
		t.Fatalf("expected ResponsesWS open to fail as unsupported, got upstream=%#v err=%+v", failedUpstream, errWithCode)
	}

	managed.exec.Lock()
	defer managed.exec.Unlock()
	state = getCodexManagedRuntimeStateLocked(managed.exec)
	if state.attachment != staleAttachment || state.ownerSeq != staleOwnerSeq {
		t.Fatalf("expected failed ResponsesWS open not to mutate realtime attachment ownership, state=%+v stale_owner=%d", state, staleOwnerSeq)
	}
	if state.requireWS || state.deferWSReader {
		t.Fatalf("expected failed ResponsesWS open not to set realtime transport flags, require_ws=%v defer_reader=%v", state.requireWS, state.deferWSReader)
	}
	if !managed.exec.Attached {
		t.Fatal("expected original realtime session attachment to remain attached after failed ResponsesWS open")
	}
}

func TestCodexRealtimeWSWriteFailureUsesAmbiguousWriteCode(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, nil)
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
	state.requireWS = true
	state.wsConn = conn
	exec.Transport = runtimesession.TransportModeRealtimeWS
	err := provider.sendRealtimeWSEventLocked(
		context.Background(),
		exec,
		state,
		[]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`),
		"evt_write_fail",
		&types.OpenAIResponsesRequest{Model: "gpt-5", Input: "hi"},
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

func TestCodexResponsesWSCreateWriteDoesNotHoldExecLock(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, nil)
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
	state.requireWS = true
	state.deferWSReader = true
	state.wsConn = conn
	exec.Transport = runtimesession.TransportModeRealtimeWS
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

func TestCodexManagedHTTPBridgeOpenUsesSendContextAndDoesNotHoldExecLock(t *testing.T) {
	requestStarted := make(chan struct{}, 1)
	releaseHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requestStarted <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-releaseHandler:
		}
	}))
	defer server.Close()
	defer close(releaseHandler)
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	defer func() {
		requester.HTTPClient = originalHTTPClient
	}()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, nil)
	provider.Channel.BaseURL = stringPtr(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("open managed session: %v", errWithCode)
	}
	defer session.Detach("test_close")
	defer cleanupCodexManagedSession(t, provider, "gpt-5")

	managed := session.(*codexManagedRealtimeSession)
	observer := &admissionFailingCodexTurnObserver{}
	managed.exec.Lock()
	state := getCodexManagedRuntimeStateLocked(managed.exec)
	state.turnObserverFactory = func() runtimesession.TurnObserver { return observer }
	managed.exec.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- session.SendClient(ctx, codexTestTextFrame([]byte(`{"type":"response.create","event_id":"evt_bridge_open_ctx","model":"gpt-5","input":"hi"}`)))
	}()

	waitCodexRealtimeTestSignal(t, requestStarted, "bridge request start")
	deadline := time.After(time.Second)
	for {
		if managed.exec.TryLock() {
			inflight := managed.exec.Inflight
			managed.exec.Unlock()
			if inflight {
				break
			}
		}
		select {
		case err := <-done:
			t.Fatalf("SendClient returned before lock probe, err=%v", err)
		case <-deadline:
			t.Fatal("expected HTTP bridge open to release exec lock while upstream request is pending")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	cancel()
	if err := waitCodexRealtimeSendDone(t, done); err == nil {
		t.Fatal("expected cancelled bridge open to return an error")
	}

	managed.exec.Lock()
	state = getCodexManagedRuntimeStateLocked(managed.exec)
	if state.bridgeOpeningCancel != nil || state.bridgeStream != nil || managed.exec.Inflight || managed.exec.State != runtimesession.SessionStateIdle {
		t.Fatalf("expected cancelled bridge open to leave idle clean state, inflight=%v state=%v opening=%v stream=%v", managed.exec.Inflight, managed.exec.State, state.bridgeOpeningCancel != nil, state.bridgeStream != nil)
	}
	managed.exec.Unlock()
	if observer.admitCount != 1 || observer.rollbackCount != 1 || observer.rollbackReason != "bridge_local_failure" {
		t.Fatalf("expected cancelled bridge open to rollback admitted turn, admit=%d rollback=%d reason=%q", observer.admitCount, observer.rollbackCount, observer.rollbackReason)
	}
}

func TestCodexManagedHTTPBridgeUsesResponsesURLPolicy(t *testing.T) {
	t.Run("local http requires explicit responses self hosted", func(t *testing.T) {
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, nil)
		provider.Context.Set("responses_ws_self_hosted", false)
		provider.Channel.BaseURL = stringPtr("http://127.0.0.1:1")

		stream, errWithCode := provider.createResponsesStreamWithSession(context.Background(), &types.OpenAIResponsesRequest{Model: "gpt-5"}, "session-managed-policy")
		if stream != nil {
			stream.Close()
		}
		if errWithCode == nil || errWithCode.Code != "ws_request_failed" || errWithCode.StatusCode != http.StatusBadRequest || !strings.Contains(errWithCode.Message, requester.ErrUpstreamResponsesHTTPURLRequiresHTTPS.Error()) {
			t.Fatalf("expected managed bridge local http URL policy rejection, stream=%T err=%+v", stream, errWithCode)
		}
	})

	t.Run("metadata stays blocked even when responses self hosted", func(t *testing.T) {
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, nil)
		provider.Channel.BaseURL = stringPtr("http://169.254.169.254")

		stream, errWithCode := provider.createResponsesStreamWithSession(context.Background(), &types.OpenAIResponsesRequest{Model: "gpt-5"}, "session-managed-policy")
		if stream != nil {
			stream.Close()
		}
		if errWithCode == nil || errWithCode.Code != "ws_request_failed" || errWithCode.StatusCode != http.StatusBadRequest || !strings.Contains(errWithCode.Message, requester.ErrUpstreamResponsesHTTPURLHostBlocked.Error()) {
			t.Fatalf("expected managed bridge metadata URL policy rejection, stream=%T err=%+v", stream, errWithCode)
		}
	})
}

func TestCodexManagedHTTPBridgeCancelDuringOpenCancelsRequestAndDoesNotAdoptStream(t *testing.T) {
	requestStarted := make(chan struct{}, 1)
	releaseHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requestStarted <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-releaseHandler:
		}
	}))
	defer server.Close()
	defer close(releaseHandler)
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	defer func() {
		requester.HTTPClient = originalHTTPClient
	}()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, nil)
	provider.Channel.BaseURL = stringPtr(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("open managed session: %v", errWithCode)
	}
	defer session.Detach("test_close")
	defer cleanupCodexManagedSession(t, provider, "gpt-5")

	done := make(chan error, 1)
	go func() {
		done <- session.SendClient(context.Background(), codexTestTextFrame([]byte(`{"type":"response.create","event_id":"evt_bridge_open_cancel","model":"gpt-5","input":"hi"}`)))
	}()

	waitCodexRealtimeTestSignal(t, requestStarted, "bridge request start")
	if err := session.SendClient(context.Background(), codexTestTextFrame([]byte(`{"type":"response.cancel","event_id":"evt_cancel_open"}`))); err != nil {
		t.Fatalf("cancel opening bridge: %v", err)
	}
	if err := waitCodexRealtimeSendDone(t, done); err != nil {
		t.Fatalf("expected cancelled opening create to return cleanly, got %v", err)
	}

	managed := session.(*codexManagedRealtimeSession)
	managed.exec.Lock()
	state := getCodexManagedRuntimeStateLocked(managed.exec)
	if state.bridgeOpeningCancel != nil || state.bridgeStream != nil || managed.exec.Inflight || managed.exec.State != runtimesession.SessionStateIdle {
		t.Fatalf("expected response.cancel during bridge open to leave idle clean state, inflight=%v state=%v opening=%v stream=%v", managed.exec.Inflight, managed.exec.State, state.bridgeOpeningCancel != nil, state.bridgeStream != nil)
	}
	managed.exec.Unlock()
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
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, nil)
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
	state.requireWS = true
	state.deferWSReader = true
	state.wsConn = conn
	state.turnObserverFactory = func() runtimesession.TurnObserver { return observer }
	exec.Transport = runtimesession.TransportModeRealtimeWS
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
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, nil)
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
	state.requireWS = true
	state.deferWSReader = true
	state.wsConn = firstConn
	exec.Transport = runtimesession.TransportModeRealtimeWS
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
	if errWithCode := provider.ensureRealtimeTransportLocked(exec, state, time.Now()); errWithCode != nil {
		exec.Unlock()
		t.Fatalf("expected websocket-only reconnect transport to stay websocket, got %v", errWithCode)
	}
	if state.wsReaderConn != secondConn {
		exec.Unlock()
		t.Fatalf("expected websocket-only reconnect to start a new reader after recv pump was armed, got %#v", state.wsReaderConn)
	}
	exec.Unlock()
}
