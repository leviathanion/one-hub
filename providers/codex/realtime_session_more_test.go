package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common/authutil"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/gorilla/websocket"
)

func newCodexRealtimeConnPair(t *testing.T) (*websocket.Conn, func()) {
	t.Helper()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		<-r.Context().Done()
	}))

	wsURL := "ws" + server.URL[len("http"):]
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		server.Close()
		t.Fatalf("failed to dial helper websocket: %v", err)
	}

	return conn, func() {
		_ = conn.Close()
		server.Close()
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
	if cleared := clearCodexManagedWebsocketLocked(state); cleared != conn || state.wsConn != nil || state.wsReaderConn != nil || state.skipBootstrapConn != nil {
		t.Fatalf("expected clearCodexManagedWebsocketLocked to clear shared websocket references, cleared=%v state=%+v", cleared, state)
	}
	if cleared := clearCodexManagedWebsocketLocked(nil); cleared != nil {
		t.Fatalf("expected nil managed websocket clear to return nil, got %v", cleared)
	}

	configureCodexRealtimeConn(nil, nil)
	if err := writeCodexRealtimeWSMessageLocked(nil, nil, websocket.TextMessage, []byte("hello")); !errors.Is(err, websocket.ErrCloseSent) {
		t.Fatalf("expected nil websocket write to return close sent, got %v", err)
	}

	conn, cleanupWrite := newCodexRealtimeConnPair(t)
	defer cleanupWrite()
	state = &codexManagedRuntimeState{}
	configureCodexRealtimeConn(state, conn)
	if err := writeCodexRealtimeWSMessageLocked(state, conn, websocket.TextMessage, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("expected websocket helper write to succeed, got %v", err)
	}

	if attachment := newCodexAttachment(); attachment == nil || len(attachment.queue) != codexRealtimeAttachmentQueueCapacity {
		t.Fatalf("expected default attachment queue capacity %d, got %+v", codexRealtimeAttachmentQueueCapacity, attachment)
	}
	attachment := newCodexAttachmentWithCapacity(1)
	if attachment == nil || len(attachment.queue) != 1 {
		t.Fatalf("expected explicit attachment capacity, got %+v", attachment)
	}
	if ok := enqueueCodexOutbound(attachment, codexRealtimeOutbound{messageType: websocket.TextMessage, payload: []byte("first")}); !ok {
		t.Fatal("expected first attachment enqueue to succeed")
	}
	if outbound := recvCodexAttachmentOutbound(t, attachment); outbound.messageType != websocket.TextMessage || string(outbound.payload) != "first" {
		t.Fatalf("expected attachment recv to return queued payload, got %+v", outbound)
	}
	attachment.close()
	if !attachment.isClosed() {
		t.Fatal("expected attachment close to mark closed")
	}
	if outbound, err := attachment.recv(context.Background()); !errors.Is(err, runtimesession.ErrSessionClosed) || outbound.messageType != 0 {
		t.Fatalf("expected closed attachment recv to fail with session closed, outbound=%+v err=%v", outbound, err)
	}
	if outbound, err := (*codexAttachment)(nil).recv(context.Background()); !errors.Is(err, runtimesession.ErrSessionClosed) || outbound.messageType != 0 {
		t.Fatalf("expected nil attachment recv to fail with session closed, outbound=%+v err=%v", outbound, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if outbound, err := newCodexAttachmentWithCapacity(1).recv(ctx); !errors.Is(err, context.Canceled) || outbound.messageType != 0 {
		t.Fatalf("expected canceled attachment recv, outbound=%+v err=%v", outbound, err)
	}

	originalBackpressure := codexRealtimeOutboundBackpressureTimeout
	codexRealtimeOutboundBackpressureTimeout = 10 * time.Millisecond
	t.Cleanup(func() {
		codexRealtimeOutboundBackpressureTimeout = originalBackpressure
	})
	backpressured := newCodexAttachmentWithCapacity(1)
	if ok := enqueueCodexOutbound(backpressured, codexRealtimeOutbound{messageType: websocket.TextMessage}); !ok {
		t.Fatal("expected initial backpressure queue fill to succeed")
	}
	if ok := enqueueCodexOutbound(backpressured, codexRealtimeOutbound{messageType: websocket.TextMessage}); ok {
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
	if !isCodexRealtimeBootstrapMessage(websocket.TextMessage, []byte(`{"type":"session.created"}`)) {
		t.Fatal("expected session.created to be treated as bootstrap message")
	}
	if isCodexRealtimeBootstrapMessage(websocket.BinaryMessage, []byte(`{"type":"session.created"}`)) {
		t.Fatal("expected non-text realtime bootstrap message to be ignored")
	}
	if isCodexRealtimeBootstrapMessage(websocket.TextMessage, []byte("not-json")) {
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

	if terminal, responseID, reason := inspectCodexRealtimeSupplierEvent(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_supplier","status":"completed"}}`)); !terminal || responseID != "resp_supplier" || reason != "response.completed" {
		t.Fatalf("expected supplier terminal event inspection, terminal=%v response_id=%q reason=%q", terminal, responseID, reason)
	}
	if terminal, responseID, reason := inspectCodexRealtimeSupplierEvent(websocket.TextMessage, []byte("bad-json")); terminal || responseID != "" || reason != "" {
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

func TestCodexManagedRealtimeSessionGuardBranches(t *testing.T) {
	if err := (*codexManagedRealtimeSession)(nil).SendClient(context.Background(), websocket.TextMessage, []byte(`{}`)); !errors.Is(err, runtimesession.ErrSessionClosed) {
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

	if err := session.SendClient(context.Background(), websocket.BinaryMessage, []byte("binary")); err == nil {
		t.Fatal("expected binary realtime client payload to be rejected")
	}
	if err := session.SendClient(context.Background(), websocket.TextMessage, []byte("not-json")); err == nil {
		t.Fatal("expected invalid realtime client json to be rejected")
	}

	foreignSession := *session
	foreignSession.ownerSeq = 2
	if err := foreignSession.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.cancel"}`)); !errors.Is(err, runtimesession.ErrSessionClosed) {
		t.Fatalf("expected foreign attachment owner SendClient to be rejected, got %v", err)
	}

	if err := session.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.create","event_id":"evt_missing"}`)); err == nil {
		t.Fatal("expected missing response.create payload to be rejected")
	}

	exec.Inflight = true
	if err := session.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.create","event_id":"evt_busy","response":{"input":[]}}`)); err == nil {
		t.Fatal("expected inflight response.create to be rejected as busy")
	}
	exec.Inflight = false

	if err := session.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.create","event_id":"evt_mismatch","response":{"model":"o4-mini","input":[]}}`)); err == nil {
		t.Fatal("expected mismatched response.create model to be rejected")
	}

	beginCodexTurnLocked(state, time.Now())
	exec.Transport = runtimesession.TransportModeResponsesWS
	if err := session.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.cancel","event_id":"evt_cancel"}`)); err != nil {
		t.Fatalf("expected response.cancel without wsConn to finalize locally, got %v", err)
	}
	if exec.Inflight || exec.State != runtimesession.SessionStateIdle {
		t.Fatalf("expected local response.cancel to reset inflight state, exec=%+v", exec)
	}

	if err := session.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"unsupported","event_id":"evt_unsupported"}`)); err == nil {
		t.Fatal("expected unsupported realtime client event to be rejected")
	}
	if _, _, _, err := (*codexManagedRealtimeSession)(nil).Recv(context.Background()); !errors.Is(err, runtimesession.ErrSessionClosed) {
		t.Fatalf("expected nil managed realtime session Recv to report session closed, got %v", err)
	}

	state.turnObserver = nil
	state.turnObserverFactory = nil
	exec.Inflight = true
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return &recordingTurnObserver{} })
	if state.turnObserverFactory == nil || state.turnObserver == nil {
		t.Fatalf("expected SetTurnObserverFactory to seed observer for inflight owned session, state=%+v", state)
	}
	exec.Inflight = false

	session.Detach("test_detach")
	if !attachment.isClosed() || exec.Attached {
		t.Fatalf("expected Detach to close attachment and mark exec detached, exec=%+v closed=%v", exec, attachment.isClosed())
	}

	manager := runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{DefaultTTL: time.Minute})
	originalManager := codexExecutionSessions
	codexExecutionSessions = manager
	t.Cleanup(func() {
		codexExecutionSessions = originalManager
		manager.Close()
	})

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
	abortState.attachment = abortAttachment
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
	if removed := manager.DeleteIf(abortExec.Key, abortExec); removed != nil {
		t.Fatalf("expected aborted execution session to be removed from manager, removed=%+v", removed)
	}
}

func TestCodexRealtimeMetadataCompatibilityAndNamespaceBranches(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"user_agent":"legacy-ua","websocket_mode":"off"}`, nil)
	provider.Context.Set("id", 7001)
	provider.Context.Request.Header.Set("Authorization", "Bearer sk-session-auth")

	if _, errWithCode := provider.buildExecutionSessionMetadata("gpt-5", runtimesession.RealtimeOpenOptions{ClientSessionID: "bad/session"}); errWithCode == nil || errWithCode.Code != "invalid_session_id" {
		t.Fatalf("expected invalid client session id metadata error, got %+v", errWithCode)
	}
	provider.Context.Request.Header.Set("X-Session-Id", "bad/session")
	if _, _, errWithCode := provider.readRealtimeClientSessionID(runtimesession.RealtimeOpenOptions{}); errWithCode == nil || errWithCode.Code != "invalid_session_id" {
		t.Fatalf("expected invalid request session id to fail validation, got %+v", errWithCode)
	}
	provider.Context.Request.Header.Set("X-Session-Id", "client-session")
	if sessionID, clientSupplied, errWithCode := provider.readRealtimeClientSessionID(runtimesession.RealtimeOpenOptions{}); errWithCode != nil || !clientSupplied || sessionID != "client-session" {
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

	if err := validateCodexRealtimeExecutionSessionID(strings.Repeat("a", codexRealtimeSessionIDMaxLen+1)); err == nil {
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

	modelHeaders := `{"Authorization":"ignored","Connection":"ignored","X-Session-Id":"ignored","Originator":"codex_cli_rs","X-Trace":"trace"}`
	provider.Channel.ModelHeaders = &modelHeaders
	channelHeaders := provider.buildRealtimeChannelCompatibilityHeaders()
	if _, exists := channelHeaders["authorization"]; exists {
		t.Fatalf("expected authorization header to be filtered, got %+v", channelHeaders)
	}
	if channelHeaders["x-trace"] != "trace" || channelHeaders["originator"] != defaultOriginator {
		t.Fatalf("expected filtered compatibility headers to preserve x-trace/originator, got %+v", channelHeaders)
	}

	signature := provider.buildRealtimeHandshakePolicySignature()
	if !strings.Contains(signature, "legacy-ua") || strings.Contains(signature, defaultOriginator) {
		t.Fatalf("expected handshake signature to use legacy user agent and strip default originator, got %q", signature)
	}
	if got := provider.buildRealtimeCompatibilityHash("gpt-5", provider.readRealtimeUpstreamIdentity()); got == "" {
		t.Fatal("expected compatibility hash to be populated")
	}
	if got := provider.readRealtimeUpstreamIdentity(); !strings.Contains(got, "credential:account:acct-123") {
		t.Fatalf("expected upstream identity to include credential identity, got %q", got)
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
	if wsExec.Transport != runtimesession.TransportModeResponsesWS || wsState.wsReaderConn != wsConn {
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
	if terminal, responseID, reason := inspectCodexRealtimeSupplierEvent(websocket.BinaryMessage, []byte(`{"type":"response.completed"}`)); terminal || responseID != "" || reason != "" {
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
