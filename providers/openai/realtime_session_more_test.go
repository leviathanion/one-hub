package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"one-api/common/logger"
	"one-api/model"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
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

func newOpenAIRealtimeConnPair(t *testing.T) (*websocket.Conn, func()) {
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

func TestOpenAIRealtimeSessionHelperNormalizationAndIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	ctx.Request.Header.Set("X-Session-Id", "client-session")
	provider := &OpenAIProvider{}
	provider.Context = ctx

	if got := readOpenAIRealtimeSessionID(provider); got != "client-session" {
		t.Fatalf("expected request session id, got %q", got)
	}
	if got := readOpenAIRealtimeSessionID(&OpenAIProvider{}); got == "" {
		t.Fatal("expected helper to generate fallback realtime session id")
	}

	binaryPayload := []byte{1, 2, 3}
	if normalized, eventType, err := normalizeOpenAIRealtimeClientPayload(binaryPayload, websocket.BinaryMessage, "gpt-4o", false); err != nil || eventType != "" || string(normalized) != string(binaryPayload) {
		t.Fatalf("expected non-text payload passthrough, normalized=%v event=%q err=%v", normalized, eventType, err)
	}

	if normalized, eventType, err := normalizeOpenAIRealtimeClientPayload([]byte("not-json"), websocket.TextMessage, "gpt-4o", false); err != nil || eventType != "" || string(normalized) != "not-json" {
		t.Fatalf("expected invalid json passthrough, normalized=%q event=%q err=%v", string(normalized), eventType, err)
	}

	compatPayload := []byte(`{"type":"response.create","response":{"input":[]}}`)
	if normalized, eventType, err := normalizeOpenAIRealtimeClientPayload(compatPayload, websocket.TextMessage, "gpt-4o", true); err != nil || eventType != "response.create" || string(normalized) != string(compatPayload) {
		t.Fatalf("expected compat mode passthrough, normalized=%q event=%q err=%v", string(normalized), eventType, err)
	}

	withResponse, eventType, err := normalizeOpenAIRealtimeClientPayload([]byte(`{"type":"response.create","response":{"input":[]}}`), websocket.TextMessage, "gpt-4o", false)
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

	topLevel, eventType, err := normalizeOpenAIRealtimeClientPayload([]byte(`{"type":"response.create"}`), websocket.TextMessage, "gpt-4o-mini", false)
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

	preseeded, _, err := normalizeOpenAIRealtimeClientPayload([]byte(`{"type":"response.create","response":{"model":"o1","input":[]}}`), websocket.TextMessage, "gpt-4o", false)
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

	cancelPayload, eventType, err := normalizeOpenAIRealtimeClientPayload([]byte(`{"type":"response.cancel"}`), websocket.TextMessage, "gpt-4o", false)
	if err != nil || eventType != "response.cancel" || string(cancelPayload) != `{"type":"response.cancel"}` {
		t.Fatalf("expected non-response.create realtime payload to pass through, payload=%q event=%q err=%v", string(cancelPayload), eventType, err)
	}
	blankModelPayload, eventType, err := normalizeOpenAIRealtimeClientPayload([]byte(`{"type":"response.create","response":{"input":[]}}`), websocket.TextMessage, "   ", false)
	if err != nil || eventType != "response.create" || string(blankModelPayload) != `{"type":"response.create","response":{"input":[]}}` {
		t.Fatalf("expected blank model normalization to preserve payload, payload=%q event=%q err=%v", string(blankModelPayload), eventType, err)
	}

	if got := anyToString(123); got != "" {
		t.Fatalf("expected non-string conversion to return empty string, got %q", got)
	}
	if usage := openAIRealtimeResponseUsage(nil); usage != nil {
		t.Fatalf("expected nil response usage to stay nil, got %+v", usage)
	}
	responseEvent := &types.ResponseEvent{Usage: &types.UsageEvent{TotalTokens: 9}}
	if usage := openAIRealtimeResponseUsage(responseEvent); usage == nil || usage.TotalTokens != 9 {
		t.Fatalf("expected response usage passthrough, got %+v", usage)
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
	if messageType, payload, usage, err, handled := (*openAIRealtimeSession)(nil).recvQueuedOutbound(); !handled || !errors.Is(err, runtimesession.ErrSessionClosed) || messageType != 0 || payload != nil || usage != nil {
		t.Fatalf("expected nil session queue read to report session closed, type=%d payload=%v usage=%v err=%v handled=%v", messageType, payload, usage, err, handled)
	}
	if messageType, payload, usage, err := decodeOpenAIRealtimeOutbound(openAIRealtimeOutbound{}, false); !errors.Is(err, runtimesession.ErrSessionClosed) || messageType != 0 || payload != nil || usage != nil {
		t.Fatalf("expected closed outbound decode to report session closed, type=%d payload=%v usage=%v err=%v", messageType, payload, usage, err)
	}
	if terminal, reason := openAIRealtimeTurnTerminal(types.EventTypeResponseDone, nil); !terminal || reason != types.EventTypeResponseDone {
		t.Fatalf("expected response.done helper classification, terminal=%v reason=%q", terminal, reason)
	}

	session := newOpenAIRealtimeHelperSession()
	if handled := session.enqueueOutbound(openAIRealtimeOutbound{messageType: websocket.TextMessage, payload: []byte("queued")}); !handled {
		t.Fatal("expected enqueueOutbound to queue active outbound")
	}
	if messageType, payload, _, err, handled := session.recvQueuedOutbound(); !handled || err != nil || messageType != websocket.TextMessage || string(payload) != "queued" {
		t.Fatalf("expected queued outbound recv, type=%d payload=%q err=%v handled=%v", messageType, string(payload), err, handled)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := session.Recv(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled Recv to return context cancellation, got %v", err)
	}

	detachedSession := newOpenAIRealtimeHelperSession()
	close(detachedSession.detached)
	if _, _, _, err := detachedSession.Recv(context.Background()); !errors.Is(err, runtimesession.ErrSessionClosed) {
		t.Fatalf("expected detached Recv to return session closed, got %v", err)
	}

	closedSession := newOpenAIRealtimeHelperSession()
	close(closedSession.closed)
	if _, _, _, err := closedSession.Recv(context.Background()); !errors.Is(err, runtimesession.ErrSessionClosed) {
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

	(&openAIRealtimeSession{}).configureConn()

	conn, cleanupConn := newOpenAIRealtimeConnPair(t)
	defer cleanupConn()
	if err := conn.Close(); err != nil {
		t.Fatalf("failed to close helper websocket client: %v", err)
	}

	writeFailSession := newOpenAIRealtimeHelperSession()
	writeFailSession.conn = conn
	if err := writeFailSession.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.create","response":{"input":[]}}`)); err == nil {
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
	if err := busySession.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.create","response":{"input":[]}}`)); err == nil {
		t.Fatal("expected busy realtime session to reject a second response.create")
	}

	guardSession := newOpenAIRealtimeHelperSession()
	if err := guardSession.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"bad"`)); err == nil {
		t.Fatal("expected invalid client payload to fail")
	}
	if err := (*openAIRealtimeSession)(nil).SendClient(context.Background(), websocket.TextMessage, []byte(`{}`)); !errors.Is(err, runtimesession.ErrSessionClosed) {
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
		provider := CreateOpenAIProvider(&model.Channel{Key: "sk-test", Proxy: &proxy}, "http://127.0.0.1:1")

		conn, errWithCode := provider.openRealtimeConn("gpt-4o-realtime-preview")
		if conn != nil {
			t.Fatalf("expected realtime dial failure to return no connection, got %#v", conn)
		}
		if errWithCode == nil || errWithCode.Code != "ws_request_failed" || errWithCode.StatusCode != http.StatusInternalServerError {
			t.Fatalf("expected ws_request_failed dial error, got %+v", errWithCode)
		}
	})

	t.Run("azure websocket auth uses api key header", func(t *testing.T) {
		headerCh := make(chan http.Header, 1)
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headerCh <- r.Header.Clone()
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade failed: %v", err)
				return
			}
			defer conn.Close()
			<-r.Context().Done()
		}))
		defer server.Close()

		proxy := ""
		provider := CreateOpenAIProvider(&model.Channel{
			Key:   "azure-key",
			Other: "2024-10-01-preview",
			Proxy: &proxy,
		}, server.URL)
		provider.IsAzure = true

		conn, errWithCode := provider.openRealtimeConn("gpt-4o-realtime-preview")
		if errWithCode != nil {
			t.Fatalf("expected azure realtime websocket to connect, got %v", errWithCode)
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("expected azure realtime websocket close to succeed, got %v", err)
		}

		headers := <-headerCh
		if got := headers.Get("Api-Key"); got != "azure-key" {
			t.Fatalf("expected azure websocket to authenticate with api-key header, got %q", got)
		}
		if got := headers.Get("Authorization"); got != "" {
			t.Fatalf("expected azure websocket auth not to use bearer auth header, got %q", got)
		}
		if got := headers.Get("Openai-Beta"); got != "realtime=v1" {
			t.Fatalf("expected realtime beta header to be attached, got %q", got)
		}
	})
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

	if outbound, shouldClose := session.observeSupplierMessage(websocket.BinaryMessage, []byte{1, 2, 3}); shouldClose || outbound.err != nil || string(outbound.payload) != string([]byte{1, 2, 3}) {
		t.Fatalf("expected binary realtime supplier payload passthrough, outbound=%+v should_close=%v", outbound, shouldClose)
	}
	if outbound, shouldClose := session.observeSupplierMessage(websocket.TextMessage, []byte("not-json")); shouldClose || outbound.err != nil || string(outbound.payload) != "not-json" {
		t.Fatalf("expected invalid json supplier payload passthrough, outbound=%+v should_close=%v", outbound, shouldClose)
	}

	session.compatMode = true
	if outbound, shouldClose := session.observeSupplierMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"bad_request","message":"boom"}}`)); !shouldClose || !errors.Is(outbound.err, runtimesession.ErrSessionClosed) {
		t.Fatalf("expected compat mode upstream errors to close the session, outbound=%+v should_close=%v", outbound, shouldClose)
	}

	session.compatMode = false
	session.turn = nil
	session.pendingTurns = nil
	session.recentFinalizedIDs = nil
	outbound, shouldClose := session.observeSupplierMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_orphan","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`))
	if shouldClose || outbound.usage == nil || outbound.usage.TotalTokens != 3 {
		t.Fatalf("expected orphan terminal usage to be forwarded without closing, outbound=%+v should_close=%v", outbound, shouldClose)
	}

	session.recentFinalizedIDs = []string{"resp_orphan"}
	outbound, shouldClose = session.observeSupplierMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_orphan","status":"completed","usage":{"input_tokens":5,"output_tokens":5,"total_tokens":10}}}`))
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
