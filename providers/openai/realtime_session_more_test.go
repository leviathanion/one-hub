package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
	"one-api/model"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

func newOpenAIRealtimeHelperSession() *openAIRealtimeSession {
	return &openAIRealtimeSession{
		model:     "gpt-4o-realtime-preview",
		sessionID: "sess_helper",
		recvCh:    make(chan openAIRealtimeOutbound, 8),
		closed:    make(chan struct{}),
		detached:  make(chan struct{}),
	}
}

func TestOpenAIResponsesWSAdapterMapsProviderCloseOnlyForPeerClose(t *testing.T) {
	adapter := openAIResponsesWSAdapter{}
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

func TestOpenAIResponsesWSAdapterRejectsDuplicateKeyClientCancel(t *testing.T) {
	adapter := openAIResponsesWSAdapter{}
	_, err := adapter.PrepareClientFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.create","model":"gpt-5","type":"response.cancel"}`)))
	if !errors.Is(err, responsesws.ErrInvalidClientEventPayload) {
		t.Fatalf("expected duplicate-key client event to be rejected, got %v", err)
	}
}

func openAIRealtimeTestWriteTimeout() func() time.Duration {
	timeout := config.RealtimeWebsocketWriteTimeout()
	return func() time.Duration { return timeout }
}

func assertNoOpenAIRealtimeOutbound(t *testing.T, ch <-chan openAIRealtimeOutbound, wait time.Duration) {
	t.Helper()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case outbound := <-ch:
		t.Fatalf("expected no queued outbound payload, got %q", outbound.payload)
	case <-timer.C:
	}
}

func openAIResponsesWSTestRecv(ctx context.Context, upstream responsesws.Upstream) (wsconn.MessageType, []byte, *types.UsageEvent, responsesws.PayloadOrigin, error) {
	event, err := upstream.Recv(ctx)
	if err != nil {
		return 0, nil, nil, responsesws.PayloadOriginProxyLocal, err
	}
	messageType := wsconn.TextMessage
	var payload []byte
	if event.Frame != nil {
		if event.Frame.Kind() == responsesws.FrameKindBinary {
			messageType = wsconn.BinaryMessage
		}
		payload = event.Frame.Payload()
	}
	return messageType, payload, event.Usage, responsesws.PayloadOriginForDetailOrigin(event.DetailOrigin), event.Err
}

func newOpenAIRealtimeConnPair(t *testing.T) (*wsconn.ManagedConn, func()) {
	t.Helper()

	wsURL, cleanupServer := wstest.Server(t, func(conn *wsconn.ManagedConn) {
		<-conn.Done()
	})
	conn := dialOpenAIRealtimeManagedTestConn(t, wsURL, wsconn.Config{
		Label:        "openai realtime test upstream",
		ReadLimit:    config.RealtimeWebsocketReadLimit(),
		WriteTimeout: openAIRealtimeTestWriteTimeout(),
	})

	return conn, func() {
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
		cleanupServer()
	}
}

func dialOpenAIRealtimeManagedTestConn(t *testing.T, wsURL string, cfg wsconn.Config) *wsconn.ManagedConn {
	t.Helper()
	conn, err := wsconn.DialManaged(context.Background(), wsURL, nil, cfg, wsconn.WithDialSecurityPolicy(wsconn.DialSecurityPolicy{
		AllowInsecureWS: true,
		AllowPrivateIP:  true,
	}))
	if err != nil {
		t.Fatalf("failed to dial helper websocket: %v", err)
	}
	return conn
}

func newOpenAIRealtimeHeaderCaptureServer(t *testing.T, headerCh chan<- http.Header, urlCh chan<- string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if headerCh != nil {
			headerCh <- r.Header.Clone()
		}
		if urlCh != nil {
			urlCh <- r.URL.String()
		}
		conn, err := wsconn.AcceptManaged(w, r, wsconn.Config{Label: "openai realtime test accept"}, wsconn.AcceptOptions{
			CheckOrigin: func(*http.Request) bool { return true },
		})
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		<-conn.Done()
	}))
}

func TestOpenAIRealtimeSessionSendClientRejectsZeroFrame(t *testing.T) {
	conn, cleanup := newOpenAIRealtimeConnPair(t)
	defer cleanup()

	session := newOpenAIRealtimeHelperSession()
	session.conn = conn

	if err := session.SendClient(context.Background(), runtimerealtime.Frame{}); !errors.Is(err, runtimerealtime.ErrInvalidFrame) {
		t.Fatalf("expected zero frame to return ErrInvalidFrame, got %v", err)
	}
}

func TestOpenAIRealtimeSessionSendClientRejectsUnknownFrameKind(t *testing.T) {
	conn, cleanup := newOpenAIRealtimeConnPair(t)
	defer cleanup()

	session := newOpenAIRealtimeHelperSession()
	session.conn = conn

	if err := session.SendClient(context.Background(), openAITestUnknownKindFrame([]byte("{}"))); !errors.Is(err, runtimerealtime.ErrInvalidFrame) {
		t.Fatalf("expected unknown frame kind to return ErrInvalidFrame, got %v", err)
	}
}

func openAITestUnknownKindFrame(payload []byte) runtimerealtime.Frame {
	frame := runtimerealtime.NewTextFrame(payload)
	field := reflect.ValueOf(&frame).Elem().FieldByName("kind")
	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().SetInt(99)
	return frame
}

func TestOpenAIRealtimeReadLoopForwardsProviderCloseCode(t *testing.T) {
	closeSent := make(chan struct{})
	wsURL, cleanupServer := wstest.Server(t, func(conn *wsconn.ManagedConn) {
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindGracefulShutdown, Code: wsconn.CloseCode(4408), Reason: "quota exhausted"})
		close(closeSent)
		<-conn.Done()
	})
	defer cleanupServer()

	conn := dialOpenAIRealtimeManagedTestConn(t, wsURL, wsconn.Config{
		Label:        "openai realtime close test upstream",
		ReadLimit:    config.RealtimeWebsocketReadLimit(),
		WriteTimeout: openAIRealtimeTestWriteTimeout(),
	})
	defer conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})

	session := newOpenAIRealtimeHelperSession()
	session.conn = conn
	session.startReadLoop()

	select {
	case <-closeSent:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for test server to send close frame")
	}

	event, err := session.Recv(context.Background())
	if err != nil {
		t.Fatalf("Recv err=%v, want provider close event", err)
	}
	if event.ProviderClose == nil {
		t.Fatalf("expected provider close event, got %+v", event)
	}
	if event.ProviderClose.Code != 4408 {
		t.Fatalf("expected provider close code 4408, got %d", event.ProviderClose.Code)
	}
	if event.ProviderClose.Reason != "quota exhausted" {
		t.Fatalf("expected provider close reason to be preserved, got %q", event.ProviderClose.Reason)
	}
	if !errors.Is(event.ProviderClose.Err, runtimerealtime.ErrSessionClosed) {
		t.Fatalf("expected provider close to carry session closed error, got %v", event.ProviderClose.Err)
	}
	if event.Frame != nil || event.Usage != nil || event.Err != nil || event.Origin != runtimerealtime.RealtimePayloadOriginProvider {
		t.Fatalf("expected only provider close with provider origin, got %+v", event)
	}
}

func TestOpenAIRealtimeSessionHelperNormalizationAndIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	ctx.Request.Header.Set("X-Session-Id", "client-session")
	provider := &OpenAIProvider{}
	provider.Context = ctx

	if got, errWithCode := readOpenAIRealtimeSessionID(provider); errWithCode != nil || got != "client-session" {
		t.Fatalf("expected request session id, got %q err=%v", got, errWithCode)
	}
	if got, errWithCode := readOpenAIRealtimeSessionID(&OpenAIProvider{}); errWithCode != nil || got == "" {
		t.Fatalf("expected helper to generate fallback realtime session id, got %q err=%v", got, errWithCode)
	}
	ctx.Request.Header.Set("X-Session-Id", strings.Repeat("x", runtimesession.ClientSessionIDMaxLen+1))
	if _, errWithCode := readOpenAIRealtimeSessionID(provider); errWithCode == nil || errWithCode.Code != "invalid_session_id" {
		t.Fatalf("expected invalid session id to be rejected, got %v", errWithCode)
	}

	binaryPayload := []byte{1, 2, 3}
	if normalized, eventType, err := normalizeOpenAIRealtimeClientPayload(binaryPayload, wsconn.BinaryMessage, "gpt-4o", false); err != nil || eventType != "" || string(normalized) != string(binaryPayload) {
		t.Fatalf("expected non-text payload passthrough, normalized=%v event=%q err=%v", normalized, eventType, err)
	}

	if normalized, eventType, err := normalizeOpenAIRealtimeClientPayload([]byte("not-json"), wsconn.TextMessage, "gpt-4o", false); err != nil || eventType != "" || string(normalized) != "not-json" {
		t.Fatalf("expected invalid json passthrough, normalized=%q event=%q err=%v", string(normalized), eventType, err)
	}

	compatPayload := []byte(`{"type":"response.create","response":{"input":[]}}`)
	if normalized, eventType, err := normalizeOpenAIRealtimeClientPayload(compatPayload, wsconn.TextMessage, "gpt-4o", true); err != nil || eventType != "response.create" || string(normalized) != string(compatPayload) {
		t.Fatalf("expected compat mode passthrough, normalized=%q event=%q err=%v", string(normalized), eventType, err)
	}

	withResponse, eventType, err := normalizeOpenAIRealtimeClientPayload([]byte(`{"type":"response.create","response":{"input":[]}}`), wsconn.TextMessage, "gpt-4o", false)
	if err != nil || eventType != "response.create" {
		t.Fatalf("expected response.create normalization, event=%q err=%v", eventType, err)
	}
	var withResponseMessage map[string]any
	if err := json.Unmarshal(withResponse, &withResponseMessage); err != nil {
		t.Fatalf("failed to decode normalized response payload: %v", err)
	}
	response, _ := withResponseMessage["response"].(map[string]any)
	if got := anyToString(response["model"]); got != "gpt-4o" {
		t.Fatalf("expected response model backfill, got %q", got)
	}

	topLevel, eventType, err := normalizeOpenAIRealtimeClientPayload([]byte(`{"type":"response.create"}`), wsconn.TextMessage, "gpt-4o-mini", false)
	if err != nil || eventType != "response.create" {
		t.Fatalf("expected top-level normalization, event=%q err=%v", eventType, err)
	}
	var topLevelMessage map[string]any
	if err := json.Unmarshal(topLevel, &topLevelMessage); err != nil {
		t.Fatalf("failed to decode normalized top-level payload: %v", err)
	}
	if got := anyToString(topLevelMessage["model"]); got != "gpt-4o-mini" {
		t.Fatalf("expected top-level model backfill, got %q", got)
	}

	preseeded, _, err := normalizeOpenAIRealtimeClientPayload([]byte(`{"type":"response.create","response":{"model":"o1","input":[]}}`), wsconn.TextMessage, "gpt-4o", false)
	if err != nil {
		t.Fatalf("expected preseeded payload to normalize without error, got %v", err)
	}
	var preseededMessage map[string]any
	if err := json.Unmarshal(preseeded, &preseededMessage); err != nil {
		t.Fatalf("failed to decode preseeded payload: %v", err)
	}
	preseededResponse, _ := preseededMessage["response"].(map[string]any)
	if got := anyToString(preseededResponse["model"]); got != "o1" {
		t.Fatalf("expected explicit response model to win, got %q", got)
	}

	rawPreseeded := []byte(`{"type":"response.create","response":{"model":"o1","input":[],"unknown_number":12345678901234567890}}`)
	if normalized, _, err := normalizeOpenAIRealtimeClientPayload(rawPreseeded, wsconn.TextMessage, "gpt-4o", false); err != nil || string(normalized) != string(rawPreseeded) {
		t.Fatalf("expected preseeded response.create to remain byte-identical, normalized=%q err=%v", string(normalized), err)
	}

	withUnknownNumber, _, err := normalizeOpenAIRealtimeClientPayload([]byte(`{"type":"response.create","response":{"input":[],"unknown_number":12345678901234567890}}`), wsconn.TextMessage, "gpt-4o", false)
	if err != nil {
		t.Fatalf("expected raw-message normalization to succeed, got %v", err)
	}
	var rawMessage map[string]json.RawMessage
	if err := json.Unmarshal(withUnknownNumber, &rawMessage); err != nil {
		t.Fatalf("failed to decode raw-message normalized payload: %v", err)
	}
	responseRaw := map[string]json.RawMessage{}
	if err := json.Unmarshal(rawMessage["response"], &responseRaw); err != nil {
		t.Fatalf("failed to decode normalized response payload: %v", err)
	}
	if string(responseRaw["unknown_number"]) != "12345678901234567890" {
		t.Fatalf("expected unknown numeric field to preserve raw precision, got %s", responseRaw["unknown_number"])
	}

	cancelPayload, eventType, err := normalizeOpenAIRealtimeClientPayload([]byte(`{"type":"response.cancel"}`), wsconn.TextMessage, "gpt-4o", false)
	if err != nil || eventType != "response.cancel" || string(cancelPayload) != `{"type":"response.cancel"}` {
		t.Fatalf("expected non-response.create realtime payload to pass through, payload=%q event=%q err=%v", string(cancelPayload), eventType, err)
	}
	blankModelPayload, eventType, err := normalizeOpenAIRealtimeClientPayload([]byte(`{"type":"response.create","response":{"input":[]}}`), wsconn.TextMessage, "   ", false)
	if err != nil || eventType != "response.create" || string(blankModelPayload) != `{"type":"response.create","response":{"input":[]}}` {
		t.Fatalf("expected blank model normalization to preserve payload, payload=%q event=%q err=%v", string(blankModelPayload), eventType, err)
	}

	if got := anyToString(123); got != "" {
		t.Fatalf("expected non-string conversion to return empty string, got %q", got)
	}
	if usage := openAIRealtimeResponseUsage("evt_ignored", nil); usage != nil {
		t.Fatalf("expected nil response usage to stay nil, got %+v", usage)
	}
	responseEvent := &types.ResponseEvent{ID: " resp_usage ", Usage: &types.UsageEvent{TotalTokens: 9}}
	if usage := openAIRealtimeResponseUsage(" evt_usage ", responseEvent); usage == nil || usage.TotalTokens != 9 ||
		usage.Source != types.UsageSourceRealtimeResponse ||
		usage.BillingBasis != types.UsageBillingBasisTokens ||
		usage.ProviderEventID != "evt_usage" ||
		usage.ResponseID != "resp_usage" {
		t.Fatalf("expected response usage passthrough, got %+v", usage)
	}

	tokenUsagePayload := []byte(`{"event_id":" evt_transcript_tokens ","type":"conversation.item.input_audio_transcription.completed","item_id":" item_1 ","usage":{"input_tokens":7,"total_tokens":7}}`)
	tokenUsage := openAIRealtimeInputAudioTranscriptionUsage("conversation.item.input_audio_transcription.completed", " evt_override ", tokenUsagePayload)
	if tokenUsage == nil ||
		tokenUsage.Source != types.UsageSourceInputAudioTranscription ||
		tokenUsage.BillingBasis != types.UsageBillingBasisTokens ||
		tokenUsage.ProviderEventID != "evt_override" ||
		tokenUsage.ItemID != "item_1" ||
		tokenUsage.InputTokens != 7 ||
		tokenUsage.TotalTokens != 7 ||
		tokenUsage.DurationSeconds != 0 {
		t.Fatalf("expected token transcription usage attribution, got %+v", tokenUsage)
	}

	durationUsagePayload := []byte(`{"event_id":" evt_transcript_duration ","type":"conversation.item.input_audio_transcription.completed","item_id":" item_2 ","usage":{"duration_seconds":2.5}}`)
	durationUsage := openAIRealtimeInputAudioTranscriptionUsage("conversation.item.input_audio_transcription.completed", "", durationUsagePayload)
	if durationUsage == nil ||
		durationUsage.Source != types.UsageSourceInputAudioTranscription ||
		durationUsage.BillingBasis != types.UsageBillingBasisDuration ||
		durationUsage.ProviderEventID != "evt_transcript_duration" ||
		durationUsage.ItemID != "item_2" ||
		durationUsage.DurationSeconds != 2.5 {
		t.Fatalf("expected duration transcription usage attribution, got %+v", durationUsage)
	}
}

func TestOpenAIRealtimeReadLoopWithNilConnClosesSession(t *testing.T) {
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	session := &openAIRealtimeSession{
		model:     "gpt-5",
		sessionID: "nil-conn-session",
		recvCh:    make(chan openAIRealtimeOutbound, 1),
		closed:    make(chan struct{}),
		detached:  make(chan struct{}),
	}

	session.readLoop()

	select {
	case <-session.closed:
	default:
		t.Fatal("expected read loop with nil conn to close the session")
	}

	if outbound, ok := <-session.recvCh; ok {
		t.Fatalf("expected nil conn read loop to close without outbound payload, got %+v", outbound)
	}
}

func TestOpenAIResponsesWSDefersReadUntilRecvAndFiltersSessionCreated(t *testing.T) {
	releaseDone := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"session.created","session":{"id":"sess_private"}}`)); err != nil {
			t.Errorf("failed to write private bootstrap event: %v", err)
			return
		}
		if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_visible","status":"in_progress"}}`)); err != nil {
			t.Errorf("failed to write visible responses event: %v", err)
			return
		}
		<-releaseDone
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{})
	if errWithCode != nil {
		t.Fatalf("expected responses websocket session to open, got %v", errWithCode)
	}
	defer func() {
		close(releaseDone)
		session.Abort("test_cleanup")
	}()

	if _, ok := session.(*responsesws.NativeSession); !ok {
		t.Fatalf("expected OpenResponsesWS to use common native helper, got %T", session)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, payload, _, _, err := openAIResponsesWSTestRecv(ctx, session)
	if err != nil {
		t.Fatalf("expected first Recv to return provider event, got %v", err)
	}
	if strings.Contains(string(payload), "session.created") {
		t.Fatalf("expected private session.created bootstrap to be filtered, got %q", payload)
	}
	if !strings.Contains(string(payload), "response.created") || !strings.Contains(string(payload), "resp_visible") {
		t.Fatalf("expected visible responses event, got %q", payload)
	}
}

func TestOpenAIResponsesWSHTTPBridgeUsesBridgeSession(t *testing.T) {
	provider := newOpenAIRealtimeTestProvider("http://127.0.0.1")
	session, errWithCode := provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{
		Transport: runtimesession.TransportModeResponsesHTTPBridge,
	})
	if errWithCode != nil {
		t.Fatalf("expected http bridge session to open without native websocket dial, got %v", errWithCode)
	}
	if _, ok := session.(*responsesws.BridgeSession); !ok {
		t.Fatalf("expected common bridge ResponsesWS upstream, got %T", session)
	}
	session.Abort("test_cleanup")
}

func TestOpenAIResponsesWSUnsupportedWhenResponsesEndpointMissing(t *testing.T) {
	provider := newOpenAIRealtimeTestProvider("http://127.0.0.1:1")
	provider.Config.Responses = ""

	for _, tc := range []struct {
		name      string
		transport runtimesession.TransportMode
	}{
		{name: "native", transport: runtimesession.TransportModeResponsesWS},
		{name: "http bridge", transport: runtimesession.TransportModeResponsesHTTPBridge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, errWithCode := provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{
				Transport: tc.transport,
			})
			if session != nil {
				session.Abort("test_cleanup")
			}
			if errWithCode == nil || errWithCode.StatusCode != http.StatusUpgradeRequired || errWithCode.Code != "responses_ws_unsupported_for_channel" {
				t.Fatalf("expected missing Responses endpoint to return responses_ws_unsupported_for_channel, session=%T err=%+v", session, errWithCode)
			}
		})
	}
}

func TestOpenAIResponsesWSCustomNativeRequiresExplicitCapability(t *testing.T) {
	proxy := ""
	disabled := CreateOpenAIProvider(&model.Channel{
		Key:   "sk-test",
		Type:  config.ChannelTypeCustom,
		Other: `{"responses_ws_self_hosted":true}`,
		Proxy: &proxy,
	}, "http://127.0.0.1:1")
	session, errWithCode := disabled.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{})
	if session != nil {
		session.Abort("test_cleanup")
	}
	if errWithCode == nil || errWithCode.StatusCode != http.StatusUpgradeRequired || errWithCode.Code != "responses_ws_unsupported_for_channel" {
		t.Fatalf("expected custom native ResponsesWS without explicit capability to be unsupported, session=%T err=%+v", session, errWithCode)
	}

	releaseDone := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		<-releaseDone
	})
	defer server.Close()

	enabled := CreateOpenAIProvider(&model.Channel{
		Key:   "sk-test",
		Type:  config.ChannelTypeCustom,
		Other: `{"responses_ws_native":true,"responses_ws_self_hosted":true}`,
		Proxy: &proxy,
	}, server.URL)
	session, errWithCode = enabled.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{})
	if errWithCode != nil {
		close(releaseDone)
		t.Fatalf("expected custom native ResponsesWS with explicit capability to open, got %v", errWithCode)
	}
	close(releaseDone)
	session.Abort("test_cleanup")
}

func TestOpenAIResponsesWSOpenAITypeCustomBaseURLRequiresExplicitNativeCapability(t *testing.T) {
	proxy := ""
	customBaseURL := "http://127.0.0.1:1"
	disabled := CreateOpenAIProvider(&model.Channel{
		Key:     "sk-test",
		Type:    config.ChannelTypeOpenAI,
		BaseURL: &customBaseURL,
		Other:   `{"responses_ws_self_hosted":true}`,
		Proxy:   &proxy,
	}, "https://api.openai.com")
	session, errWithCode := disabled.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{})
	if session != nil {
		session.Abort("test_cleanup")
	}
	if errWithCode == nil || errWithCode.StatusCode != http.StatusUpgradeRequired || errWithCode.Code != "responses_ws_unsupported_for_channel" {
		t.Fatalf("expected OpenAI type with custom base URL and no explicit native capability to be unsupported, session=%T err=%+v", session, errWithCode)
	}

	releaseDone := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		<-releaseDone
	})
	defer server.Close()

	enabledBaseURL := server.URL
	enabled := CreateOpenAIProvider(&model.Channel{
		Key:     "sk-test",
		Type:    config.ChannelTypeOpenAI,
		BaseURL: &enabledBaseURL,
		Other:   `{"responses_ws_native":true,"responses_ws_self_hosted":true}`,
		Proxy:   &proxy,
	}, "https://api.openai.com")
	session, errWithCode = enabled.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{})
	if errWithCode != nil {
		close(releaseDone)
		t.Fatalf("expected explicit native capability to allow OpenAI type custom base URL, got %v", errWithCode)
	}
	close(releaseDone)
	session.Abort("test_cleanup")
}

func TestOpenAIResponsesWSHTTPBridgeStreamsResponsesEvents(t *testing.T) {
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = &http.Client{}
	defer func() {
		requester.HTTPClient = originalHTTPClient
	}()
	seenRequest := make(chan types.OpenAIResponsesRequest, 1)
	seenRawRequest := make(chan map[string]json.RawMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rawRequest map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&rawRequest); err != nil {
			t.Errorf("decode bridge request: %v", err)
		} else {
			seenRawRequest <- rawRequest
			requestBytes, _ := json.Marshal(rawRequest)
			var request types.OpenAIResponsesRequest
			if err := json.Unmarshal(requestBytes, &request); err != nil {
				t.Errorf("decode typed bridge request: %v", err)
			}
			seenRequest <- request
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bridge\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n"))
	}))
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	provider.Usage = &types.Usage{}
	session, errWithCode := provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{
		Transport: runtimesession.TransportModeResponsesHTTPBridge,
	})
	if errWithCode != nil {
		t.Fatalf("expected http bridge session to open, got %v", errWithCode)
	}
	defer session.Abort("test_cleanup")

	result := session.(responsesws.TransportSendCapable).SendClientWithResult(context.Background(), responsesws.SendRequest{AttemptID: "attempt-test", Frame: responsesws.NewTextFrame([]byte(`{"type":"response.create","event_id":"evt_bridge","model":"gpt-5","input":"hello","stream":true,"background":false,"stream_options":{"include_usage":true},"generate":true,"unknown_number":12345678901234567890}`))})
	if result.Status != responsesws.ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected bridge create to be attempted, got %+v", result)
	}
	select {
	case request := <-seenRequest:
		if request.Model != "gpt-5" || !request.Stream {
			t.Fatalf("expected top-level response.create to become streamed provider request, got %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("expected provider request to be observed")
	}
	select {
	case rawRequest := <-seenRawRequest:
		if _, ok := rawRequest["type"]; ok {
			t.Fatalf("expected websocket event type to be stripped from HTTP bridge body, got %s", rawRequest["type"])
		}
		if _, ok := rawRequest["event_id"]; ok {
			t.Fatalf("expected websocket event_id to be stripped from HTTP bridge body, got %s", rawRequest["event_id"])
		}
		if _, ok := rawRequest["background"]; ok {
			t.Fatalf("expected websocket background to be stripped from HTTP bridge body, got %s", rawRequest["background"])
		}
		if string(rawRequest["stream_options"]) != `{"include_usage":true}` {
			t.Fatalf("expected stream_options to be preserved in HTTP bridge body, got %s", rawRequest["stream_options"])
		}
		if string(rawRequest["generate"]) != "true" {
			t.Fatalf("expected unknown/future generate field to be preserved, got %s", rawRequest["generate"])
		}
		if string(rawRequest["unknown_number"]) != "12345678901234567890" {
			t.Fatalf("expected unknown numeric field to preserve raw precision, got %s", rawRequest["unknown_number"])
		}
		if string(rawRequest["stream"]) != "true" {
			t.Fatalf("expected bridge body to force stream true, got %s", rawRequest["stream"])
		}
	case <-time.After(time.Second):
		t.Fatal("expected raw provider request to be observed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	opened, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv bridge opened: %v", err)
	}
	if opened.DetailOrigin != responsesws.RecvDetailOriginBridgeStreamOpened {
		t.Fatalf("expected bridge_stream_opened, got %+v", opened)
	}
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv bridge provider event: %v", err)
	}
	if event.Frame == nil || !strings.Contains(string(event.Frame.Payload()), "resp_bridge") || event.DetailOrigin != responsesws.RecvDetailOriginProviderStream {
		t.Fatalf("expected provider_stream bridge frame, got %+v", event)
	}
	if event.Usage == nil || event.Usage.TotalTokens != 3 {
		t.Fatalf("expected usage to be surfaced to actor path, got %+v", event.Usage)
	}
}

func TestOpenAIResponsesWSHTTPBridgeDeliversFinalEventWithoutTrailingNewline(t *testing.T) {
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = &http.Client{}
	defer func() {
		requester.HTTPClient = originalHTTPClient
	}()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_no_trailing_newline","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`))
	}))
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	provider.Usage = &types.Usage{}
	session, errWithCode := provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{
		Transport: runtimesession.TransportModeResponsesHTTPBridge,
	})
	if errWithCode != nil {
		t.Fatalf("expected http bridge session to open, got %v", errWithCode)
	}
	defer session.Abort("test_cleanup")

	result := session.(responsesws.TransportSendCapable).SendClientWithResult(context.Background(), responsesws.SendRequest{AttemptID: "attempt-test", Frame: responsesws.NewTextFrame([]byte(`{"type":"response.create","event_id":"evt_no_newline","model":"gpt-5","input":"hello"}`))})
	if result.Status != responsesws.ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected bridge create to be attempted, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	opened, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv bridge opened: %v", err)
	}
	if opened.DetailOrigin != responsesws.RecvDetailOriginBridgeStreamOpened {
		t.Fatalf("expected bridge_stream_opened, got %+v", opened)
	}
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv bridge provider event without trailing newline: %v", err)
	}
	if event.Frame == nil || !strings.Contains(string(event.Frame.Payload()), "resp_no_trailing_newline") || event.DetailOrigin != responsesws.RecvDetailOriginProviderStream {
		t.Fatalf("expected provider_stream bridge terminal frame, got %+v", event)
	}
	if event.Usage == nil || event.Usage.TotalTokens != 5 {
		t.Fatalf("expected terminal usage to be surfaced, got %+v", event.Usage)
	}
}

func TestOpenAIResponsesWSHTTPBridgeClassicAzureUsesResourceLevelResponsesURL(t *testing.T) {
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = &http.Client{}
	defer func() {
		requester.HTTPClient = originalHTTPClient
	}()

	seenURL := make(chan string, 1)
	seenRawRequest := make(chan map[string]json.RawMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenURL <- r.URL.String()
		var rawRequest map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&rawRequest); err != nil {
			t.Errorf("decode Azure bridge request: %v", err)
		} else {
			seenRawRequest <- rawRequest
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_azure_bridge","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
	}))
	defer server.Close()

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{
		Key:   "azure-key",
		Type:  config.ChannelTypeAzure,
		Other: `{"api_version":"2024-10-01-preview","responses_ws_transport":"http_bridge","responses_ws_self_hosted":true}`,
		Proxy: &proxy,
	}, server.URL)
	provider.IsAzure = true
	provider.Usage = &types.Usage{}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	provider.Context = ctx

	session, errWithCode := provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{
		Transport: runtimesession.TransportModeResponsesHTTPBridge,
	})
	if errWithCode != nil {
		t.Fatalf("expected classic Azure http bridge session to open, got %v", errWithCode)
	}
	defer session.Abort("test_cleanup")

	result := session.(responsesws.TransportSendCapable).SendClientWithResult(context.Background(), responsesws.SendRequest{AttemptID: "attempt-test", Frame: responsesws.NewTextFrame([]byte(`{"type":"response.create","event_id":"evt_azure_bridge","model":"gpt-5","input":"hello"}`))})
	if result.Status != responsesws.ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected Azure bridge create to be attempted, got %+v", result)
	}

	select {
	case got := <-seenURL:
		if got != "/openai/responses?api-version=2024-10-01-preview" {
			t.Fatalf("expected classic Azure bridge to use resource-level Responses URL, got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("expected Azure bridge request URL to be observed")
	}
	select {
	case rawRequest := <-seenRawRequest:
		if string(rawRequest["model"]) != `"gpt-5"` {
			t.Fatalf("expected Azure bridge request body to preserve model, got %s", rawRequest["model"])
		}
	case <-time.After(time.Second):
		t.Fatal("expected Azure bridge request body to be observed")
	}
}

func TestOpenAIResponsesWSHTTPBridgeURLPolicyRejectsUnsafeDefaults(t *testing.T) {
	t.Run("local http requires explicit responses self hosted", func(t *testing.T) {
		proxy := ""
		provider := CreateOpenAIProvider(&model.Channel{
			Key:   "sk-test",
			Type:  config.ChannelTypeOpenAI,
			Proxy: &proxy,
		}, "http://127.0.0.1:1")
		session, errWithCode := provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{
			Transport: runtimesession.TransportModeResponsesHTTPBridge,
		})
		if errWithCode != nil {
			t.Fatalf("expected bridge session to open lazily, got %v", errWithCode)
		}
		defer session.Abort("test_cleanup")

		result := session.(responsesws.TransportSendCapable).SendClientWithResult(context.Background(), responsesws.SendRequest{AttemptID: "attempt-test", Frame: responsesws.NewTextFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hello"}`))})
		if result.Status != responsesws.ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, requester.ErrUpstreamResponsesHTTPURLRequiresHTTPS) {
			t.Fatalf("expected local http bridge send to be rejected before request, got %+v", result)
		}
	})

	t.Run("metadata stays blocked even when responses self hosted", func(t *testing.T) {
		proxy := ""
		provider := CreateOpenAIProvider(&model.Channel{
			Key:   "sk-test",
			Type:  config.ChannelTypeOpenAI,
			Other: `{"responses_ws_self_hosted":true}`,
			Proxy: &proxy,
		}, "http://169.254.169.254")
		session, errWithCode := provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{
			Transport: runtimesession.TransportModeResponsesHTTPBridge,
		})
		if errWithCode != nil {
			t.Fatalf("expected bridge session to open lazily, got %v", errWithCode)
		}
		defer session.Abort("test_cleanup")

		result := session.(responsesws.TransportSendCapable).SendClientWithResult(context.Background(), responsesws.SendRequest{AttemptID: "attempt-test", Frame: responsesws.NewTextFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hello"}`))})
		if result.Status != responsesws.ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, requester.ErrUpstreamResponsesHTTPURLHostBlocked) {
			t.Fatalf("expected metadata bridge send to be rejected before request, got %+v", result)
		}
	})
}

func TestOpenAIResponsesWSHTTPBridgeURLPolicyPrecedesBridgeBodyValidation(t *testing.T) {
	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{
		Key:   "sk-test",
		Type:  config.ChannelTypeOpenAI,
		Proxy: &proxy,
	}, "http://127.0.0.1:1")
	session, errWithCode := provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{
		Transport: runtimesession.TransportModeResponsesHTTPBridge,
	})
	if errWithCode != nil {
		t.Fatalf("expected bridge session to open lazily, got %v", errWithCode)
	}
	defer session.Abort("test_cleanup")

	result := session.(responsesws.TransportSendCapable).SendClientWithResult(context.Background(), responsesws.SendRequest{AttemptID: "attempt-test", Frame: responsesws.NewTextFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hello","background":true}`))})
	if result.Status != responsesws.ResponsesWSTransportSendNotAttempted || !errors.Is(result.Err, requester.ErrUpstreamResponsesHTTPURLRequiresHTTPS) {
		t.Fatalf("expected URL policy error before unsupported background validation, got %+v", result)
	}
	if strings.Contains(result.Err.Error(), "background") {
		t.Fatalf("expected URL policy error to win over background validation, got %v", result.Err)
	}
}

func TestOpenAIResponsesWSHTTPBridgeDoesNotReuseOpenPreviousResponseID(t *testing.T) {
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = &http.Client{}
	defer func() {
		requester.HTTPClient = originalHTTPClient
	}()
	seenRawRequest := make(chan map[string]json.RawMessage, 2)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rawRequest map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&rawRequest); err != nil {
			t.Errorf("decode bridge request: %v", err)
		} else {
			seenRawRequest <- rawRequest
		}
		id := "resp_openai_bridge_first"
		if requests.Add(1) == 2 {
			id = "resp_openai_bridge_second"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"` + id + `","status":"completed"}}` + "\n\n"))
	}))
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{
		Transport:          runtimesession.TransportModeResponsesHTTPBridge,
		PreviousResponseID: "resp_open_default",
	})
	if errWithCode != nil {
		t.Fatalf("expected http bridge session to open, got %v", errWithCode)
	}
	defer session.Abort("test_cleanup")

	result := session.(responsesws.TransportSendCapable).SendClientWithResult(context.Background(), responsesws.SendRequest{AttemptID: "attempt-test", Frame: responsesws.NewTextFrame([]byte(`{"type":"response.create","event_id":"evt_first","model":"gpt-5","input":"first"}`))})
	if result.Status != responsesws.ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected first bridge create to be attempted, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv first bridge opened: %v", err)
	}
	if _, err := session.Recv(ctx); err != nil {
		t.Fatalf("recv first bridge terminal: %v", err)
	}
	first := recvSeenRawRequest(t, seenRawRequest)
	if string(first["previous_response_id"]) != `"resp_open_default"` {
		t.Fatalf("expected first bridge request to use open default, got %s", first["previous_response_id"])
	}

	result = session.(responsesws.TransportSendCapable).SendClientWithResult(context.Background(), responsesws.SendRequest{AttemptID: "attempt-test", Frame: responsesws.NewTextFrame([]byte(`{"type":"response.create","event_id":"evt_second","model":"gpt-5","input":"second"}`))})
	if result.Status != responsesws.ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected second bridge create to be attempted, got %+v", result)
	}
	second := recvSeenRawRequest(t, seenRawRequest)
	if _, ok := second["previous_response_id"]; ok {
		t.Fatalf("expected second bridge request not to reuse open default, body=%#v", second)
	}
}

func recvSeenRawRequest(t *testing.T, seen <-chan map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	select {
	case rawRequest := <-seen:
		return rawRequest
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for provider bridge HTTP request")
		return nil
	}
}

func TestOpenAIResponsesWSHTTPBridgeSeparatesOpenFailureAndStreamEOF(t *testing.T) {
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = &http.Client{}
	defer func() {
		requester.HTTPClient = originalHTTPClient
	}()

	rejectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"rate_limit","type":"provider_error","message":"provider busy"}}`, http.StatusTooManyRequests)
	}))
	defer rejectServer.Close()

	provider := newOpenAIRealtimeTestProvider(rejectServer.URL)
	provider.Usage = &types.Usage{}
	session, errWithCode := provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{
		Transport: runtimesession.TransportModeResponsesHTTPBridge,
	})
	if errWithCode != nil {
		t.Fatalf("expected reject bridge session to open, got %v", errWithCode)
	}
	result := session.(responsesws.TransportSendCapable).SendClientWithResult(context.Background(), responsesws.SendRequest{AttemptID: "attempt-test", Frame: responsesws.NewTextFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hello"}`))})
	if result.Status != responsesws.ResponsesWSTransportSendRejectedBeforeStream || result.Err != nil {
		t.Fatalf("expected provider HTTP rejection to be rejected_before_stream, got %+v", result)
	}
	rejectCtx, rejectCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer rejectCancel()
	rejectEvent, err := session.Recv(rejectCtx)
	if err != nil {
		t.Fatalf("recv provider HTTP rejection: %v", err)
	}
	if rejectEvent.DetailOrigin != responsesws.RecvDetailOriginBridgeOpenProviderError || responsesws.ClientPayloadFromError(rejectEvent.Err) == nil || rejectEvent.ProviderClose != nil {
		t.Fatalf("expected provider HTTP rejection payload through event path without provider close, got %+v", rejectEvent)
	}
	session.Abort("test_cleanup")

	eofServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer eofServer.Close()
	provider = newOpenAIRealtimeTestProvider(eofServer.URL)
	provider.Usage = &types.Usage{}
	session, errWithCode = provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{
		Transport: runtimesession.TransportModeResponsesHTTPBridge,
	})
	if errWithCode != nil {
		t.Fatalf("expected eof bridge session to open, got %v", errWithCode)
	}
	defer session.Abort("test_cleanup")
	result = session.(responsesws.TransportSendCapable).SendClientWithResult(context.Background(), responsesws.SendRequest{AttemptID: "attempt-test", Frame: responsesws.NewTextFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hello"}`))})
	if result.Status != responsesws.ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("expected stream open to be attempted, got %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	opened, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv stream opened: %v", err)
	}
	if opened.DetailOrigin != responsesws.RecvDetailOriginBridgeStreamOpened {
		t.Fatalf("expected bridge_stream_opened before EOF, got %+v", opened)
	}
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv stream EOF: %v", err)
	}
	if event.DetailOrigin != responsesws.RecvDetailOriginBridgeStreamEOF || event.ProviderClose != nil {
		t.Fatalf("expected bridge_stream_eof without provider close, got %+v", event)
	}
}

func TestOpenAIOpenResponsesWSUsesResponsesTransportWithoutCompatMode(t *testing.T) {
	originalCompatMode := config.OpenAIRealtimeSessionCompatMode
	config.OpenAIRealtimeSessionCompatMode = true
	defer func() {
		config.OpenAIRealtimeSessionCompatMode = originalCompatMode
	}()

	releaseDone := make(chan struct{})
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"session.created","session":{"id":"sess_private"}}`)); err != nil {
			t.Errorf("failed to write private bootstrap event: %v", err)
			return
		}
		if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_visible","status":"in_progress"}}`)); err != nil {
			t.Errorf("failed to write visible responses event: %v", err)
			return
		}
		<-releaseDone
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{})
	if errWithCode != nil {
		t.Fatalf("expected responses websocket session to open, got %v", errWithCode)
	}
	defer func() {
		close(releaseDone)
		session.Abort("test_cleanup")
	}()

	if _, ok := session.(*responsesws.NativeSession); !ok {
		t.Fatalf("expected OpenResponsesWS to use common native helper, got %T", session)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, payload, _, _, err := openAIResponsesWSTestRecv(ctx, session)
	if err != nil {
		t.Fatalf("expected first Recv to return provider event, got %v", err)
	}
	if strings.Contains(string(payload), "session.created") {
		t.Fatalf("expected private session.created bootstrap to be filtered, got %q", payload)
	}
	if !strings.Contains(string(payload), "response.created") || !strings.Contains(string(payload), "resp_visible") {
		t.Fatalf("expected visible responses event, got %q", payload)
	}
}

func TestOpenAIResponsesWSUsesInlineResponseCreatePayload(t *testing.T) {
	received := make(chan []byte, 1)
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("failed to read response.create request: %v", err)
			return
		}
		received <- payload
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{})
	if errWithCode != nil {
		t.Fatalf("expected responses websocket session to open, got %v", errWithCode)
	}
	defer session.Abort("test_cleanup")

	payload := []byte(`{"type":"response.create","response":{"input":[],"unknown_number":12345678901234567890}}`)
	result := session.SendClientWithResult(context.Background(), responsesws.SendRequest{
		AttemptID: "attempt-inline-payload",
		Frame:     responsesws.NewTextFrame(payload),
	})
	if result.Status != responsesws.ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("send response.create: %+v", result)
	}
	select {
	case got := <-received:
		if string(got) != string(payload) {
			t.Fatalf("expected OpenAI ResponsesWS inline payload to pass through byte-identically\nwant: %s\n got: %s", payload, got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream response.create")
	}
}

func TestOpenAIResponsesWSMalformedProviderPayloadClosesAsProviderMalformed(t *testing.T) {
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.created"`)); err != nil {
			t.Errorf("failed to write malformed provider event: %v", err)
		}
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{})
	if errWithCode != nil {
		t.Fatalf("expected responses websocket session to open, got %v", errWithCode)
	}
	defer session.Abort("test_cleanup")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv malformed provider event: %v", err)
	}
	if event.DetailOrigin != responsesws.RecvDetailOriginProviderMalformed || responsesws.PayloadOriginForDetailOrigin(event.DetailOrigin) != responsesws.PayloadOriginProxyLocal || event.Err == nil {
		t.Fatalf("expected provider_malformed proxy-local event, got %+v", event)
	}
}

func TestOpenAIResponsesWSSchemaInvalidProviderPayloadClosesAsProviderMalformed(t *testing.T) {
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"foo":1}`)); err != nil {
			t.Errorf("failed to write schema-invalid provider event: %v", err)
		}
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{})
	if errWithCode != nil {
		t.Fatalf("expected responses websocket session to open, got %v", errWithCode)
	}
	defer session.Abort("test_cleanup")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv schema-invalid provider event: %v", err)
	}
	if event.DetailOrigin != responsesws.RecvDetailOriginProviderMalformed || responsesws.PayloadOriginForDetailOrigin(event.DetailOrigin) != responsesws.PayloadOriginProxyLocal || !errors.Is(event.Err, responsesws.ErrInvalidProviderEventPayload) {
		t.Fatalf("expected provider_malformed proxy-local event, got %+v", event)
	}
}

func TestOpenAIResponsesWSKnownTerminalBadResponseShapeClosesAsProviderMalformed(t *testing.T) {
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.completed","response":"opaque"}`)); err != nil {
			t.Errorf("failed to write bad terminal provider event: %v", err)
		}
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{})
	if errWithCode != nil {
		t.Fatalf("expected responses websocket session to open, got %v", errWithCode)
	}
	defer session.Abort("test_cleanup")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	event, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("recv bad terminal provider event: %v", err)
	}
	if event.DetailOrigin != responsesws.RecvDetailOriginProviderMalformed || responsesws.PayloadOriginForDetailOrigin(event.DetailOrigin) != responsesws.PayloadOriginProxyLocal || !errors.Is(event.Err, responsesws.ErrInvalidProviderEventPayload) || event.Frame != nil {
		t.Fatalf("expected provider_malformed proxy-local event for bad terminal shape, got %+v", event)
	}
}

func TestOpenAIResponsesWSFutureProviderEventShapePassesThrough(t *testing.T) {
	payload := []byte(`{"type":"response.future","event_id":"evt_future","response":"opaque","future":{"enabled":true}}`)
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		if err := conn.WriteMessage(wsconn.TextMessage, payload); err != nil {
			t.Errorf("failed to write future provider event: %v", err)
		}
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := provider.OpenResponsesWS(context.Background(), "gpt-5", responsesws.OpenOptions{})
	if errWithCode != nil {
		t.Fatalf("expected responses websocket session to open, got %v", errWithCode)
	}
	defer session.Abort("test_cleanup")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	messageType, gotPayload, usage, origin, err := openAIResponsesWSTestRecv(ctx, session)
	if err != nil {
		t.Fatalf("recv future provider event: %v", err)
	}
	if messageType != wsconn.TextMessage || string(gotPayload) != string(payload) || usage != nil || origin != responsesws.PayloadOriginProvider {
		t.Fatalf("expected future provider event to pass through byte-identically without usage, messageType=%v payload=%s usage=%+v origin=%v", messageType, gotPayload, usage, origin)
	}
}

func TestOpenAIRealtimeConfigureConnAppliesUpstreamReadLimit(t *testing.T) {
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

	conn := dialOpenAIRealtimeManagedTestConn(t, wsURL, wsconn.Config{
		Label:        "openai realtime read limit test upstream",
		ReadLimit:    config.RealtimeWebsocketReadLimit(),
		WriteTimeout: openAIRealtimeTestWriteTimeout(),
	})
	defer conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})

	session := &openAIRealtimeSession{
		conn:     conn,
		recvCh:   make(chan openAIRealtimeOutbound, 8),
		closed:   make(chan struct{}),
		detached: make(chan struct{}),
	}
	session.startReadLoop()
	close(releaseWrite)

	event, err := session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected read-limit error event, got err=%v", err)
	}
	if event.Frame == nil || !strings.Contains(string(event.Frame.Payload()), "provider_connection_closed") {
		t.Fatalf("expected provider_connection_closed payload after read limit, got %+v", event)
	}
	if !errors.Is(event.Err, runtimerealtime.ErrSessionClosed) {
		t.Fatalf("expected read-limit event to carry RecvEvent.Err source, got %v", event.Err)
	}
}

func TestOpenAIRealtimePumpNonPeerCloseEmitsRecvEventErr(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind wsconn.CloseKind
	}{
		{name: "read error", kind: wsconn.CloseKindReadError},
		{name: "backpressure", kind: wsconn.CloseKindBackpressure},
		{name: "pong miss", kind: wsconn.CloseKindPongMiss},
		{name: "handler panic", kind: wsconn.CloseKindHandlerPanic},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := &openAIRealtimeSession{
				recvCh:   make(chan openAIRealtimeOutbound, 1),
				closed:   make(chan struct{}),
				detached: make(chan struct{}),
			}
			session.handlePumpClose(wsconn.CloseInfo{Kind: tc.kind, Reason: string(tc.kind)})
			event, err := session.Recv(context.Background())
			if err != nil {
				t.Fatalf("expected RecvEvent for %s, got err=%v", tc.kind, err)
			}
			if event.ProviderClose != nil {
				t.Fatalf("expected non-peer close not to produce ProviderClose, got %+v", event.ProviderClose)
			}
			if event.Frame == nil || !strings.Contains(string(event.Frame.Payload()), "provider_connection_closed") {
				t.Fatalf("expected provider_connection_closed payload, got %+v", event)
			}
			if !errors.Is(event.Err, runtimerealtime.ErrSessionClosed) {
				t.Fatalf("expected RecvEvent.Err for %s, got %v", tc.kind, event.Err)
			}
		})
	}
}

func TestOpenAIRealtimePumpNormalCloseEmitsProviderClose(t *testing.T) {
	session := &openAIRealtimeSession{
		recvCh:   make(chan openAIRealtimeOutbound, 1),
		closed:   make(chan struct{}),
		detached: make(chan struct{}),
	}
	session.handlePumpClose(wsconn.CloseInfo{Kind: wsconn.CloseKindNormal, Code: wsconn.CloseNormalClosure, Reason: "normal"})
	event, err := session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected ProviderClose event, got err=%v", err)
	}
	if event.ProviderClose == nil || event.ProviderClose.Code != int(wsconn.CloseNormalClosure) || event.ProviderClose.Reason != "normal" {
		t.Fatalf("expected normal close to become ProviderClose, got %+v", event.ProviderClose)
	}
	if event.Frame != nil {
		t.Fatalf("expected normal close not to produce error payload, got %+v", event.Frame)
	}
}

func TestOpenAIRealtimeSessionSelectionAndFinalizationHelpers(t *testing.T) {
	recorder := &recordingOpenAIRealtimeObserver{}
	now := time.Now()

	session := newOpenAIRealtimeHelperSession()
	session.turn = newOpenAIRealtimeTurnState(1, now, recorder)
	session.turn.rememberResponseID("resp-active")

	pending := newOpenAIRealtimeTurnState(2, now, recorder)
	pending.rememberResponseID("resp-pending")
	session.pendingTurns = []openAIRealtimePendingTurn{{state: pending, reason: "pending_recovery"}}
	session.recentFinalizedIDs = []string{"resp-finalized"}

	if selected := session.selectSupplierTurnLocked(""); selected.state != session.turn || selected.dropAttribution {
		t.Fatalf("expected empty response id to prefer active turn, got %+v", selected)
	}
	if selected := session.selectSupplierTurnLocked("resp-active"); selected.state != session.turn || selected.dropAttribution {
		t.Fatalf("expected active response id lookup to return current turn, got %+v", selected)
	}
	if selected := session.selectSupplierTurnLocked("resp-pending"); selected.state != pending || selected.dropAttribution {
		t.Fatalf("expected pending response id lookup to return pending turn, got %+v", selected)
	}
	if selected := session.selectSupplierTurnLocked("resp-finalized"); !selected.dropAttribution || selected.state != nil {
		t.Fatalf("expected finalized response id lookup to drop attribution, got %+v", selected)
	}

	session.releaseTurnStateForRecovery(session.turn, "supplier_recovery")
	if session.turn != nil || len(session.pendingTurns) != 2 || session.pendingTurns[1].reason != "supplier_recovery" {
		t.Fatalf("expected active turn release to move turn into pending queue, pending=%+v", session.pendingTurns)
	}
	session.releaseTurnStateForRecovery(pending, "updated_reason")
	if session.pendingTurns[0].reason != "updated_reason" {
		t.Fatalf("expected pending release to update recovery reason, got %+v", session.pendingTurns[0])
	}
	if index := session.pendingTurnIndexLocked(pending); index != 0 {
		t.Fatalf("expected pending turn index 0, got %d", index)
	}
	if index := session.pendingTurnIndexLocked(newOpenAIRealtimeTurnState(3, now, recorder)); index != -1 {
		t.Fatalf("expected unknown pending turn index -1, got %d", index)
	}

	session.turn = newOpenAIRealtimeTurnState(3, now, recorder)
	session.turn.rememberResponseID("resp-finalize-current")
	finalizedCurrent := session.finalizeObservedTurnState(session.turn, "response.done", now)
	if len(finalizedCurrent) != 1 || session.turn != nil {
		t.Fatalf("expected current turn finalization to produce one finalizer, finalized=%d turn=%+v", len(finalizedCurrent), session.turn)
	}
	runOpenAIRealtimeFinalizers(finalizedCurrent)

	pendingFinalizer := newOpenAIRealtimeTurnState(4, now, recorder)
	pendingFinalizer.rememberResponseID("resp-finalize-pending")
	session.pendingTurns = []openAIRealtimePendingTurn{
		{state: pendingFinalizer, reason: ""},
		{state: nil, reason: "ignored"},
	}
	finalizedPending := session.finalizeObservedTurnState(pendingFinalizer, "fallback_reason", now)
	if len(finalizedPending) != 1 || len(session.pendingTurns) != 1 {
		t.Fatalf("expected pending turn finalization to remove one pending turn, finalized=%d pending=%d", len(finalizedPending), len(session.pendingTurns))
	}
	runOpenAIRealtimeFinalizers(finalizedPending)

	session.pendingTurns = []openAIRealtimePendingTurn{
		{state: newOpenAIRealtimeTurnState(5, now, recorder), reason: ""},
		{state: nil, reason: "ignored"},
	}
	finalizedAll := session.finalizePendingTurns("default_reason", now)
	if len(finalizedAll) != 1 || len(session.pendingTurns) != 0 {
		t.Fatalf("expected finalizePendingTurns to flush pending queue, finalized=%d pending=%d", len(finalizedAll), len(session.pendingTurns))
	}
	runOpenAIRealtimeFinalizers(finalizedAll)

	session.rememberFinalizedResponseIDsLocked("", "dup", "dup")
	for i := 0; i < openAIRealtimeFinalizedResponseIDLimit+2; i++ {
		session.rememberFinalizedResponseIDsLocked("resp-limit-" + string(rune('a'+i)))
	}
	if len(session.recentFinalizedIDs) != openAIRealtimeFinalizedResponseIDLimit {
		t.Fatalf("expected finalized response id history cap %d, got %d", openAIRealtimeFinalizedResponseIDLimit, len(session.recentFinalizedIDs))
	}
	if !session.isRecentlyFinalizedResponseIDLocked("resp-limit-r") {
		t.Fatal("expected newest finalized response id to be remembered")
	}
	if session.isRecentlyFinalizedResponseIDLocked("dup") {
		t.Fatal("expected oldest finalized response ids to be evicted after limit overflow")
	}
	if session.isRecentlyFinalizedResponseIDLocked("") {
		t.Fatal("expected blank finalized response id lookup to return false")
	}
	if recorder.finalizeCount() < 3 {
		t.Fatalf("expected multiple helper finalizers to run, got %d", recorder.finalizeCount())
	}
}

func TestOpenAIRealtimeSessionQueueLifecycleAndSendClientGuards(t *testing.T) {
	if event, err, handled := (*openAIRealtimeSession)(nil).recvQueuedOutbound(); !handled || !errors.Is(err, runtimerealtime.ErrSessionClosed) || event != (runtimerealtime.RecvEvent{}) {
		t.Fatalf("expected nil session queue read to report session closed, event=%+v err=%v handled=%v", event, err, handled)
	}
	if event, err := decodeOpenAIRealtimeOutbound(openAIRealtimeOutbound{}, false); !errors.Is(err, runtimerealtime.ErrSessionClosed) || event != (runtimerealtime.RecvEvent{}) {
		t.Fatalf("expected closed outbound decode to report session closed, event=%+v err=%v", event, err)
	}
	providerClose := &runtimerealtime.ProviderClose{Code: int(wsconn.ClosePolicyViolation), Reason: "quota exhausted", Err: runtimerealtime.ErrSessionClosed}
	if event, err := decodeOpenAIRealtimeOutbound(openAIRealtimeOutbound{
		providerClose: providerClose,
		origin:        runtimerealtime.RealtimePayloadOriginProvider,
	}, true); err != nil || event.ProviderClose != providerClose || event.Frame != nil || event.Usage != nil || event.Err != nil || event.Origin != runtimerealtime.RealtimePayloadOriginProvider {
		t.Fatalf("expected provider close only event, event=%+v err=%v", event, err)
	}
	providerErr := errors.New("provider business error")
	if event, err := decodeOpenAIRealtimeOutbound(openAIRealtimeOutbound{
		err:    providerErr,
		origin: runtimerealtime.RealtimePayloadOriginProvider,
	}, true); err != nil || !errors.Is(event.Err, providerErr) || event.Frame != nil || event.Usage != nil || event.ProviderClose != nil || event.Origin != runtimerealtime.RealtimePayloadOriginProvider {
		t.Fatalf("expected provider error event without top-level error, event=%+v err=%v", event, err)
	}
	if event, err := decodeOpenAIRealtimeOutbound(openAIRealtimeOutbound{
		messageType: wsconn.TextMessage,
		payload:     []byte("queued"),
		usage:       &types.UsageEvent{TotalTokens: 1},
		origin:      runtimerealtime.RealtimePayloadOriginProvider,
	}, true); err != nil || event.Frame == nil || event.Frame.Kind() != runtimerealtime.FrameKindText || event.Usage == nil || event.Usage.TotalTokens != 1 || event.ProviderClose != nil || event.Err != nil || event.Origin != runtimerealtime.RealtimePayloadOriginProvider {
		t.Fatalf("expected frame+usage event without top-level error, event=%+v err=%v", event, err)
	}
	if terminal, reason := openAIRealtimeTurnTerminal(types.EventTypeResponseDone, nil); !terminal || reason != types.EventTypeResponseDone {
		t.Fatalf("expected response.done helper classification, terminal=%v reason=%q", terminal, reason)
	}

	session := newOpenAIRealtimeHelperSession()
	if handled := session.enqueueOutbound(openAIRealtimeOutbound{messageType: wsconn.TextMessage, payload: []byte("queued")}); !handled {
		t.Fatal("expected enqueueOutbound to queue active outbound")
	}
	if event, err, handled := session.recvQueuedOutbound(); !handled || err != nil || event.Frame == nil || event.Frame.Kind() != runtimerealtime.FrameKindText || string(event.Frame.Payload()) != "queued" {
		t.Fatalf("expected queued outbound recv, event=%+v err=%v handled=%v", event, err, handled)
	}
	observerErr := runtimerealtime.NewClientPayloadError(errors.New("quota"), []byte(`{"type":"error","error":{"message":"quota"}}`))
	if handled := session.enqueueOutbound(openAIRealtimeOutbound{
		messageType: wsconn.TextMessage,
		payload:     []byte(`{"type":"response.done","response":{"usage":{"total_tokens":1}}}`),
		usage:       &types.UsageEvent{TotalTokens: 1},
		origin:      runtimerealtime.RealtimePayloadOriginProvider,
		err:         observerErr,
	}); !handled {
		t.Fatal("expected frame+usage+err outbound to enqueue as split events")
	}
	if event, err, handled := session.recvQueuedOutbound(); !handled || err != nil || event.Frame == nil || event.Usage == nil || event.Err != nil || event.ProviderClose != nil || event.Origin != runtimerealtime.RealtimePayloadOriginProvider {
		t.Fatalf("expected first split event to preserve frame+usage without err, event=%+v err=%v handled=%v", event, err, handled)
	}
	if event, err, handled := session.recvQueuedOutbound(); !handled || err != nil || event.Frame == nil || event.Usage != nil || !errors.Is(event.Err, observerErr) || event.ProviderClose != nil || event.Origin != runtimerealtime.RealtimePayloadOriginProxyLocal {
		t.Fatalf("expected second split event to carry client payload error without usage, event=%+v err=%v handled=%v", event, err, handled)
	}
	if normalized := normalizeOpenAIRealtimeOutbound(openAIRealtimeOutbound{
		messageType:   wsconn.TextMessage,
		payload:       []byte("ignored"),
		providerClose: providerClose,
		usage:         &types.UsageEvent{TotalTokens: 1},
		origin:        runtimerealtime.RealtimePayloadOriginProvider,
		err:           errors.New("ignored"),
	}); len(normalized) != 1 || normalized[0].providerClose != providerClose || len(normalized[0].payload) != 0 || normalized[0].usage != nil || normalized[0].err != nil {
		t.Fatalf("expected provider close normalization to keep only ProviderClose, got %+v", normalized)
	}
	if normalized := normalizeOpenAIRealtimeOutbound(openAIRealtimeOutbound{
		usage:  &types.UsageEvent{TotalTokens: 2},
		origin: runtimerealtime.RealtimePayloadOriginProvider,
		err:    observerErr,
	}); len(normalized) != 2 || normalized[0].usage == nil || normalized[0].err != nil || normalized[1].usage != nil || !errors.Is(normalized[1].err, observerErr) {
		t.Fatalf("unexpected normalized openai usage+err shape: %+v", normalized)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, _, err := openAITestRecv(ctx, session); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled Recv to return context cancellation, got %v", err)
	}

	detachedSession := newOpenAIRealtimeHelperSession()
	close(detachedSession.detached)
	if _, _, _, _, err := openAITestRecv(context.Background(), detachedSession); !errors.Is(err, runtimerealtime.ErrSessionClosed) {
		t.Fatalf("expected detached Recv to return session closed, got %v", err)
	}

	closedSession := newOpenAIRealtimeHelperSession()
	close(closedSession.closed)
	if _, _, _, _, err := openAITestRecv(context.Background(), closedSession); !errors.Is(err, runtimerealtime.ErrSessionClosed) {
		t.Fatalf("expected closed Recv to return session closed, got %v", err)
	}

	if (*openAIRealtimeSession)(nil).isDetached() != true {
		t.Fatal("expected nil realtime session to report detached")
	}

	session.Detach("client_detached")
	if !session.isDetached() || session.detachReason != "client_detached" {
		t.Fatalf("expected Detach to mark session detached, detached=%v reason=%q", session.isDetached(), session.detachReason)
	}
	if handled := session.enqueueOutbound(openAIRealtimeOutbound{}); !handled {
		t.Fatal("expected detached enqueue to drain outbound without failing")
	}

	timerSession := newOpenAIRealtimeHelperSession()
	timerSession.startDetachTimer()
	if timerSession.detachTimer == nil {
		t.Fatal("expected startDetachTimer to create detach timer")
	}
	timerSession.stopDetachTimer()
	if timerSession.detachTimer != nil {
		t.Fatal("expected stopDetachTimer to clear detach timer")
	}
	timerSession.close("cleanup")
	select {
	case <-timerSession.closed:
	default:
		t.Fatal("expected close to close session")
	}

	if handled := (*openAIRealtimeSession)(nil).discardDetachedOutbound(); handled {
		t.Fatal("expected nil session detach discard to fail")
	}
	if handled := (*openAIRealtimeSession)(nil).enqueueOutbound(openAIRealtimeOutbound{}); handled {
		t.Fatal("expected nil session enqueueOutbound to fail")
	}
	closedEnqueueSession := newOpenAIRealtimeHelperSession()
	close(closedEnqueueSession.closed)
	if handled := closedEnqueueSession.enqueueOutbound(openAIRealtimeOutbound{}); handled {
		t.Fatal("expected closed realtime session enqueue to fail")
	}

	backpressuredSession := &openAIRealtimeSession{
		recvCh:   make(chan openAIRealtimeOutbound, 1),
		closed:   make(chan struct{}),
		detached: make(chan struct{}),
	}
	backpressuredSession.recvCh <- openAIRealtimeOutbound{messageType: wsconn.TextMessage}
	originalOutboundTimeout := openAIRealtimeOutboundBackpressureTimeout
	openAIRealtimeOutboundBackpressureTimeout = 10 * time.Millisecond
	t.Cleanup(func() {
		openAIRealtimeOutboundBackpressureTimeout = originalOutboundTimeout
	})
	start := time.Now()
	if handled := backpressuredSession.enqueueOutbound(openAIRealtimeOutbound{messageType: wsconn.TextMessage}); handled {
		t.Fatal("expected backpressured realtime session enqueue to fail")
	}
	if elapsed := time.Since(start); elapsed < openAIRealtimeOutboundBackpressureTimeout {
		t.Fatalf("expected enqueue to wait for bounded backpressure timeout, elapsed=%s", elapsed)
	}

	conn, cleanupConn := newOpenAIRealtimeConnPair(t)
	defer cleanupConn()
	conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_closed_conn"})

	writeFailSession := newOpenAIRealtimeHelperSession()
	writeFailSession.conn = conn
	if err := writeFailSession.SendClient(context.Background(), openAITestTextFrame([]byte(`{"type":"response.create","response":{"input":[]}}`))); err == nil {
		t.Fatal("expected closed websocket write to fail")
	} else {
		var event *types.Event
		if !errors.As(err, &event) || event.ErrorDetail == nil || event.ErrorDetail.Code != "ws_write_failed" {
			t.Fatalf("expected ws_write_failed event, got %v", err)
		}
	}
	if writeFailSession.turn != nil {
		t.Fatalf("expected failed response.create write to roll back active turn, got %+v", writeFailSession.turn)
	}

	busySession := newOpenAIRealtimeHelperSession()
	busySession.conn, cleanupConn = newOpenAIRealtimeConnPair(t)
	defer cleanupConn()
	busySession.turn = newOpenAIRealtimeTurnState(1, time.Now(), nil)
	if err := busySession.SendClient(context.Background(), openAITestTextFrame([]byte(`{"type":"response.create","response":{"input":[]}}`))); err == nil {
		t.Fatal("expected busy realtime session to reject a second response.create")
	}

	guardSession := newOpenAIRealtimeHelperSession()
	if err := guardSession.SendClient(context.Background(), openAITestTextFrame([]byte(`{"type":"bad"`))); err == nil {
		t.Fatal("expected invalid client payload to fail")
	}
	if err := (*openAIRealtimeSession)(nil).SendClient(context.Background(), openAITestTextFrame([]byte(`{}`))); !errors.Is(err, runtimerealtime.ErrSessionClosed) {
		t.Fatalf("expected nil session SendClient to report session closed, got %v", err)
	}

	finalizerRecorder := &recordingOpenAIRealtimeObserver{}
	closingSession := newOpenAIRealtimeHelperSession()
	closingSession.turn = newOpenAIRealtimeTurnState(11, time.Now(), finalizerRecorder)
	closingSession.pendingTurns = []openAIRealtimePendingTurn{
		{state: newOpenAIRealtimeTurnState(12, time.Now(), finalizerRecorder), reason: "pending_reason"},
	}
	closingSession.close("provider_closed")
	if finalizerRecorder.finalizeCount() != 2 {
		t.Fatalf("expected close to finalize active and pending turns, got %d", finalizerRecorder.finalizeCount())
	}
}

func TestOpenAIRealtimeSessionReadRealtimeConnHeaders(t *testing.T) {
	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Key: "sk-test", Proxy: &proxy}, "https://api.openai.com")
	if provider == nil {
		t.Fatal("expected OpenAI test provider")
	}

	if terminal, reason := openAIRealtimeTurnTerminal(types.EventTypeResponseDone, nil); !terminal || reason != types.EventTypeResponseDone {
		t.Fatalf("expected response.done to be terminal, terminal=%v reason=%q", terminal, reason)
	}
	if terminal, reason := openAIRealtimeTurnTerminal("response.updated", types.NewErrorEvent("", "invalid_request_error", "bad_request", "boom")); !terminal || reason != types.EventTypeError {
		t.Fatalf("expected error event to be terminal, terminal=%v reason=%q", terminal, reason)
	}
	if terminal, reason := openAIRealtimeTurnTerminal("response.updated", nil); terminal || reason != "" {
		t.Fatalf("expected non-terminal event to remain open, terminal=%v reason=%q", terminal, reason)
	}
}

func TestOpenAIRealtimeSessionConnectionErrorsAndAzureHeaders(t *testing.T) {
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	t.Run("unsupported realtime API bubbles from open", func(t *testing.T) {
		proxy := ""
		provider := CreateOpenAIProvider(&model.Channel{Key: "sk-test", Proxy: &proxy}, "https://api.openai.com")
		provider.Config.ChatRealtime = ""

		session, errWithCode := provider.OpenRealtimeSession("gpt-4o-realtime-preview")
		if session != nil {
			t.Fatalf("expected unsupported realtime open to fail before creating a session, got %#v", session)
		}
		if errWithCode == nil || errWithCode.Code != "unsupported_api" {
			t.Fatalf("expected unsupported_api error, got %+v", errWithCode)
		}
	})

	t.Run("request failures wrap websocket dial errors", func(t *testing.T) {
		proxy := ""
		provider := CreateOpenAIProvider(&model.Channel{Key: "sk-test", Proxy: &proxy, Other: `{"self_hosted":true}`}, "http://127.0.0.1:1")

		conn, errWithCode := provider.openRealtimeConn("gpt-4o-realtime-preview")
		if conn != nil {
			t.Fatalf("expected realtime dial failure to return no connection, got %#v", conn)
		}
		if errWithCode == nil || errWithCode.Code != "ws_request_failed" || errWithCode.StatusCode != http.StatusInternalServerError {
			t.Fatalf("expected ws_request_failed dial error, got %+v", errWithCode)
		}
	})

	t.Run("responses self hosted does not allow realtime local websocket", func(t *testing.T) {
		proxy := ""
		provider := CreateOpenAIProvider(&model.Channel{Key: "sk-test", Proxy: &proxy, Other: `{"responses_ws_self_hosted":true}`}, "http://127.0.0.1:1")

		conn, errWithCode := provider.openRealtimeConn("gpt-4o-realtime-preview")
		if conn != nil {
			t.Fatalf("expected realtime local websocket to be rejected, got %#v", conn)
		}
		if errWithCode == nil || errWithCode.Code != "ws_request_failed" {
			t.Fatalf("expected ws_request_failed for realtime local websocket, got %+v", errWithCode)
		}
	})

	t.Run("realtime self hosted does not allow responses local websocket", func(t *testing.T) {
		proxy := ""
		provider := CreateOpenAIProvider(&model.Channel{Key: "sk-test", Proxy: &proxy, Other: `{"self_hosted":true}`}, "http://127.0.0.1:1")

		conn, errWithCode := provider.openResponsesWSConn("gpt-5")
		if conn != nil {
			t.Fatalf("expected responses local websocket to be rejected, got %#v", conn)
		}
		if errWithCode == nil || errWithCode.Code != "ws_request_failed" {
			t.Fatalf("expected ws_request_failed for responses local websocket, got %+v", errWithCode)
		}
	})

	t.Run("azure websocket auth uses api key header", func(t *testing.T) {
		headerCh := make(chan http.Header, 1)
		urlCh := make(chan string, 1)
		server := newOpenAIRealtimeHeaderCaptureServer(t, headerCh, urlCh)
		defer server.Close()

		proxy := ""
		provider := CreateOpenAIProvider(&model.Channel{
			Key:   "azure-key",
			Other: `{"api_version":"2024-10-01-preview"}`,
			Proxy: &proxy,
		}, server.URL)
		provider.IsAzure = true
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
		ctx.Set("self_hosted", true)
		provider.Context = ctx

		conn, errWithCode := provider.openRealtimeConn("gpt-4o-realtime-preview")
		if errWithCode != nil {
			t.Fatalf("expected azure realtime websocket to connect, got %v", errWithCode)
		}
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_done"})

		headers := <-headerCh
		if got := headers.Get("Api-Key"); got != "azure-key" {
			t.Fatalf("expected azure websocket to authenticate with api-key header, got %q", got)
		}
		if got := headers.Get("Authorization"); got != "" {
			t.Fatalf("expected azure websocket auth not to use bearer auth header, got %q", got)
		}
		if got := headers.Get("Openai-Beta"); got != "" {
			t.Fatalf("expected realtime beta header to be opt-in, got %q", got)
		}
		if got := <-urlCh; got != "/openai/realtime?api-version=2024-10-01-preview&deployment=gpt-4o-realtime-preview" {
			t.Fatalf("expected azure preview realtime websocket URL, got %q", got)
		}
	})

	t.Run("realtime beta header is preserved only when channel config opts in", func(t *testing.T) {
		headerCh := make(chan http.Header, 1)
		server := newOpenAIRealtimeHeaderCaptureServer(t, headerCh, nil)
		defer server.Close()

		proxy := ""
		modelHeaders := `{"OpenAI-Beta":"realtime=v1"}`
		provider := CreateOpenAIProvider(&model.Channel{
			Key:          "sk-test",
			Proxy:        &proxy,
			Other:        `{"self_hosted":true}`,
			ModelHeaders: &modelHeaders,
		}, server.URL)

		conn, errWithCode := provider.openRealtimeConn("gpt-4o-realtime-preview")
		if errWithCode != nil {
			t.Fatalf("expected realtime websocket to connect, got %v", errWithCode)
		}
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_done"})

		headers := <-headerCh
		if got := headers.Get("Openai-Beta"); got != "realtime=v1" {
			t.Fatalf("expected configured realtime beta header to be preserved, got %q", got)
		}
	})

	t.Run("responses websocket merges custom headers", func(t *testing.T) {
		headerCh := make(chan http.Header, 1)
		server := newOpenAIRealtimeHeaderCaptureServer(t, headerCh, nil)
		defer server.Close()

		proxy := ""
		modelHeaders := `{"OpenAI-Organization":"org-test","X-Gateway-Auth":"gateway-token"}`
		provider := CreateOpenAIProvider(&model.Channel{
			Key:          "sk-test",
			Proxy:        &proxy,
			Other:        `{"responses_ws_self_hosted":true}`,
			ModelHeaders: &modelHeaders,
		}, server.URL)

		conn, errWithCode := provider.openResponsesWSConn("gpt-5")
		if errWithCode != nil {
			t.Fatalf("expected responses websocket to connect, got %v", errWithCode)
		}
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_done"})

		headers := <-headerCh
		if got := headers.Get("Authorization"); got != "Bearer sk-test" {
			t.Fatalf("expected responses websocket to authenticate with bearer header, got %q", got)
		}
		if got := headers.Get("Openai-Organization"); got != "org-test" {
			t.Fatalf("expected responses websocket to preserve organization header, got %q", got)
		}
		if got := headers.Get("X-Gateway-Auth"); got != "gateway-token" {
			t.Fatalf("expected responses websocket to preserve custom gateway header, got %q", got)
		}
	})

	t.Run("azure responses websocket auth uses bearer header", func(t *testing.T) {
		headerCh := make(chan http.Header, 1)
		urlCh := make(chan string, 1)
		server := newOpenAIRealtimeHeaderCaptureServer(t, headerCh, urlCh)
		defer server.Close()

		proxy := ""
		modelHeaders := `{"X-Gateway-Auth":"azure-gateway","Authorization":"Bearer should-not-send"}`
		provider := CreateOpenAIProvider(&model.Channel{
			Key:          "azure-key",
			Other:        `{"api_version":"2024-10-01-preview","responses_ws_self_hosted":true}`,
			Proxy:        &proxy,
			ModelHeaders: &modelHeaders,
		}, server.URL)
		provider.IsAzure = true
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
		provider.Context = ctx

		conn, errWithCode := provider.openResponsesWSConn("gpt-5")
		if errWithCode != nil {
			t.Fatalf("expected azure responses websocket to connect, got %v", errWithCode)
		}
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_done"})

		headers := <-headerCh
		if got := headers.Get("Api-Key"); got != "" {
			t.Fatalf("expected azure responses websocket auth not to use api-key header, got %q", got)
		}
		if got := headers.Get("Authorization"); got != "Bearer azure-key" {
			t.Fatalf("expected azure responses websocket to authenticate with bearer auth header, got %q", got)
		}
		if got := headers.Get("X-Gateway-Auth"); got != "azure-gateway" {
			t.Fatalf("expected azure responses websocket to preserve non-auth custom header, got %q", got)
		}
		if got := <-urlCh; got != "/openai/v1/responses" {
			t.Fatalf("expected azure responses websocket resource-level URL, got %q", got)
		}
	})
}

func TestOpenAIResponsesWSURLConstruction(t *testing.T) {
	proxy := "http://proxy.example"
	provider := CreateOpenAIProvider(&model.Channel{Key: "sk-test", Proxy: &proxy}, "https://api.openai.com")
	got, errWithCode := provider.responsesWSURL("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected official responses websocket URL, got %v", errWithCode)
	}
	if got != "wss://api.openai.com/v1/responses" {
		t.Fatalf("expected official responses websocket URL, got %q", got)
	}

	disabled := CreateOpenAIProvider(&model.Channel{Key: "sk-test", Type: config.ChannelTypeCustom, Proxy: &proxy}, "https://compat.example")
	disabled.Config.Responses = ""
	got, errWithCode = disabled.responsesWSURL("gpt-5")
	if errWithCode == nil || errWithCode.Code != "unsupported_api" || got != "" {
		t.Fatalf("expected disabled responses API to block websocket URL construction, url=%q err=%+v", got, errWithCode)
	}

	azureClassic := CreateOpenAIProvider(&model.Channel{Key: "azure-key", Other: `{"api_version":"2024-10-01-preview"}`, Type: config.ChannelTypeAzure, Proxy: &proxy}, "https://resource.openai.azure.com")
	azureClassic.IsAzure = true
	got, errWithCode = azureClassic.responsesWSURL("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected classic azure responses websocket URL, got %v", errWithCode)
	}
	if got != "wss://resource.openai.azure.com/openai/v1/responses" {
		t.Fatalf("expected classic azure resource-level responses websocket URL, got %q", got)
	}

	legacyPlain := CreateOpenAIProvider(&model.Channel{Key: "azure-key", Other: "2024-10-01-preview", Type: config.ChannelTypeAzure, Proxy: &proxy}, "https://resource.openai.azure.com")
	legacyPlain.IsAzure = true
	got, errWithCode = legacyPlain.responsesWSURL("gpt-5")
	if errWithCode == nil || errWithCode.Code != "invalid_azure_api_version" || got != "" {
		t.Fatalf("expected legacy plain Azure responses websocket other to fail locally, url=%q err=%+v", got, errWithCode)
	}

	azureV1 := CreateOpenAIProvider(&model.Channel{Key: "azure-key", Type: config.ChannelTypeAzureV1, Proxy: &proxy}, "https://resource.openai.azure.com")
	azureV1.IsAzure = true
	got, errWithCode = azureV1.responsesWSURL("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected azure v1 responses websocket URL, got %v", errWithCode)
	}
	if got != "wss://resource.openai.azure.com/openai/v1/responses" {
		t.Fatalf("expected azure v1 resource-level responses websocket URL, got %q", got)
	}

	azureV1OpenAIBase := CreateOpenAIProvider(&model.Channel{Key: "azure-key", Type: config.ChannelTypeAzureV1, Proxy: &proxy}, "https://resource.openai.azure.com/openai/v1/")
	azureV1OpenAIBase.IsAzure = true
	got, errWithCode = azureV1OpenAIBase.responsesWSURL("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected azure v1 /openai/v1 base responses websocket URL, got %v", errWithCode)
	}
	if got != "wss://resource.openai.azure.com/openai/v1/responses" {
		t.Fatalf("expected azure v1 /openai/v1 base not to duplicate path, got %q", got)
	}

	azureGateway := CreateOpenAIProvider(&model.Channel{Key: "azure-key", Type: config.ChannelTypeAzureV1, Proxy: &proxy}, "https://resource.openai.azure.com/gateway")
	azureGateway.IsAzure = true
	got, errWithCode = azureGateway.responsesWSURL("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected azure v1 gateway responses websocket URL, got %v", errWithCode)
	}
	if got != "wss://resource.openai.azure.com/gateway/openai/v1/responses" {
		t.Fatalf("expected azure v1 gateway prefix to be preserved, got %q", got)
	}

	azureGatewayOpenAIBase := CreateOpenAIProvider(&model.Channel{Key: "azure-key", Type: config.ChannelTypeAzureV1, Proxy: &proxy}, "https://resource.openai.azure.com/gateway/openai/v1")
	azureGatewayOpenAIBase.IsAzure = true
	got, errWithCode = azureGatewayOpenAIBase.responsesWSURL("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected azure v1 gateway /openai/v1 base responses websocket URL, got %v", errWithCode)
	}
	if got != "wss://resource.openai.azure.com/gateway/openai/v1/responses" {
		t.Fatalf("expected azure v1 gateway /openai/v1 base not to duplicate path, got %q", got)
	}

	deployment := CreateOpenAIProvider(&model.Channel{Key: "azure-key", Type: config.ChannelTypeAzureV1, Proxy: &proxy}, "https://resource.openai.azure.com/openai/deployments/gpt-5")
	deployment.IsAzure = true
	got, errWithCode = deployment.responsesWSURL("gpt-5")
	if errWithCode == nil || errWithCode.Code != "invalid_azure_responses_ws_base_url" || got != "" {
		t.Fatalf("expected deployment-path azure v1 base URL to fail locally, url=%q err=%+v", got, errWithCode)
	}
}

func TestOpenAIRealtimeWSURLConstruction(t *testing.T) {
	proxy := "http://proxy.invalid"
	provider := CreateOpenAIProvider(&model.Channel{Key: "sk-test", Proxy: &proxy}, "https://api.openai.com")
	got, errWithCode := provider.realtimeWSURL("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected official realtime websocket URL, got %v", errWithCode)
	}
	if got != "wss://api.openai.com/v1/realtime?model=gpt-4o-realtime-preview" {
		t.Fatalf("expected official realtime websocket URL, got %q", got)
	}

	disabled := CreateOpenAIProvider(&model.Channel{Key: "sk-test", Type: config.ChannelTypeCustom, Proxy: &proxy}, "https://compat.example")
	disabled.Config.ChatRealtime = ""
	got, errWithCode = disabled.realtimeWSURL("gpt-4o")
	if errWithCode == nil || errWithCode.Code != "unsupported_api" || got != "" {
		t.Fatalf("expected disabled realtime API to block websocket URL construction, url=%q err=%+v", got, errWithCode)
	}

	azureClassic := CreateOpenAIProvider(&model.Channel{Key: "azure-key", Other: `{"api_version":"2024-10-01-preview"}`, Type: config.ChannelTypeAzure, Proxy: &proxy}, "https://resource.openai.azure.com")
	azureClassic.IsAzure = true
	got, errWithCode = azureClassic.realtimeWSURL("gpt-4o-realtime-preview")
	if errWithCode != nil {
		t.Fatalf("expected classic azure realtime websocket URL, got %v", errWithCode)
	}
	if got != "wss://resource.openai.azure.com/openai/realtime?api-version=2024-10-01-preview&deployment=gpt-4o-realtime-preview" {
		t.Fatalf("expected classic azure preview realtime websocket URL, got %q", got)
	}

	legacyPlain := CreateOpenAIProvider(&model.Channel{Key: "azure-key", Other: "2024-10-01-preview", Type: config.ChannelTypeAzure, Proxy: &proxy}, "https://resource.openai.azure.com")
	legacyPlain.IsAzure = true
	got, errWithCode = legacyPlain.realtimeWSURL("gpt-4o-realtime-preview")
	if errWithCode == nil || errWithCode.Code != "invalid_azure_api_version" || got != "" {
		t.Fatalf("expected legacy plain Azure realtime other to fail locally, url=%q err=%+v", got, errWithCode)
	}

	azureV1 := CreateOpenAIProvider(&model.Channel{Key: "azure-key", Type: config.ChannelTypeAzureV1, Proxy: &proxy}, "https://resource.openai.azure.com")
	azureV1.IsAzure = true
	got, errWithCode = azureV1.realtimeWSURL("gpt-4o")
	if errWithCode != nil {
		t.Fatalf("expected azure v1 realtime websocket URL, got %v", errWithCode)
	}
	if got != "wss://resource.openai.azure.com/openai/v1/realtime?model=gpt-4o" {
		t.Fatalf("expected azure v1 resource-level realtime websocket URL, got %q", got)
	}

	azureV1OpenAIBase := CreateOpenAIProvider(&model.Channel{Key: "azure-key", Type: config.ChannelTypeAzureV1, Proxy: &proxy}, "https://resource.openai.azure.com/openai/v1/")
	azureV1OpenAIBase.IsAzure = true
	got, errWithCode = azureV1OpenAIBase.realtimeWSURL("gpt-4o")
	if errWithCode != nil {
		t.Fatalf("expected azure v1 /openai/v1 base realtime websocket URL, got %v", errWithCode)
	}
	if got != "wss://resource.openai.azure.com/openai/v1/realtime?model=gpt-4o" {
		t.Fatalf("expected azure v1 /openai/v1 base not to duplicate realtime path, got %q", got)
	}

	azureGateway := CreateOpenAIProvider(&model.Channel{Key: "azure-key", Type: config.ChannelTypeAzureV1, Proxy: &proxy}, "https://resource.openai.azure.com/gateway")
	azureGateway.IsAzure = true
	got, errWithCode = azureGateway.realtimeWSURL("gpt-4o")
	if errWithCode != nil {
		t.Fatalf("expected azure v1 gateway realtime websocket URL, got %v", errWithCode)
	}
	if got != "wss://resource.openai.azure.com/gateway/openai/v1/realtime?model=gpt-4o" {
		t.Fatalf("expected azure v1 gateway prefix to be preserved, got %q", got)
	}

	azureGatewayOpenAIBase := CreateOpenAIProvider(&model.Channel{Key: "azure-key", Type: config.ChannelTypeAzureV1, Proxy: &proxy}, "https://resource.openai.azure.com/gateway/openai/v1")
	azureGatewayOpenAIBase.IsAzure = true
	got, errWithCode = azureGatewayOpenAIBase.realtimeWSURL("gpt-4o")
	if errWithCode != nil {
		t.Fatalf("expected azure v1 gateway /openai/v1 base realtime websocket URL, got %v", errWithCode)
	}
	if got != "wss://resource.openai.azure.com/gateway/openai/v1/realtime?model=gpt-4o" {
		t.Fatalf("expected azure v1 gateway /openai/v1 base not to duplicate realtime path, got %q", got)
	}

	deployment := CreateOpenAIProvider(&model.Channel{Key: "azure-key", Type: config.ChannelTypeAzureV1, Proxy: &proxy}, "https://resource.openai.azure.com/openai/deployments/gpt-4o")
	deployment.IsAzure = true
	got, errWithCode = deployment.realtimeWSURL("gpt-4o")
	if errWithCode == nil || errWithCode.Code != "invalid_azure_realtime_base_url" || got != "" {
		t.Fatalf("expected deployment-path azure v1 base URL to fail locally, url=%q err=%+v", got, errWithCode)
	}
}

func TestOpenAIAzureHTTPURLConstructionUsesJSONOtherAPIVersion(t *testing.T) {
	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{
		Key:   "azure-key",
		Type:  config.ChannelTypeAzure,
		Other: `{"api_version":"2024-10-01-preview"}`,
		Proxy: &proxy,
	}, "https://resource.openai.azure.com")
	provider.IsAzure = true

	if got := provider.GetFullRequestURL("/chat/completions", "gpt-4o"); got != "https://resource.openai.azure.com/openai/deployments/gpt-4o/chat/completions?api-version=2024-10-01-preview" {
		t.Fatalf("expected Azure deployment HTTP URL to use JSON api_version, got %q", got)
	}
	if got := provider.GetFullRequestURL("/v1/responses", ""); got != "https://resource.openai.azure.com/openai/responses?api-version=2024-10-01-preview" {
		t.Fatalf("expected Azure resource HTTP URL to use JSON api_version, got %q", got)
	}
	if got := provider.GetFullRequestURL("/v1/responses", "gpt-5"); got != "https://resource.openai.azure.com/openai/responses?api-version=2024-10-01-preview" {
		t.Fatalf("expected Azure Responses HTTP URL with model to stay resource-level, got %q", got)
	}
	if got := provider.GetFullRequestURL("/v1/responses/compact", "gpt-5"); got != "https://resource.openai.azure.com/openai/responses/compact?api-version=2024-10-01-preview" {
		t.Fatalf("expected Azure compact Responses HTTP URL with model to stay resource-level, got %q", got)
	}
	if got := provider.GetFullRequestURL("/responses", "gpt-5"); got != "https://resource.openai.azure.com/openai/responses?api-version=2024-10-01-preview" {
		t.Fatalf("expected normalized Azure Responses HTTP path with model to stay resource-level, got %q", got)
	}

	azureV1 := CreateOpenAIProvider(&model.Channel{
		Key:   "azure-key",
		Type:  config.ChannelTypeAzureV1,
		Proxy: &proxy,
	}, "https://resource.openai.azure.com")
	azureV1.IsAzure = true
	if got := azureV1.GetFullRequestURL("/v1/chat/completions", "gpt-4o"); got != "https://resource.openai.azure.com/openai/v1/chat/completions" {
		t.Fatalf("expected Azure V1 HTTP URL to use resource-level v1 path without api-version, got %q", got)
	}
	if got := azureV1.GetFullRequestURL("/v1/responses", ""); got != "https://resource.openai.azure.com/openai/v1/responses" {
		t.Fatalf("expected Azure V1 Responses HTTP URL to use resource-level v1 path, got %q", got)
	}
	if got := azureV1.GetFullRequestURL("/v1/responses", "gpt-5"); got != "https://resource.openai.azure.com/openai/v1/responses" {
		t.Fatalf("expected Azure V1 Responses HTTP URL with model to stay resource-level v1 path, got %q", got)
	}

	azureV1OpenAIBase := CreateOpenAIProvider(&model.Channel{
		Key:   "azure-key",
		Type:  config.ChannelTypeAzureV1,
		Proxy: &proxy,
	}, "https://resource.openai.azure.com/openai/v1/")
	azureV1OpenAIBase.IsAzure = true
	if got := azureV1OpenAIBase.GetFullRequestURL("/v1/responses", ""); got != "https://resource.openai.azure.com/openai/v1/responses" {
		t.Fatalf("expected Azure V1 /openai/v1 base not to duplicate HTTP path, got %q", got)
	}

	azureV1Gateway := CreateOpenAIProvider(&model.Channel{
		Key:   "azure-key",
		Type:  config.ChannelTypeAzureV1,
		Proxy: &proxy,
	}, "https://resource.openai.azure.com/gateway")
	azureV1Gateway.IsAzure = true
	if got := azureV1Gateway.GetFullRequestURL("/v1/responses", ""); got != "https://resource.openai.azure.com/gateway/openai/v1/responses" {
		t.Fatalf("expected Azure V1 gateway prefix to be preserved, got %q", got)
	}
	azureV1GatewayOpenAIBase := CreateOpenAIProvider(&model.Channel{
		Key:   "azure-key",
		Type:  config.ChannelTypeAzureV1,
		Proxy: &proxy,
	}, "https://resource.openai.azure.com/gateway/openai/v1")
	azureV1GatewayOpenAIBase.IsAzure = true
	if got := azureV1GatewayOpenAIBase.GetFullRequestURL("/v1/responses", ""); got != "https://resource.openai.azure.com/gateway/openai/v1/responses" {
		t.Fatalf("expected Azure V1 gateway /openai/v1 base not to duplicate HTTP path, got %q", got)
	}
}

func TestOpenAIRealtimeSelfHostedDialOptionsStillBlockMetadataIP(t *testing.T) {
	_, err := wsconn.DialManaged(context.Background(), "ws://169.254.169.254/v1/realtime", nil, wsconn.Config{},
		openAIRealtimeDialOptions("", true, nil)...,
	)
	if !errors.Is(err, wsconn.ErrPrivateAddrBlocked) {
		t.Fatalf("expected self-hosted dial options to block metadata IP, got %v", err)
	}
}

func TestMapOpenAIResponsesWSDialErrorPreservesHandshakeStatus(t *testing.T) {
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	cases := []struct {
		name       string
		statusCode int
		wantCode   string
		wantStatus int
	}{
		{"unauthorized", http.StatusUnauthorized, "provider_authentication_failed", http.StatusUnauthorized},
		{"rate limited", http.StatusTooManyRequests, "provider_rate_limit_exceeded", http.StatusTooManyRequests},
		{"not found unsupported", http.StatusNotFound, "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired},
		{"upgrade required unsupported", http.StatusUpgradeRequired, "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired},
		{"server error", http.StatusBadGateway, "provider_ws_request_failed", http.StatusBadGateway},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errWithCode := mapOpenAIResponsesWSDialError(&wsconn.DialError{
				URL:        "wss://provider.example/v1/responses?api_key=secret",
				StatusCode: tc.statusCode,
				Header:     http.Header{"Retry-After": []string{"2"}},
				Err:        errors.New("handshake failed"),
			})
			if errWithCode == nil {
				t.Fatalf("expected mapped error")
			}
			gotCode, _ := errWithCode.Code.(string)
			if gotCode != tc.wantCode || errWithCode.StatusCode != tc.wantStatus {
				t.Fatalf("expected %s/%d, got code=%v status=%d", tc.wantCode, tc.wantStatus, errWithCode.Code, errWithCode.StatusCode)
			}
			if strings.Contains(errWithCode.Message, "provider.example") || strings.Contains(errWithCode.Message, "secret") {
				t.Fatalf("expected mapped client message to omit upstream URL, got %q", errWithCode.Message)
			}
		})
	}

	errWithCode := mapOpenAIResponsesWSDialError(errors.New("dial tcp: no route"))
	if errWithCode == nil {
		t.Fatalf("expected mapped transport error")
	}
	gotCode, _ := errWithCode.Code.(string)
	if gotCode != "ws_request_failed" || errWithCode.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected transport errors without HTTP status to remain ws_request_failed, got %+v", errWithCode)
	}
}

func TestMapOpenAIResponsesWSDialErrorDoesNotLogSecrets(t *testing.T) {
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	_ = mapOpenAIResponsesWSDialError(errors.New("dial failed with Authorization: Bearer sk-responses-ws-secret"))

	entries, err := logger.GetLatestLogs(5)
	if err != nil {
		t.Fatalf("read latest logs: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Message, "sk-responses-ws-secret") ||
			strings.Contains(entry.Message, "Authorization") ||
			strings.Contains(entry.Message, "Bearer") {
			t.Fatalf("expected responses websocket dial log to omit auth material, got %q", entry.Message)
		}
	}
	if got := openAIResponsesWSDialErrorSummary(context.DeadlineExceeded); !strings.Contains(got, "category=context_deadline_exceeded") {
		t.Fatalf("expected safe dial summary to classify context deadline, got %q", got)
	}
	if got := openAIResponsesWSDialErrorSummary(errors.New("Authorization: Bearer sk-secret")); strings.Contains(got, "Authorization") || strings.Contains(got, "Bearer") || strings.Contains(got, "sk-secret") {
		t.Fatalf("expected safe dial summary to omit raw error text, got %q", got)
	}
}

func TestOpenAIRealtimeSessionAdditionalHelperBranches(t *testing.T) {
	recorder := &recordingOpenAIRealtimeObserver{}
	now := time.Now()

	session := newOpenAIRealtimeHelperSession()
	session.turn = newOpenAIRealtimeTurnState(1, now, nil)
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })
	if session.turn.observer == nil {
		t.Fatal("expected SetTurnObserverFactory to attach an observer to the active turn")
	}

	if outbound, shouldClose := session.observeSupplierMessage(wsconn.BinaryMessage, []byte{1, 2, 3}); shouldClose || outbound.err != nil || string(outbound.payload) != string([]byte{1, 2, 3}) {
		t.Fatalf("expected binary realtime supplier payload passthrough, outbound=%+v should_close=%v", outbound, shouldClose)
	}
	if outbound, shouldClose := session.observeSupplierMessage(wsconn.TextMessage, []byte("not-json")); shouldClose || outbound.err != nil || string(outbound.payload) != "not-json" {
		t.Fatalf("expected invalid json supplier payload passthrough, outbound=%+v should_close=%v", outbound, shouldClose)
	}

	session.compatMode = true
	if outbound, shouldClose := session.observeSupplierMessage(wsconn.TextMessage, []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"bad_request","message":"boom"}}`)); shouldClose || outbound.err != nil {
		t.Fatalf("expected non-fatal compat mode upstream errors to pass through, outbound=%+v should_close=%v", outbound, shouldClose)
	}
	if outbound, shouldClose := session.observeSupplierMessage(wsconn.TextMessage, []byte(`{"type":"error","error":{"type":"server_error","code":"session_expired","message":"boom"}}`)); shouldClose || outbound.err != nil {
		t.Fatalf("expected compat mode upstream errors to pass through without closing the session, outbound=%+v should_close=%v", outbound, shouldClose)
	}

	session.compatMode = false
	session.turn = nil
	session.pendingTurns = nil
	session.recentFinalizedIDs = nil
	outbound, shouldClose := session.observeSupplierMessage(wsconn.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_orphan","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`))
	if shouldClose || outbound.usage == nil || outbound.usage.TotalTokens != 3 {
		t.Fatalf("expected orphan terminal usage to be forwarded without closing, outbound=%+v should_close=%v", outbound, shouldClose)
	}

	session.recentFinalizedIDs = []string{"resp_orphan"}
	outbound, shouldClose = session.observeSupplierMessage(wsconn.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_orphan","status":"completed","usage":{"input_tokens":5,"output_tokens":5,"total_tokens":10}}}`))
	if shouldClose || outbound.usage != nil {
		t.Fatalf("expected late finalized response usage to be dropped, outbound=%+v should_close=%v", outbound, shouldClose)
	}

	pendingRecorder := &recordingOpenAIRealtimeObserver{}
	startSession := newOpenAIRealtimeHelperSession()
	startSession.pendingTurns = []openAIRealtimePendingTurn{
		{state: newOpenAIRealtimeTurnState(2, now, pendingRecorder), reason: "stale_pending"},
	}
	startSession.turnObserverFactory = func() runtimesession.TurnObserver { return recorder }
	startedTurn, finalized, err := startSession.startTurn()
	if err != nil {
		t.Fatalf("expected helper startTurn to succeed, got %v", err)
	}
	if startedTurn == nil || startedTurn.observer == nil {
		t.Fatalf("expected startTurn to attach a guarded observer, got %+v", startedTurn)
	}
	if len(finalized) != 1 {
		t.Fatalf("expected startTurn to finalize stale pending turns, got %d", len(finalized))
	}
	runOpenAIRealtimeFinalizers(finalized)
	if pendingRecorder.finalizeCount() != 1 {
		t.Fatalf("expected pending turn finalizer to run once, got %d", pendingRecorder.finalizeCount())
	}

	if observer, payload := (&openAIRealtimeSession{}).finalizeTurn("ignored", now); observer != nil || payload.TurnSeq != 0 {
		t.Fatalf("expected finalizeTurn without an active turn to no-op, observer=%+v payload=%+v", observer, payload)
	}

	pendingState := newOpenAIRealtimeTurnState(3, now, nil)
	pendingState.rememberResponseID("resp_pending")
	startSession.recentFinalizedIDs = nil
	if finalized := startSession.finalizePendingTurn(openAIRealtimePendingTurn{state: pendingState}, "default_reason", now); len(finalized) != 0 {
		t.Fatalf("expected pending turns without observers not to emit finalizers, got %+v", finalized)
	}
	if !startSession.isRecentlyFinalizedResponseIDLocked("resp_pending") {
		t.Fatal("expected finalizePendingTurn to still remember finalized response ids")
	}

	timerSession := newOpenAIRealtimeHelperSession()
	timerSession.startDetachTimer()
	firstTimer := timerSession.detachTimer
	timerSession.startDetachTimer()
	if firstTimer == nil || timerSession.detachTimer != firstTimer {
		t.Fatalf("expected repeated startDetachTimer calls to reuse the same timer, first=%v current=%v", firstTimer, timerSession.detachTimer)
	}
	timerSession.stopDetachTimer()
	timerSession.stopDetachTimer()
}
