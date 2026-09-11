package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
	"one-api/model"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func openAIResponsesWSTestSession(provider *OpenAIProvider, ctx context.Context, model string, req responsesws.OpenRequest) (responsesws.Upstream, *types.OpenAIErrorWithStatusCode) {
	req.SelectedModel = model
	return provider.OpenResponsesWS(ctx, &req)
}

func TestOpenAIRealtimePumpContextPreservesRequestValuesWithoutCancel(t *testing.T) {
	base, cancel := context.WithCancel(context.WithValue(context.Background(), logger.RequestIdKey, "req-openai-pump"))
	pumpCtx := openAIRealtimePumpContext(base)
	cancel()

	if got := pumpCtx.Value(logger.RequestIdKey); got != "req-openai-pump" {
		t.Fatalf("expected request id to be preserved, got %v", got)
	}
	select {
	case <-pumpCtx.Done():
		t.Fatal("expected pump context to ignore request cancellation")
	default:
	}
}

func newOpenAIRealtimeHelperSession() *openAIRealtimeSession {
	return &openAIRealtimeSession{
		model:                       "gpt-4o-realtime-preview",
		sessionID:                   "sess_helper",
		recvCh:                      make(chan openAIRealtimeOutbound, 8),
		closed:                      make(chan struct{}),
		detached:                    make(chan struct{}),
		outboundBackpressureTimeout: openAIRealtimeOutboundBackpressureTimeout,
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
	if usage := openAIRealtimeResponseUsage("evt_ignored", nil, nil); usage != nil {
		t.Fatalf("expected nil response usage to stay nil, got %+v", usage)
	}
	responseEvent := &types.ResponseEvent{ID: " resp_usage ", Usage: &types.UsageEvent{TotalTokens: 9}}
	if usage := openAIRealtimeResponseUsage(" evt_usage ", responseEvent, nil); usage == nil || usage.TotalTokens != 9 ||
		usage.Source != types.UsageSourceRealtimeResponse ||
		usage.BillingBasis != types.UsageBillingBasisTokens ||
		usage.ProviderEventID != "evt_usage" ||
		usage.ResponseID != "resp_usage" {
		t.Fatalf("expected response usage passthrough, got %+v", usage)
	}

	tokenUsagePayload := []byte(`{"event_id":" evt_transcript_tokens ","type":"conversation.item.input_audio_transcription.completed","item_id":" item_1 ","usage":{"type":"tokens","input_tokens":7,"output_tokens":3,"total_tokens":10,"input_token_details":{"audio_tokens":7}}}`)
	tokenUsage := openAIRealtimeInputAudioTranscriptionUsage("conversation.item.input_audio_transcription.completed", " evt_override ", tokenUsagePayload)
	if tokenUsage == nil ||
		tokenUsage.Source != types.UsageSourceInputAudioTranscription ||
		tokenUsage.BillingBasis != types.UsageBillingBasisTokens ||
		tokenUsage.ProviderEventID != "evt_override" ||
		tokenUsage.ItemID != "item_1" ||
		tokenUsage.InputTokens != 7 ||
		tokenUsage.OutputTokens != 3 ||
		tokenUsage.TotalTokens != 10 ||
		tokenUsage.InputTokenDetails.AudioTokens != 7 ||
		tokenUsage.DurationSeconds != 0 {
		t.Fatalf("expected token transcription usage attribution, got %+v", tokenUsage)
	}

	durationUsagePayload := []byte(`{"event_id":" evt_transcript_duration ","type":"conversation.item.input_audio_transcription.completed","item_id":" item_2 ","usage":{"type":"duration","seconds":2.5}}`)
	durationUsage := openAIRealtimeInputAudioTranscriptionUsage("conversation.item.input_audio_transcription.completed", "", durationUsagePayload)
	if durationUsage == nil ||
		durationUsage.Source != types.UsageSourceInputAudioTranscription ||
		durationUsage.BillingBasis != types.UsageBillingBasisDuration ||
		durationUsage.ProviderEventID != "evt_transcript_duration" ||
		durationUsage.ItemID != "item_2" ||
		durationUsage.DurationSeconds != 2.5 ||
		durationUsage.ProviderOperationUnits == nil || *durationUsage.ProviderOperationUnits != 1 {
		t.Fatalf("expected duration transcription usage attribution, got %+v", durationUsage)
	}
}

func TestOpenAIRealtimeUsageUsesSessionCreatedModel(t *testing.T) {
	session := &openAIRealtimeSession{model: "requested-alias"}
	created := []byte(`{"type":"session.created","session":{"id":"sess_1","model":"gpt-realtime-2026-08-01"}}`)
	if outbound, shouldClose := session.observeSupplierMessage(wsconn.TextMessage, created); shouldClose || outbound.usage != nil {
		t.Fatalf("expected session bootstrap without usage, got close=%v outbound=%+v", shouldClose, outbound)
	}

	done := []byte(`{"event_id":"evt_done","type":"response.done","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`)
	outbound, shouldClose := session.observeSupplierMessage(wsconn.TextMessage, done)
	if shouldClose || outbound.usage == nil {
		t.Fatalf("expected terminal usage, got close=%v outbound=%+v", shouldClose, outbound)
	}
	if outbound.usage.ResponseModel != "gpt-realtime-2026-08-01" {
		t.Fatalf("expected session-created actual model attribution, got %+v", outbound.usage)
	}
}

func TestOpenAIRealtimeReadLoopWithNilConnClosesSession(t *testing.T) {
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
	session, errWithCode := openAIResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{})
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

func TestOpenAIResponsesWSUnsupportedWhenResponsesEndpointMissing(t *testing.T) {
	provider := newOpenAIRealtimeTestProvider("http://127.0.0.1:1")
	provider.Config.Responses = ""

	session, errWithCode := openAIResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{})
	if session != nil {
		session.Abort("test_cleanup")
	}
	if errWithCode == nil || errWithCode.StatusCode != http.StatusUpgradeRequired || errWithCode.Code != "responses_ws_unsupported_for_channel" {
		t.Fatalf("expected missing Responses endpoint to return responses_ws_unsupported_for_channel, session=%T err=%+v", session, errWithCode)
	}
}

func TestOpenAIResponsesWSCustomNativeRequiresExplicitCapability(t *testing.T) {
	proxy := ""
	disabled := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(),
		Key:   "sk-test",
		Type:  config.ChannelTypeCustom,
		Other: `{"responses_ws_self_hosted":true}`,
		Proxy: &proxy,
	}, "http://127.0.0.1:1")
	session, errWithCode := openAIResponsesWSTestSession(disabled, context.Background(), "gpt-5", responsesws.OpenRequest{})
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

	enabled := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(),
		Key:   "sk-test",
		Type:  config.ChannelTypeCustom,
		Other: `{"responses_ws_native":true,"responses_ws_self_hosted":true}`,
		Proxy: &proxy,
	}, server.URL)
	session, errWithCode = openAIResponsesWSTestSession(enabled, context.Background(), "gpt-5", responsesws.OpenRequest{})
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
	session, errWithCode := openAIResponsesWSTestSession(disabled, context.Background(), "gpt-5", responsesws.OpenRequest{})
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
	session, errWithCode = openAIResponsesWSTestSession(enabled, context.Background(), "gpt-5", responsesws.OpenRequest{})
	if errWithCode != nil {
		close(releaseDone)
		t.Fatalf("expected explicit native capability to allow OpenAI type custom base URL, got %v", errWithCode)
	}
	close(releaseDone)
	session.Abort("test_cleanup")
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
	session, errWithCode := openAIResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{})
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
	session, errWithCode := openAIResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{})
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
	session, errWithCode := openAIResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{})
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
	session, errWithCode := openAIResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{})
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

func TestOpenAIResponsesWSKnownTerminalOpaqueResponsePreservesWire(t *testing.T) {
	server := newOpenAIRealtimeTestServer(t, func(conn *openAIRealtimeTestConn) {
		if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.completed","response":"opaque"}`)); err != nil {
			t.Errorf("failed to write bad terminal provider event: %v", err)
		}
	})
	defer server.Close()

	provider := newOpenAIRealtimeTestProvider(server.URL)
	session, errWithCode := openAIResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{})
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
	if event.DetailOrigin != responsesws.RecvDetailOriginProviderFrame || event.Err != nil || event.Frame == nil || string(event.Frame.Payload()) != `{"type":"response.completed","response":"opaque"}` {
		t.Fatalf("无法投影的 terminal 应保持原帧, got %+v", event)
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
	session, errWithCode := openAIResponsesWSTestSession(provider, context.Background(), "gpt-5", responsesws.OpenRequest{})
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

func TestOpenAIResponsesWSInterpretsOnlyResponsesUsageEvents(t *testing.T) {
	adapter := openAIResponsesWSAdapter{}
	terminalPayload := []byte(`{"type":"response.completed","event_id":"evt_responses_usage","sequence_number":0,"response":{"id":"resp_usage","status":"completed","model":"gpt-5.6","service_tier":"priority","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}}}`)
	terminal := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame(terminalPayload))
	if terminal.Err != nil || terminal.EmitFrame == nil || string(terminal.EmitFrame.Payload()) != string(terminalPayload) {
		t.Fatalf("expected Responses terminal to pass through unchanged, got %+v", terminal)
	}
	if terminal.Usage == nil || !terminal.Usage.ProviderTokenEvidence || terminal.Usage.Source != types.UsageSourceResponsesResponse || terminal.Usage.ProviderEventID != "evt_responses_usage" || terminal.Usage.ResponseID != "resp_usage" || terminal.Usage.TotalTokens != 8 || terminal.Usage.InputTokenDetails.CachedTokens != 2 || terminal.Usage.OutputTokenDetails.ReasoningTokens != 1 {
		t.Fatalf("expected Responses DTO usage evidence, got %+v", terminal.Usage)
	}

	transcriptionPayload := []byte(`{"type":"conversation.item.input_audio_transcription.completed","event_id":"evt_transcription","item_id":"item_1","usage":{"input_tokens":7,"output_tokens":0,"total_tokens":7}}`)
	transcription := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame(transcriptionPayload))
	if transcription.Err != nil || transcription.EmitFrame == nil || string(transcription.EmitFrame.Payload()) != string(transcriptionPayload) {
		t.Fatalf("expected unknown Realtime-only event to remain passthrough, got %+v", transcription)
	}
	if transcription.Usage != nil {
		t.Fatalf("Responses adapter must not interpret Realtime transcription usage, got %+v", transcription.Usage)
	}
}

func TestOpenAIResponsesWSRequiresCompleteProviderTokenPartition(t *testing.T) {
	adapter := openAIResponsesWSAdapter{}
	for name, payload := range map[string][]byte{
		"missing output": []byte(`{"type":"response.completed","sequence_number":0,"response":{"id":"resp_partial","status":"completed","usage":{"input_tokens":3,"total_tokens":3}}}`),
		"null output":    []byte(`{"type":"response.completed","sequence_number":0,"response":{"id":"resp_partial","status":"completed","usage":{"input_tokens":3,"output_tokens":null,"total_tokens":3}}}`),
	} {
		t.Run(name, func(t *testing.T) {
			result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame(payload))
			if result.Err != nil || result.Usage == nil {
				t.Fatalf("partial usage observation was not preserved: %+v", result)
			}
			if result.Usage.ProviderTokenEvidence {
				t.Fatalf("partial provider partition was authorized: %+v", result.Usage)
			}
		})
	}
}

func TestOpenAIRealtimeRequiresCompleteProviderTokenPartition(t *testing.T) {
	for name, payload := range map[string][]byte{
		"complete":        []byte(`{"type":"response.done","response":{"id":"resp_usage","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`),
		"missing output":  []byte(`{"type":"response.done","response":{"id":"resp_usage","usage":{"input_tokens":3,"total_tokens":3}}}`),
		"null output":     []byte(`{"type":"response.done","response":{"id":"resp_usage","usage":{"input_tokens":3,"output_tokens":null,"total_tokens":3}}}`),
		"negative output": []byte(`{"type":"response.done","response":{"id":"resp_usage","usage":{"input_tokens":3,"output_tokens":-1,"total_tokens":2}}}`),
	} {
		t.Run(name, func(t *testing.T) {
			var event types.Event
			if err := json.Unmarshal(payload, &event); err != nil {
				t.Fatal(err)
			}
			usage := openAIRealtimeResponseUsage("evt_usage", event.Response, payload)
			if usage == nil {
				t.Fatal("provider usage observation was lost")
			}
			want := name == "complete"
			if usage.ProviderTokenEvidence != want {
				t.Fatalf("provider token evidence=%v want %v: %+v", usage.ProviderTokenEvidence, want, usage)
			}
		})
	}
}

func TestOpenAIRealtimeExplicitTurnAdmissionRequiresAutomaticWorkDisabled(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		want    bool
		present bool
	}{
		{name: "top-level null", payload: `{"type":"session.update","session":{"turn_detection":null}}`, want: true, present: true},
		{name: "top-level create disabled", payload: `{"type":"session.update","session":{"turn_detection":{"type":"server_vad","create_response":false}}}`, want: true, present: true},
		{name: "nested create disabled", payload: `{"type":"session.update","session":{"audio":{"input":{"turn_detection":{"type":"semantic_vad","create_response":false}}}}}`, want: true, present: true},
		{name: "automatic create enabled", payload: `{"type":"session.update","session":{"turn_detection":{"create_response":true}}}`, want: false, present: true},
		{name: "unrelated update", payload: `{"type":"session.update","session":{"voice":"alloy"}}`, present: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, present := openAIRealtimeAutomaticFeaturesDisabled([]byte(test.payload))
			if got != test.want || present != test.present {
				t.Fatalf("automatic feature state=(%v,%v), want (%v,%v)", got, present, test.want, test.present)
			}
		})
	}

	payload := []byte(`{"type":"response.create","response":{"model":"gpt-realtime-selected","max_output_tokens":512,"service_tier":"priority","tools":[{"type":"function"}]}}`)
	admission := openAIRealtimeBoundedTurnAdmission(payload, runtimesession.ModelBinding{RequestedModel: "voice-public", ProviderModel: "gpt-realtime-default", BillingModel: "voice-public"}, true)
	if !admission.ExplicitClientCreate || !admission.AutomaticFeaturesDisabled || admission.Models.RequestedModel != "voice-public" || admission.Models.ProviderModel != "gpt-realtime-default" || admission.MaxOutputTokens != 512 || admission.ServiceTier != "priority" || admission.UnknownChargeDimensions {
		t.Fatalf("bounded turn admission lost billing dimensions: %+v", admission)
	}
	unknown := openAIRealtimeBoundedTurnAdmission([]byte(`{"type":"response.create","response":{"tools":[{"type":"web_search"}]}}`), runtimesession.ModelBinding{RequestedModel: "gpt-realtime", ProviderModel: "gpt-realtime", BillingModel: "gpt-realtime"}, true)
	if !unknown.UnknownChargeDimensions {
		t.Fatalf("unknown realtime charge dimension was accepted: %+v", unknown)
	}
}

func TestOpenAIRealtimeConnEnforcesUpstreamReadLimit(t *testing.T) {
	const limit = int64(64)
	const oversizedPayloadBytes = 512

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
		ReadLimit:    limit,
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
	session.recentFinalizedIDs = []string{"resp-finalized"}

	if selected := session.selectSupplierTurnLocked(""); selected.state != session.turn || selected.dropAttribution {
		t.Fatalf("expected empty response id to prefer active turn, got %+v", selected)
	}
	if selected := session.selectSupplierTurnLocked("resp-active"); selected.state != session.turn || selected.dropAttribution {
		t.Fatalf("expected active response id lookup to return current turn, got %+v", selected)
	}
	if selected := session.selectSupplierTurnLocked("resp-finalized"); !selected.dropAttribution || selected.state != nil {
		t.Fatalf("expected finalized response id lookup to drop attribution, got %+v", selected)
	}
	session.turn.rememberResponseID("resp-finalize-current")
	finalizedCurrent := session.finalizeObservedTurnState(session.turn, "response.done", now)
	if len(finalizedCurrent) != 1 || session.turn != nil {
		t.Fatalf("expected current turn finalization to produce one finalizer, finalized=%d turn=%+v", len(finalizedCurrent), session.turn)
	}
	session.runFinalizers(finalizedCurrent)

	session.rememberFinalizedResponseIDsLocked("", "dup", "dup")
	for i := 0; i < openAIRealtimeFinalizedResponseIDLimit+2; i++ {
		session.rememberFinalizedResponseIDsLocked(fmt.Sprintf("resp-limit-%d", i))
	}
	if len(session.recentFinalizedIDs) != openAIRealtimeFinalizedResponseIDLimit {
		t.Fatalf("expected finalized response id history cap %d, got %d", openAIRealtimeFinalizedResponseIDLimit, len(session.recentFinalizedIDs))
	}
	if !session.isRecentlyFinalizedResponseIDLocked(fmt.Sprintf("resp-limit-%d", openAIRealtimeFinalizedResponseIDLimit+1)) {
		t.Fatal("expected newest finalized response id to be remembered")
	}
	if session.isRecentlyFinalizedResponseIDLocked("dup") || session.isRecentlyFinalizedResponseIDLocked("resp-limit-0") {
		t.Fatal("expected oldest finalized response ids to be evicted after limit overflow")
	}
	if session.isRecentlyFinalizedResponseIDLocked("") {
		t.Fatal("expected blank finalized response id lookup to return false")
	}
	oversizedID := strings.Repeat("r", openAIRealtimeIdentifierMaxBytes+1)
	session.rememberFinalizedResponseIDsLocked(oversizedID)
	if len(session.recentFinalizedIDs) != openAIRealtimeFinalizedResponseIDLimit || session.isRecentlyFinalizedResponseIDLocked(oversizedID) {
		t.Fatal("expected oversized finalized response id to be ignored without disturbing the bounded history")
	}
	if recorder.finalizeCount() != 1 {
		t.Fatalf("expected active helper finalizer to run once, got %d", recorder.finalizeCount())
	}
}

func TestOpenAIRealtimeFinalizedHistoryCoversEveryIdentityFromPreviousTurn(t *testing.T) {
	session := newOpenAIRealtimeHelperSession()
	previous := newOpenAIRealtimeTurnState(1, time.Now(), &recordingOpenAIRealtimeObserver{})
	for i := 0; i < openAIRealtimeResponseIDLimit; i++ {
		if err := previous.rememberResponseID(fmt.Sprintf("resp-previous-%d", i)); err != nil {
			t.Fatalf("remember previous response %d: %v", i, err)
		}
	}
	session.turn = previous
	session.runFinalizers(session.finalizeObservedTurnState(previous, types.EventTypeResponseDone, time.Now()))
	if len(session.recentFinalizedIDs) != openAIRealtimeResponseIDLimit {
		t.Fatalf("finalized history count=%d, want full previous-turn capacity %d", len(session.recentFinalizedIDs), openAIRealtimeResponseIDLimit)
	}

	current := newOpenAIRealtimeTurnState(2, time.Now(), &recordingOpenAIRealtimeObserver{})
	if err := current.rememberResponseID("resp-current"); err != nil {
		t.Fatalf("remember current response: %v", err)
	}
	session.turn = current
	selected := session.selectSupplierTurnLocked("resp-previous-0")
	if !selected.dropAttribution || selected.state != nil {
		t.Fatalf("oldest identity from immediately previous full turn must not be attributed to current turn: %+v", selected)
	}
}

func TestOpenAIRealtimeSessionRejectsOversizedClientEventIDBeforeStartingTurn(t *testing.T) {
	conn, cleanup := newOpenAIRealtimeConnPair(t)
	defer cleanup()

	session := newOpenAIRealtimeHelperSession()
	session.conn = conn
	session.turnObserverFactory = func() runtimesession.TurnObserver { return &recordingOpenAIRealtimeObserver{} }
	clientEventID := strings.Repeat("e", openAIRealtimeIdentifierMaxBytes+1)
	payload := []byte(fmt.Sprintf(`{"type":"response.create","event_id":%q,"response":{"input":[]}}`, clientEventID))

	err := session.SendClient(context.Background(), openAITestTextFrame(payload))
	var event *types.Event
	if !errors.As(err, &event) || event.ErrorDetail == nil || event.ErrorDetail.Code != "invalid_event" {
		t.Fatalf("oversized client event id error=%v, want invalid_event", err)
	}
	if session.turn != nil || session.turnSeq != 0 {
		t.Fatalf("oversized client event id must not start provider work: turn=%+v seq=%d", session.turn, session.turnSeq)
	}
}

func TestOpenAIRealtimeSessionUsageStateOverflowReplacesProviderFrameAndFinalizesPrefix(t *testing.T) {
	recorder := &recordingOpenAIRealtimeObserver{}
	session := newOpenAIRealtimeHelperSession()
	session.turn = newOpenAIRealtimeTurnState(1, time.Now(), recorder)

	firstPayload := []byte(`{"type":"response.created","response":{"id":"resp-0","status":"in_progress","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)
	first, shouldClose := session.observeSupplierMessage(wsconn.TextMessage, firstPayload)
	if shouldClose || first.usage == nil || first.usage.TotalTokens != 3 || recorder.observeCount() != 1 {
		t.Fatalf("expected accepted prefix usage before overflow: outbound=%+v close=%v observed=%d", first, shouldClose, recorder.observeCount())
	}
	for i := 1; i < openAIRealtimeResponseIDLimit; i++ {
		if err := session.turn.rememberResponseID(fmt.Sprintf("resp-%d", i)); err != nil {
			t.Fatalf("seed response identity %d: %v", i, err)
		}
	}

	overflowPayload := []byte(`{"type":"response.done","response":{"id":"resp-overflow","status":"completed","usage":{"input_tokens":4,"output_tokens":5,"total_tokens":9}}}`)
	outbound, shouldClose := session.observeSupplierMessage(wsconn.TextMessage, overflowPayload)
	if !shouldClose || outbound.origin != runtimerealtime.RealtimePayloadOriginProxyLocal || outbound.usage != nil {
		t.Fatalf("overflow must close with a local error and no current-frame usage: outbound=%+v close=%v", outbound, shouldClose)
	}
	if strings.Contains(string(outbound.payload), "resp-overflow") || string(outbound.payload) == string(overflowPayload) {
		t.Fatalf("overflowing provider frame must not be delivered: %s", outbound.payload)
	}
	var errorEnvelope struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(outbound.payload, &errorEnvelope); err != nil || errorEnvelope.Type != types.EventTypeError || errorEnvelope.Error.Type != "provider_error" || errorEnvelope.Error.Code != "provider_usage_state_limit" {
		t.Fatalf("unexpected realtime overflow error payload: payload=%s err=%v decoded=%+v", outbound.payload, err, errorEnvelope)
	}
	if session.turn != nil || recorder.observeCount() != 1 || recorder.finalizeCount() != 1 {
		t.Fatalf("overflow must finalize exactly the accepted prefix: turn=%+v observed=%d finalized=%d", session.turn, recorder.observeCount(), recorder.finalizeCount())
	}
	finalized := recorder.lastPayload()
	if finalized.TerminationReason != "provider_usage_state_limit" || finalized.Usage == nil || finalized.Usage.TotalTokens != 3 || finalized.LastResponseID == "resp-overflow" {
		t.Fatalf("overflow finalization must preserve only accepted prefix facts: %+v", finalized)
	}
}

func TestOpenAIRealtimeProviderFrameQueueStopsObservationAfterUsageStateOverflow(t *testing.T) {
	recorder := &recordingOpenAIRealtimeObserver{}
	session := newOpenAIRealtimeHelperSession()
	session.turn = newOpenAIRealtimeTurnState(1, time.Now(), recorder)
	for i := 0; i < openAIRealtimeResponseIDLimit-1; i++ {
		if err := session.turn.rememberResponseID(fmt.Sprintf("resp-seed-%d", i)); err != nil {
			t.Fatalf("seed response identity %d: %v", i, err)
		}
	}

	frames := make(chan openAIRealtimeProviderFrame, 3)
	frames <- openAIRealtimeProviderFrame{
		messageType: wsconn.TextMessage,
		payload:     []byte(`{"type":"response.created","response":{"id":"resp-prefix","status":"in_progress","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`),
	}
	frames <- openAIRealtimeProviderFrame{
		messageType: wsconn.TextMessage,
		payload:     []byte(`{"type":"response.done","response":{"id":"resp-overflow","status":"completed","usage":{"input_tokens":4,"output_tokens":5,"total_tokens":9}}}`),
	}
	frames <- openAIRealtimeProviderFrame{
		messageType: wsconn.TextMessage,
		payload:     []byte(`{"type":"response.done","response":{"id":"resp-after-limit","status":"completed","usage":{"input_tokens":50,"output_tokens":50,"total_tokens":100}}}`),
	}
	close(frames)

	session.consumeProviderFrames(frames)
	first, err, handled := session.recvQueuedOutbound()
	if !handled || err != nil || first.Frame == nil || first.Usage == nil || first.Usage.TotalTokens != 3 || !strings.Contains(string(first.Frame.Payload()), "resp-prefix") {
		t.Fatalf("expected accepted prefix frame first: event=%+v handled=%v err=%v", first, handled, err)
	}
	limitEvent, err, handled := session.recvQueuedOutbound()
	if !handled || err != nil || limitEvent.Frame == nil || limitEvent.Usage != nil || !strings.Contains(string(limitEvent.Frame.Payload()), "provider_usage_state_limit") {
		t.Fatalf("expected local state-limit event second: event=%+v handled=%v err=%v", limitEvent, handled, err)
	}
	select {
	case extra := <-session.recvCh:
		t.Fatalf("provider frame after hard limit boundary was still delivered: %+v", extra)
	default:
	}
	if recorder.observeCount() != 1 || recorder.finalizeCount() != 1 || session.turn != nil {
		t.Fatalf("frames after hard limit boundary must not be observed: observed=%d finalized=%d turn=%+v", recorder.observeCount(), recorder.finalizeCount(), session.turn)
	}
	finalized := recorder.lastPayload()
	if finalized.Usage == nil || finalized.Usage.TotalTokens != 3 || finalized.LastResponseID != "resp-prefix" {
		t.Fatalf("hard limit finalization included a queued follow-up frame: %+v", finalized)
	}
}

func TestOpenAIRealtimeUnknownTranscriptionDoesNotAffectResponseOwner(t *testing.T) {
	recorder := &recordingOpenAIRealtimeObserver{}
	session := newOpenAIRealtimeHelperSession()
	session.turn = newOpenAIRealtimeTurnState(1, time.Now(), recorder)
	payload := []byte(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"unknown-input","usage":{"input_tokens":7,"total_tokens":7},"response":{"id":"resp-unrelated"}}`)
	if outbound, close := session.observeSupplierMessage(wsconn.TextMessage, payload); close || outbound.err != nil {
		t.Fatalf("unknown input closed response: %+v", outbound)
	}
	if recorder.observeCount() != 0 || recorder.finalizeCount() != 0 || session.turn == nil || session.turn.lastResponseID != "" {
		t.Fatal("unowned input usage affected current response")
	}
}

func TestOpenAIRealtimeSessionUsageStateOverflowDoesNotCommitSessionModel(t *testing.T) {
	recorder := &recordingOpenAIRealtimeObserver{}
	session := newOpenAIRealtimeHelperSession()
	session.actualModel = "model-accepted"
	session.turn = newOpenAIRealtimeTurnState(1, time.Now(), recorder)

	prefixPayload := []byte(`{"type":"response.created","response":{"id":"resp-prefix","status":"in_progress","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)
	if outbound, shouldClose := session.observeSupplierMessage(wsconn.TextMessage, prefixPayload); shouldClose || outbound.usage == nil || outbound.usage.TotalTokens != 3 {
		t.Fatalf("expected accepted response prefix, outbound=%+v close=%v", outbound, shouldClose)
	}
	overflowID := strings.Repeat("r", openAIRealtimeIdentifierMaxBytes+1)
	overflowPayload := []byte(fmt.Sprintf(`{"type":"session.created","session":{"id":"session-1","model":"model-offending"},"response":{"id":%q}}`, overflowID))

	outbound, shouldClose := session.observeSupplierMessage(wsconn.TextMessage, overflowPayload)
	if !shouldClose || outbound.usage != nil || outbound.origin != runtimerealtime.RealtimePayloadOriginProxyLocal || !strings.Contains(string(outbound.payload), "provider_usage_state_limit") {
		t.Fatalf("offending session model frame must be replaced by local state-limit error: outbound=%+v close=%v", outbound, shouldClose)
	}
	if session.actualModel != "model-accepted" || session.turn != nil || recorder.observeCount() != 1 || recorder.finalizeCount() != 1 {
		t.Fatalf("offending session model mutated accepted state: model=%q turn=%+v observed=%d finalized=%d", session.actualModel, session.turn, recorder.observeCount(), recorder.finalizeCount())
	}
	finalized := recorder.lastPayload()
	if finalized.Models.ReportedModel != "model-accepted" || finalized.Models.BillingModel != session.model || finalized.Usage == nil || finalized.Usage.TotalTokens != 3 || finalized.LastResponseID != "resp-prefix" {
		t.Fatalf("offending session model changed finalized prefix: %+v", finalized)
	}
}

func TestOpenAIRealtimeLateFinalizedUsageIdentityDoesNotCloseCurrentTurn(t *testing.T) {
	recorder := &recordingOpenAIRealtimeObserver{}
	session := newOpenAIRealtimeHelperSession()
	current := newOpenAIRealtimeTurnState(2, time.Now(), recorder)
	if err := current.rememberResponseID("resp-current"); err != nil {
		t.Fatalf("remember current response: %v", err)
	}
	session.turn = current
	session.recentFinalizedIDs = []string{"resp-old"}
	providerEventID := strings.Repeat("e", openAIRealtimeIdentifierMaxBytes+1)
	payload := []byte(fmt.Sprintf(`{"event_id":%q,"type":"response.done","response":{"id":"resp-old","status":"completed","usage":{"input_tokens":4,"output_tokens":5,"total_tokens":9}}}`, providerEventID))

	outbound, shouldClose := session.observeSupplierMessage(wsconn.TextMessage, payload)
	if shouldClose || outbound.usage != nil || outbound.origin != runtimerealtime.RealtimePayloadOriginProvider || string(outbound.payload) != string(payload) {
		t.Fatalf("late finalized frame must pass through without attribution or local failure: outbound=%+v close=%v", outbound, shouldClose)
	}
	if session.turn != current || recorder.observeCount() != 0 || recorder.finalizeCount() != 0 {
		t.Fatalf("late finalized frame disturbed current turn: turn=%+v observed=%d finalized=%d", session.turn, recorder.observeCount(), recorder.finalizeCount())
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
	byteBoundedSession := newOpenAIRealtimeHelperSession()
	byteBoundedSession.outboundBudget = runtimerealtime.NewByteBudget(4)
	if handled := byteBoundedSession.enqueueOutbound(openAIRealtimeOutbound{messageType: wsconn.TextMessage, payload: []byte("four")}); !handled {
		t.Fatal("expected outbound within the session byte budget")
	}
	if got := byteBoundedSession.outboundBudget.Used(); got != 4 {
		t.Fatalf("expected queued outbound to own four bytes, got %d", got)
	}
	if _, err, handled := byteBoundedSession.recvQueuedOutbound(); !handled || err != nil {
		t.Fatalf("expected byte-budgeted outbound consumption, handled=%v err=%v", handled, err)
	}
	if got := byteBoundedSession.outboundBudget.Used(); got != 0 {
		t.Fatalf("outbound consumption leaked byte credit: %d", got)
	}
	oversizeSession := newOpenAIRealtimeHelperSession()
	oversizeSession.outboundBudget = runtimerealtime.NewByteBudget(3)
	if handled := oversizeSession.enqueueOutbound(openAIRealtimeOutbound{messageType: wsconn.TextMessage, payload: []byte("four")}); handled {
		t.Fatal("session admitted an outbound frame larger than its byte budget")
	}
	if got := oversizeSession.outboundBudget.Used(); got != 0 {
		t.Fatalf("oversize outbound leaked byte credit: %d", got)
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
		recvCh:                      make(chan openAIRealtimeOutbound, 1),
		closed:                      make(chan struct{}),
		detached:                    make(chan struct{}),
		outboundBackpressureTimeout: 10 * time.Millisecond,
	}
	backpressuredSession.recvCh <- openAIRealtimeOutbound{messageType: wsconn.TextMessage}
	start := time.Now()
	if handled := backpressuredSession.enqueueOutbound(openAIRealtimeOutbound{messageType: wsconn.TextMessage}); handled {
		t.Fatal("expected backpressured realtime session enqueue to fail")
	}
	if elapsed := time.Since(start); elapsed < backpressuredSession.outboundBackpressureTimeout {
		t.Fatalf("expected enqueue to wait for bounded backpressure timeout, elapsed=%s", elapsed)
	}

	conn, cleanupConn := newOpenAIRealtimeConnPair(t)
	defer cleanupConn()
	conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_closed_conn"})

	writeFailSession := newOpenAIRealtimeHelperSession()
	writeFailSession.conn = conn
	prewriteObserver := &failingAdmissionOpenAIRealtimeObserver{}
	writeFailSession.turnObserverFactory = func() runtimesession.TurnObserver { return prewriteObserver }
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
	if admitted, rolledBack := prewriteObserver.counts(); admitted != 1 || rolledBack != 1 || prewriteObserver.finalizeCount() != 0 {
		t.Fatalf("definitive pre-write failure admission=%d rollback=%d finalize=%d, want 1/1/0", admitted, rolledBack, prewriteObserver.finalizeCount())
	}

	ambiguousObserver := &failingAdmissionOpenAIRealtimeObserver{}
	ambiguousSession := newOpenAIRealtimeHelperSession()
	ambiguousSession.turnObserverFactory = func() runtimesession.TurnObserver { return ambiguousObserver }
	ambiguousTurn, err := ambiguousSession.startTurnWithClientEventID("evt_ambiguous_write", false)
	if err != nil {
		t.Fatalf("start ambiguous write turn: %v", err)
	}
	if err := runtimesession.AdmitTurn(ambiguousTurn.observer); err != nil {
		t.Fatalf("admit ambiguous write turn: %v", err)
	}
	rollbackAdmission := ambiguousSession.resolveOpenAIRealtimeWriteFailure(ambiguousTurn, true)
	if rollbackAdmission || ambiguousSession.turn != ambiguousTurn {
		t.Fatal("ambiguous write discarded the owner before draining evidence")
	}
	ambiguousSession.close("ws_write_failed")
	if admitted, rolledBack := ambiguousObserver.counts(); admitted != 1 || rolledBack != 0 || ambiguousObserver.finalizeCount() != 1 {
		t.Fatalf("ambiguous write admission=%d rollback=%d finalize=%d, want 1/0/1", admitted, rolledBack, ambiguousObserver.finalizeCount())
	}
	if reason := ambiguousObserver.lastPayload().TerminationReason; reason != "ws_write_failed" {
		t.Fatalf("ambiguous write termination reason=%q, want ws_write_failed", reason)
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
	closingSession.close("provider_closed")
	if finalizerRecorder.finalizeCount() != 1 {
		t.Fatalf("expected close to finalize the active turn, got %d", finalizerRecorder.finalizeCount())
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
	if terminal, reason := openAIRealtimeTurnTerminal("response.updated", types.NewErrorEvent("", "invalid_request_error", "bad_request", "boom")); terminal || reason != "" {
		t.Fatalf("expected generic error event not to terminate a realtime response, terminal=%v reason=%q", terminal, reason)
	}
	if terminal, reason := openAIRealtimeTurnTerminal("response.updated", nil); terminal || reason != "" {
		t.Fatalf("expected non-terminal event to remain open, terminal=%v reason=%q", terminal, reason)
	}
	if terminal, reason := openAIRealtimeTurnTerminal("response.completed", &types.Event{Response: &types.ResponseEvent{Status: types.ResponseStatusCompleted}}); terminal || reason != "" {
		t.Fatalf("expected Responses streaming terminal not to classify as Realtime, terminal=%v reason=%q", terminal, reason)
	}
}

func TestOpenAIRealtimeSessionConnectionErrorsAndAzureHeaders(t *testing.T) {
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

	t.Run("realtime forwards only a singleton client safety identifier", func(t *testing.T) {
		headerCh := make(chan http.Header, 1)
		server := newOpenAIRealtimeHeaderCaptureServer(t, headerCh, nil)
		defer server.Close()

		proxy := ""
		provider := CreateOpenAIProvider(&model.Channel{
			Key:   "sk-test",
			Proxy: &proxy,
			Other: `{"self_hosted":true}`,
		}, server.URL)
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
		ctx.Request.Header.Set(openAISafetyIdentifierHeader, "stable-user-hash")
		ctx.Request.Header.Set("OpenAI-Beta", "client-must-not-own-realtime-beta")
		provider.Context = ctx

		conn, errWithCode := provider.openRealtimeConn("gpt-4o-realtime-preview")
		if errWithCode != nil {
			t.Fatalf("expected realtime websocket to connect, got %v", errWithCode)
		}
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_done"})

		headers := <-headerCh
		if got := headers.Get(openAISafetyIdentifierHeader); got != "stable-user-hash" {
			t.Fatalf("expected client safety identifier to be forwarded, got %q", got)
		}
		if got := headers.Get("OpenAI-Beta"); got != "" {
			t.Fatalf("client realtime beta header crossed the controlled allowlist: %q", got)
		}
	})

	t.Run("realtime channel safety identifier overrides the client value", func(t *testing.T) {
		headerCh := make(chan http.Header, 1)
		server := newOpenAIRealtimeHeaderCaptureServer(t, headerCh, nil)
		defer server.Close()

		proxy := ""
		modelHeaders := `{"OpenAI-Safety-Identifier":"channel-owned-hash"}`
		provider := CreateOpenAIProvider(&model.Channel{
			Key:          "sk-test",
			Proxy:        &proxy,
			Other:        `{"self_hosted":true}`,
			ModelHeaders: &modelHeaders,
		}, server.URL)
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
		ctx.Request.Header.Set(openAISafetyIdentifierHeader, "client-hash")
		provider.Context = ctx

		conn, errWithCode := provider.openRealtimeConn("gpt-4o-realtime-preview")
		if errWithCode != nil {
			t.Fatalf("expected realtime websocket to connect, got %v", errWithCode)
		}
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_done"})
		if got := (<-headerCh).Get(openAISafetyIdentifierHeader); got != "channel-owned-hash" {
			t.Fatalf("expected channel-owned safety identifier, got %q", got)
		}
	})

	t.Run("realtime rejects multiple client safety identifiers before dialing", func(t *testing.T) {
		proxy := ""
		provider := CreateOpenAIProvider(&model.Channel{
			Key:   "sk-test",
			Proxy: &proxy,
			Other: `{"self_hosted":true}`,
		}, "http://127.0.0.1:1")
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
		ctx.Request.Header[openAISafetyIdentifierHeader] = []string{"first", "second"}
		provider.Context = ctx

		conn, errWithCode := provider.openRealtimeConn("gpt-4o-realtime-preview")
		if conn != nil || errWithCode == nil || errWithCode.Code != "invalid_request_header" || errWithCode.StatusCode != http.StatusBadRequest || !errWithCode.LocalError {
			t.Fatalf("expected pre-dial singleton validation error, conn=%v err=%+v", conn, errWithCode)
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

	disabled := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Key: "sk-test", Type: config.ChannelTypeCustom, Proxy: &proxy}, "https://compat.example")
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

func TestOpenAIResponsesWSTransportUsesSharedOfficialURLPolicy(t *testing.T) {
	proxy := "http://proxy.example"
	for _, test := range []struct {
		name    string
		baseURL string
		want    bool
	}{
		{name: "default TLS port", baseURL: "https://api.openai.com:443", want: true},
		{name: "uppercase authority", baseURL: "https://API.OPENAI.COM:443/", want: true},
		{name: "non-default port", baseURL: "https://api.openai.com:444"},
		{name: "subpath", baseURL: "https://api.openai.com/proxy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy}, test.baseURL)
			if got := provider.supportsNativeResponsesWSTransport(); got != test.want {
				t.Fatalf("supportsNativeResponsesWSTransport()=%t, want %t", got, test.want)
			}
			if test.want {
				if wsURL, errWithCode := provider.responsesWSURL("gpt-5"); errWithCode != nil || wsURL == "" {
					t.Fatalf("official URL passed capability but failed runtime URL construction: url=%q err=%+v", wsURL, errWithCode)
				}
			}
		})
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

	disabled := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Key: "sk-test", Type: config.ChannelTypeCustom, Proxy: &proxy}, "https://compat.example")
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
			if !errWithCode.UpstreamNotAttempted || errWithCode.UpstreamAmbiguous || errWithCode.UpstreamAccepted {
				t.Fatalf("handshake failure lost safe pre-write disposition: %+v", errWithCode)
			}
			if !errWithCode.ProviderOpenRetrySafe {
				t.Fatalf("handshake failure did not authorize safe route retry: %+v", errWithCode)
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
	if !errWithCode.UpstreamNotAttempted || errWithCode.UpstreamAmbiguous {
		t.Fatalf("dial transport failure lost pre-write disposition: %+v", errWithCode)
	}
}

func TestMapOpenAIResponsesWSDialErrorDoesNotLogSecrets(t *testing.T) {
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
	session.recentFinalizedIDs = nil
	providerInitiatedObserver := &failingAdmissionOpenAIRealtimeObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return providerInitiatedObserver })
	outbound, shouldClose := session.observeSupplierMessage(wsconn.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_orphan","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`))
	if shouldClose || outbound.usage == nil || outbound.usage.TotalTokens != 3 {
		t.Fatalf("expected provider-initiated terminal usage to be forwarded without closing, outbound=%+v should_close=%v", outbound, shouldClose)
	}
	if admits, rollbacks := providerInitiatedObserver.counts(); admits != 1 || rollbacks != 0 {
		t.Fatalf("expected provider-initiated turn admission once, admits=%d rollbacks=%d", admits, rollbacks)
	}
	if providerInitiatedObserver.observeCount() != 1 || providerInitiatedObserver.finalizeCount() != 1 {
		t.Fatalf("expected provider-initiated usage observation and settlement, observed=%d finalized=%d", providerInitiatedObserver.observeCount(), providerInitiatedObserver.finalizeCount())
	}
	if payload := providerInitiatedObserver.lastPayload(); payload.LastResponseID != "resp_orphan" || payload.Usage == nil || payload.Usage.TotalTokens != 3 {
		t.Fatalf("unexpected provider-initiated finalization payload: %+v", payload)
	}

	session.recentFinalizedIDs = []string{"resp_orphan"}
	outbound, shouldClose = session.observeSupplierMessage(wsconn.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_orphan","status":"completed","usage":{"input_tokens":5,"output_tokens":5,"total_tokens":10}}}`))
	if shouldClose || outbound.usage != nil {
		t.Fatalf("expected late finalized response usage to be dropped, outbound=%+v should_close=%v", outbound, shouldClose)
	}

	startSession := newOpenAIRealtimeHelperSession()
	startSession.turnObserverFactory = func() runtimesession.TurnObserver { return recorder }
	startedTurn, err := startSession.startTurnWithClientEventID("evt_started", false)
	if err != nil {
		t.Fatalf("expected helper startTurn to succeed, got %v", err)
	}
	if startedTurn == nil || startedTurn.observer == nil {
		t.Fatalf("expected startTurn to attach a guarded observer, got %+v", startedTurn)
	}
	if observer, payload := (&openAIRealtimeSession{}).finalizeTurn("ignored", now); observer != nil || payload.TurnSeq != 0 {
		t.Fatalf("expected finalizeTurn without an active turn to no-op, observer=%+v payload=%+v", observer, payload)
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

func TestOpenAIRealtimeProviderInitiatedTurnAdmissionFailureStopsSession(t *testing.T) {
	session := newOpenAIRealtimeHelperSession()
	observer := &failingAdmissionOpenAIRealtimeObserver{
		admitErr: common.StringErrorWrapperLocal("quota exhausted", "quota_exhausted", http.StatusForbidden),
	}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return observer })

	outbound, shouldClose := session.observeSupplierMessage(wsconn.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_auto","status":"in_progress"}}`))
	if !shouldClose || outbound.err == nil || outbound.origin != runtimerealtime.RealtimePayloadOriginProxyLocal {
		t.Fatalf("provider-initiated admission failure must stop the session with a local error, outbound=%+v should_close=%v", outbound, shouldClose)
	}
	if admits, rollbacks := observer.counts(); admits != 1 || rollbacks != 0 {
		t.Fatalf("provider-observed work must not synthesize a pre-send rollback, admits=%d rollbacks=%d", admits, rollbacks)
	}
	if observer.observeCount() != 0 || observer.finalizeCount() != 0 || session.turn == nil {
		t.Fatal("failed future admission discarded in-flight response before drain")
	}
	session.close("provider_initiated_admission_failed")
	if observer.finalizeCount() != 1 || session.turn != nil {
		t.Fatal("failed live admission was not finalized at final close")
	}
}

func TestOpenAIResponsesWSUnknownSteeringControlPreservesWire(t *testing.T) {
	adapter := openAIResponsesWSAdapter{}
	payload := `{"type":"response.steer.pending","sequence_number":{"future":true},"steer":{"previous_response_id":"old","future":[9007199254740993]},"reason":"future_reason"}`
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(payload)))
	if result.Err != nil || result.EmitFrame == nil || string(result.EmitFrame.Payload()) != payload || result.Usage != nil || result.CloseTransport {
		t.Fatalf("control not transparent: %+v", result)
	}
}

func TestOpenAIResponsesWSToolIdentityConflictPreservesWireAndIndependentTokens(t *testing.T) {
	adapter := &openAIResponsesWSAdapter{}
	adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.created","response":{"id":"resp_conflict","tools":[{"type":"web_search"}]}}`)))
	payload := []byte(`{"type":"response.output_item.done","item_id":"search_a","item":{"id":"search_b","type":"web_search_call","status":"completed","action":{"type":"search"}},"future":9007199254740993}`)
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame(payload))
	if result.Err != nil || result.CloseTransport || result.EmitFrame == nil || string(result.EmitFrame.Payload()) != string(payload) || result.Usage == nil {
		t.Fatalf("component conflict blocked raw delivery or lost diagnostic: %+v", result)
	}
	usage := result.Usage.ToChatUsage()
	if !usage.HasExtraBillingConflict(types.APIToolTypeWebSearch) || usage.AttributionConflict || len(usage.ExtraBilling) != 0 {
		t.Fatalf("tool conflict contaminated independent attribution: %+v", usage)
	}
	terminal := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.completed","response":{"id":"resp_conflict","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`)))
	if terminal.Err != nil || terminal.Usage == nil || !terminal.Usage.ProviderTokenEvidence || terminal.Usage.TotalTokens != 5 || terminal.Usage.AttributionConflict {
		t.Fatalf("tool conflict prevented independent token evidence: %+v", terminal)
	}
}
