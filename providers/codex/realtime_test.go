package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/wsconn"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func recvCodexAttachmentOutbound(t *testing.T, attachment *codexAttachment) codexRealtimeOutbound {
	t.Helper()
	outbound, err := recvCodexAttachmentOutboundWithTimeout(attachment, 2*time.Second)
	if err != nil {
		t.Fatalf("expected outbound payload, got %v", err)
	}
	return outbound
}

func recvCodexAttachmentOutboundWithTimeout(attachment *codexAttachment, timeout time.Duration) (codexRealtimeOutbound, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return attachment.recv(ctx)
}

func waitForCodexAttachmentClosed(t *testing.T, attachment *codexAttachment, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if attachment.isClosed() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !attachment.isClosed() {
		t.Fatal("timed out waiting for attachment to close")
	}
}

func dialCodexManagedTestConn(t *testing.T, wsURL string) *wsconn.ManagedConn {
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
		t.Fatalf("failed to dial managed test websocket: %v", err)
	}
	return conn
}

func codexIdleWSTestServer(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		t.Cleanup(conn.Close) // httptest.Server 不负责关闭已升级的连接。
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

type codexRealtimeTestFrame struct {
	messageType wsconn.MessageType
	payload     []byte
}

type codexRealtimeTestConn struct {
	conn *wsconn.ManagedConn

	frames          chan codexRealtimeTestFrame
	closeFramesOnce sync.Once
}

func newCodexRealtimeTestConn(conn *wsconn.ManagedConn) *codexRealtimeTestConn {
	testConn := &codexRealtimeTestConn{
		conn:   conn,
		frames: make(chan codexRealtimeTestFrame, 32),
	}
	go func() {
		defer func() {
			if recover() != nil {
				testConn.closeFramesOnce.Do(func() { close(testConn.frames) })
			}
		}()
		wsconn.Pump{
			Conn: conn,
			Handle: func(_ context.Context, mt wsconn.MessageType, payload []byte) {
				testConn.frames <- codexRealtimeTestFrame{
					messageType: mt,
					payload:     append([]byte(nil), payload...),
				}
			},
			OnClose: func(wsconn.CloseInfo) {
				testConn.closeFramesOnce.Do(func() { close(testConn.frames) })
			},
		}.Run(context.Background())
	}()
	return testConn
}

func (c *codexRealtimeTestConn) ReadMessage() (wsconn.MessageType, []byte, error) {
	frame, ok := <-c.frames
	if !ok {
		return 0, nil, net.ErrClosed
	}
	return frame.messageType, frame.payload, nil
}

func (c *codexRealtimeTestConn) WriteMessage(mt wsconn.MessageType, payload []byte) error {
	return c.conn.WriteMessage(mt, payload)
}

func (c *codexRealtimeTestConn) Close() {
	c.conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
}

func acceptCodexRealtimeTestConn(t *testing.T, w http.ResponseWriter, r *http.Request) (*codexRealtimeTestConn, bool) {
	t.Helper()
	conn, err := wsconn.AcceptManaged(w, r, wsconn.Config{Label: "codex realtime test accept"}, wsconn.AcceptOptions{
		CheckOrigin: func(*http.Request) bool { return true },
	})
	if err != nil {
		t.Errorf("accept managed: %v", err)
		return nil, false
	}
	return newCodexRealtimeTestConn(conn), true
}

func newCodexRealtimeCountingServer(t *testing.T, counter *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		if counter != nil {
			counter.Add(1)
		}
		go func() {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
}

func codexTestTextFrame(payload []byte) runtimerealtime.Frame {
	return runtimerealtime.NewTextFrame(payload)
}

func codexTestBinaryFrame(payload []byte) runtimerealtime.Frame {
	return runtimerealtime.NewBinaryFrame(payload)
}

func codexTestRecv(ctx context.Context, session runtimerealtime.RealtimeSession) (wsconn.MessageType, []byte, *types.UsageEvent, runtimerealtime.RealtimePayloadOrigin, error) {
	event, err := session.Recv(ctx)
	if err != nil {
		return 0, nil, nil, runtimerealtime.RealtimePayloadOriginProxyLocal, err
	}
	messageType := wsconn.TextMessage
	var payload []byte
	if event.Frame != nil {
		if event.Frame.Kind() == runtimerealtime.FrameKindBinary {
			messageType = wsconn.BinaryMessage
		}
		payload = event.Frame.Payload()
	}
	return messageType, payload, event.Usage, event.Origin, event.Err
}

func assignCodexAttachmentOwnerLocked(state *codexManagedRuntimeState, attachment *codexAttachment) uint64 {
	if state == nil {
		return 0
	}
	state.ownerSeq++
	state.attachment = attachment
	return state.ownerSeq
}

func resolveTestRealtimeBinding(c *gin.Context) (*runtimesession.Binding, bool) {
	if c == nil || c.Request == nil {
		return nil, false
	}
	sessionID := strings.TrimSpace(c.Request.Header.Get("x-session-id"))
	if sessionID == "" {
		sessionID = strings.TrimSpace(c.Request.Header.Get("session_id"))
	}
	if sessionID == "" {
		return nil, false
	}
	bindingKey := runtimesession.BuildBindingKey(readCodexRealtimeCallerNamespace(c), runtimesession.BindingScopeChatRealtime, sessionID)
	return currentCodexExecutionSessions().Resolve(bindingKey)
}

func TestCodexRealtimeBootstrapMessageDetection(t *testing.T) {
	payload := []byte(`{"type":"session.created","session":{"id":"execution-session-456"}}`)
	if !isCodexRealtimeBootstrapMessage(wsconn.TextMessage, payload) {
		t.Fatal("expected Codex realtime bootstrap detector to match session.created events")
	}
}

func TestCodexRealtimeHandlerLogsMalformedJSONAndContinues(t *testing.T) {
	provider := &CodexProvider{}
	shouldContinue, usage, newMessage, err := provider.handleCodexSupplierMessage(wsconn.TextMessage, []byte(`{"type":`), nil)
	if err != nil {
		t.Fatalf("expected malformed provider JSON to be ignored without handler error, got %v", err)
	}
	if !shouldContinue || usage != nil || newMessage != nil {
		t.Fatalf("expected malformed provider JSON to be ignored, continue=%v usage=%+v message=%s", shouldContinue, usage, string(newMessage))
	}

	longPayload := []byte(strings.Repeat("x", codexSupplierMalformedPayloadLogLimit+8))
	snippet := codexSupplierPayloadSnippet(longPayload)
	if len(snippet) <= codexSupplierMalformedPayloadLogLimit || !strings.Contains(snippet, "truncated") {
		t.Fatalf("expected malformed payload snippet to be bounded and marked truncated, got len=%d snippet suffix=%q", len(snippet), snippet[len(snippet)-20:])
	}
}

func TestCodexRealtimeHandlerExtractsUsageOnTerminalResponseEvents(t *testing.T) {
	provider := &CodexProvider{}
	testCases := []struct {
		name    string
		payload []byte
	}{
		{
			name:    "completed event",
			payload: []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`),
		},
		{
			name:    "done event",
			payload: []byte(`{"type":"response.done","response":{"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`),
		},
		{
			name:    "failed event",
			payload: []byte(`{"type":"response.failed","response":{"status":"failed","usage":{"input_tokens":3,"output_tokens":0,"total_tokens":3}}}`),
		},
		{
			name:    "incomplete event",
			payload: []byte(`{"type":"response.incomplete","response":{"status":"incomplete","usage":{"input_tokens":3,"output_tokens":0,"total_tokens":3}}}`),
		},
		{
			name:    "terminal status without terminal event type",
			payload: []byte(`{"type":"response.updated","response":{"status":"cancelled","usage":{"input_tokens":3,"output_tokens":0,"total_tokens":3}}}`),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			shouldContinue, usage, newMessage, err := provider.handleCodexSupplierMessage(wsconn.TextMessage, testCase.payload, newCodexTurnUsageAccumulator())
			if err != nil {
				t.Fatalf("expected no handler error, got %v", err)
			}
			if !shouldContinue {
				t.Fatalf("expected stream to continue")
			}
			if newMessage != nil {
				t.Fatalf("expected passthrough without rewriting")
			}
			if usage == nil {
				t.Fatalf("expected usage to be extracted")
			}
			if usage.TotalTokens == 0 || usage.InputTokens == 0 {
				t.Fatalf("expected terminal usage to preserve prompt accounting, got %+v", usage)
			}
		})
	}
}

func TestCodexRealtimeHandlerPreservesToolCallExtraBillingOnTerminalResponses(t *testing.T) {
	provider := &CodexProvider{}
	payload := []byte(`{
		"type":"response.done",
		"response":{
			"status":"completed",
			"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8},
			"tools":[{"type":"web_search_preview","search_context_size":"high"}],
			"output":[{"type":"web_search_call","id":"ws_123","status":"completed","action":{"type":"search"}}]
		}
	}`)

	shouldContinue, usage, _, err := provider.handleCodexSupplierMessage(wsconn.TextMessage, payload, newCodexTurnUsageAccumulator())
	if err != nil {
		t.Fatalf("expected no handler error, got %v", err)
	}
	if !shouldContinue {
		t.Fatal("expected terminal response to keep the stream alive")
	}
	if usage == nil {
		t.Fatal("expected usage to be extracted")
	}
	billing, ok := usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected terminal realtime usage to carry web search extra billing, got %+v", usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected a single high web search tool charge, got %+v", billing)
	}
}

func TestCodexRealtimeHandlerDoesNotSynthesizeMissingTerminalUsage(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() {
		config.DisableTokenEncoders = originalDisable
	})

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	accumulator := newCodexTurnUsageAccumulator()
	accumulator.SeedPromptFromRequest(&types.OpenAIResponsesRequest{Model: "gpt-5", Input: "hello"}, provider.Channel.PreCost)

	payload := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_seeded",
			"status":"completed",
			"tools":[{"type":"web_search_preview","search_context_size":"high"}],
			"output":[
				{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello from realtime"}]},
				{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search"}}
			]
		}
	}`)

	shouldContinue, usage, _, err := provider.handleCodexSupplierMessage(wsconn.TextMessage, payload, accumulator)
	if err != nil {
		t.Fatalf("expected no handler error, got %v", err)
	}
	if !shouldContinue {
		t.Fatal("expected terminal response to keep the stream alive")
	}
	if usage == nil || usage.InputTokens <= 0 || usage.OutputTokens != 0 || usage.TotalTokens != usage.InputTokens || usage.ProviderTokenEvidence {
		t.Fatalf("provider-missing terminal content produced token evidence: %+v", usage)
	}
	billing, ok := usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected non-authoritative realtime usage to preserve web search extra billing, got %+v", usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected a single high web search tool charge, got %+v", billing)
	}
}

func TestCodexRealtimeHandlerIgnoresUsageOnNonTerminalResponseEvents(t *testing.T) {
	provider := &CodexProvider{}
	payload := []byte(`{"type":"response.created","response":{"status":"in_progress","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`)

	shouldContinue, usage, newMessage, err := provider.handleCodexSupplierMessage(wsconn.TextMessage, payload, newCodexTurnUsageAccumulator())
	if err != nil {
		t.Fatalf("expected no handler error, got %v", err)
	}
	if !shouldContinue {
		t.Fatalf("expected stream to continue")
	}
	if newMessage != nil {
		t.Fatalf("expected passthrough without rewriting")
	}
	if usage != nil {
		t.Fatalf("expected non-terminal usage snapshot to be ignored, got %+v", usage)
	}
}

func TestCodexRealtimeHeadersFollowConfiguredBaseURLHost(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	provider.Channel.BaseURL = stringPtr("https://proxy.internal:8443/custom-prefix")

	headers, err := provider.getRealtimeHeaders("execution-session-456")
	if err != nil {
		t.Fatalf("expected realtime headers to build, got %v", err)
	}

	if got := headers["Host"]; got != "proxy.internal:8443" {
		t.Fatalf("expected realtime host header to follow configured base url, got %q", got)
	}
}

func TestCodexRealtimeHeadersBackfillXSessionIDForSessionIDOnlyClients(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"session_id": "execution-session-456",
	})

	headers, err := provider.getRealtimeHeaders("execution-session-456")
	if err != nil {
		t.Fatalf("expected realtime headers to build, got %v", err)
	}

	if got := headers["session_id"]; got != "execution-session-456" {
		t.Fatalf("expected websocket session_id to be preserved, got %q", got)
	}
	if got := headers["x-session-id"]; got != "execution-session-456" {
		t.Fatalf("expected websocket path to backfill x-session-id, got %q", got)
	}
}

func TestCodexRealtimeHeadersReplaceIncomingOpenAIBetaHeader(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"OpenAI-Beta": "legacy_beta_header",
	})

	headers, err := provider.getRealtimeHeaders("execution-session-456")
	if err != nil {
		t.Fatalf("expected realtime headers to build, got %v", err)
	}

	if got := headers["OpenAI-Beta"]; got != codexResponsesWebsocketBetaHeaderValue {
		t.Fatalf("expected realtime beta header %q, got %q", codexResponsesWebsocketBetaHeaderValue, got)
	}
	if got := countHeadersByKey(headers, "openai-beta"); got != 1 {
		t.Fatalf("expected a single openai-beta header after replacement, got %d", got)
	}
}

func TestCodexRealtimeHeadersRemoveConnectionAndAcceptCaseInsensitively(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	provider.Channel.ModelHeaders = stringPtr(`{"connection":"Upgrade","accept":"text/event-stream"}`)

	headers, err := provider.getRealtimeHeaders("execution-session-456")
	if err != nil {
		t.Fatalf("expected realtime headers to build, got %v", err)
	}

	if got := countHeadersByKey(headers, "connection"); got != 0 {
		t.Fatalf("expected websocket headers to remove connection overrides case-insensitively, got %d variants", got)
	}
	if got := countHeadersByKey(headers, "accept"); got != 0 {
		t.Fatalf("expected websocket headers to remove accept overrides case-insensitively, got %d variants", got)
	}
}

func TestCodexRealtimeHeadersReplaceOverridesCaseInsensitively(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Codex-Beta-Features":                 "request-feature",
		"X-Codex-Turn-State":                    "request-turn-state",
		"X-ResponsesApi-Include-Timing-Metrics": "true",
	})
	provider.Channel.ModelHeaders = stringPtr(`{"x-codex-beta-features":"channel-feature","x-codex-turn-state":"channel-turn-state","x-responsesapi-include-timing-metrics":"false"}`)

	headers, err := provider.getRealtimeHeaders("execution-session-456")
	if err != nil {
		t.Fatalf("expected realtime headers to build, got %v", err)
	}

	if got := getHeaderValue(headers, "x-codex-beta-features"); got != "request-feature" {
		t.Fatalf("expected realtime beta feature override to win, got %q", got)
	}
	if got := countHeadersByKey(headers, "x-codex-beta-features"); got != 1 {
		t.Fatalf("expected a single beta feature header after override replacement, got %d", got)
	}
	if got := getHeaderValue(headers, "x-codex-turn-state"); got != "request-turn-state" {
		t.Fatalf("expected realtime turn state override to win, got %q", got)
	}
	if got := countHeadersByKey(headers, "x-codex-turn-state"); got != 1 {
		t.Fatalf("expected a single turn state header after override replacement, got %d", got)
	}
	if got := getHeaderValue(headers, "x-responsesapi-include-timing-metrics"); got != "true" {
		t.Fatalf("expected realtime timing metrics override to win, got %q", got)
	}
	if got := countHeadersByKey(headers, "x-responsesapi-include-timing-metrics"); got != 1 {
		t.Fatalf("expected a single timing metrics header after override replacement, got %d", got)
	}
}

func TestCodexManagedRealtimeSkipsBootstrapFrameOnNewWebsocket(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		defer conn.Close()

		if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"session.created","event_id":"evt_bootstrap"}`)); err != nil {
			return
		}

		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}

		_ = conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_after_bootstrap","status":"completed"}}`))
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-bootstrap-session",
	})
	provider.Context.Set("token_id", 108)
	provider.Channel.BaseURL = stringPtr(server.URL)

	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime websocket session to open, got %v", errWithCode)
	}
	defer session.Detach("test_close")
	defer cleanupCodexManagedSession(t, provider, "gpt-5")

	createEvent := []byte(`{"type":"response.create","event_id":"evt_create","model":"gpt-5","input":"hello"}`)
	if err := session.SendClient(context.Background(), codexTestTextFrame(createEvent)); err != nil {
		t.Fatalf("expected websocket dispatch to succeed, got %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, payload, _, _, err := codexTestRecv(ctx, session)
	if err != nil {
		t.Fatalf("expected post-bootstrap response, got %v", err)
	}
	if got := string(payload); containsAll(got, "session.created") {
		t.Fatalf("expected bootstrap frame to be skipped, got %q", got)
	}
	if got := string(payload); !containsAll(got, "response.completed", "resp_after_bootstrap") {
		t.Fatalf("expected response after bootstrap skip, got %q", got)
	}
}

func TestCodexManagedRealtimeReplacementReaderPreservesBootstrapOwnership(t *testing.T) {
	accepted := make(chan *codexRealtimeTestConn, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		accepted <- conn
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn1 := dialCodexManagedTestConn(t, wsURL)
	defer conn1.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})

	serverConn1 := <-accepted
	defer serverConn1.Close()

	conn2 := dialCodexManagedTestConn(t, wsURL)
	defer conn2.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})

	serverConn2 := <-accepted
	defer serverConn2.Close()

	provider := &CodexProvider{}
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "replacement-bootstrap-owner",
		SessionID: "replacement-bootstrap-owner",
		Model:     "gpt-5",
		Protocol:  codexRealtimeProtocolName,
	})
	attachment := newCodexAttachment()

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	assignCodexAttachmentOwnerLocked(state, attachment)
	state.wsConn = conn1
	provider.startRealtimeWSReaderLocked(exec, state)
	replaced := clearCodexManagedWebsocketLocked(state)
	if replaced.conn != conn1 {
		exec.Unlock()
		t.Fatalf("expected websocket replacement to clear the original connection")
	}
	state.wsConn = conn2
	state.skipBootstrapConn = conn2
	provider.startRealtimeWSReaderLocked(exec, state)
	exec.Unlock()

	serverConn1.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		exec.Lock()
		state = getCodexManagedRuntimeStateLocked(exec)
		replacementPreserved := state.wsConn == conn2 && state.wsReaderConn == conn2 && state.skipBootstrapConn == conn2
		exec.Unlock()
		if replacementPreserved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for stale reader cleanup to preserve replacement websocket")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := serverConn2.WriteMessage(wsconn.TextMessage, []byte(`{"type":"session.created","event_id":"evt_replacement_bootstrap"}`)); err != nil {
		t.Fatalf("expected replacement bootstrap frame to be delivered, got %v", err)
	}
	if err := serverConn2.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_after_replacement","status":"completed"}}`)); err != nil {
		t.Fatalf("expected replacement response frame to be delivered, got %v", err)
	}

	outbound := recvCodexAttachmentOutbound(t, attachment)
	if got := string(outbound.payload); containsAll(got, "session.created") {
		t.Fatalf("expected replacement bootstrap frame to remain skipped after stale reader exit, got %q", got)
	}
	if got := string(outbound.payload); !containsAll(got, "response.completed", "resp_after_replacement") {
		t.Fatalf("expected first delivered frame after replacement to be the post-bootstrap response, got %q", got)
	}

	if outbound, err := recvCodexAttachmentOutboundWithTimeout(attachment, 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected stale reader cleanup to avoid leaking bootstrap on the replacement websocket, got %q err=%v", string(outbound.payload), err)
	}
}

func TestCodexManagedRealtimeReusesExecutionSessionWebsocket(t *testing.T) {
	var connections atomic.Int32
	server := newCodexRealtimeCountingServer(t, &connections)
	defer server.Close()

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-reuse-session",
	})
	providerA.Context.Set("token_id", 101)
	providerA.Channel.BaseURL = stringPtr(server.URL)

	sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime session to open, got %v", errWithCode)
	}
	sessionA.Detach("test_detach")

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-reuse-session",
	})
	providerB.Context.Set("token_id", 101)
	providerB.Channel.BaseURL = stringPtr(server.URL)

	sessionB, errWithCode := providerB.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime session to reopen, got %v", errWithCode)
	}
	defer sessionB.Detach("test_detach")

	waitForAtomicCount(t, &connections, 1, 2*time.Second, "expected reattach to reuse one upstream transport")
	if got := connections.Load(); got != 1 {
		t.Fatalf("reattach replaced the logical session instead of reusing its upstream transport: %d connections", got)
	}

	cleanupCodexManagedSession(t, providerB, "gpt-5")
}

func TestCodexManagedRealtimeStaleAbortDoesNotCloseReattachedExecutionSession(t *testing.T) {
	var connections atomic.Int32
	server := newCodexRealtimeCountingServer(t, &connections)
	defer server.Close()

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-stale-abort-reattach-session",
	})
	providerA.Context.Set("token_id", 131)
	providerA.Channel.BaseURL = stringPtr(server.URL)

	sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected initial managed realtime session to open, got %v", errWithCode)
	}
	managedA, ok := sessionA.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionA)
	}
	sessionA.Detach("test_detach")

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-stale-abort-reattach-session",
	})
	providerB.Context.Set("token_id", 131)
	providerB.Channel.BaseURL = stringPtr(server.URL)

	sessionB, errWithCode := providerB.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected reattached managed realtime session to open, got %v", errWithCode)
	}
	managedB, ok := sessionB.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionB)
	}
	defer managedB.Detach("test_close")
	defer cleanupCodexManagedSession(t, providerB, "gpt-5")

	waitForAtomicCount(t, &connections, 1, 2*time.Second, "expected stale abort scenario to retain one upstream transport")
	if got := connections.Load(); got != 1 {
		t.Fatalf("reattach unexpectedly replaced upstream transport: %d connections", got)
	}

	managedA.Abort("stale_abort")

	if binding, ok := resolveTestRealtimeBinding(providerB.Context); !ok || binding == nil {
		t.Fatal("expected stale abort on a reattached session not to delete the live binding")
	}

	managedB.exec.Lock()
	defer managedB.exec.Unlock()
	if managedB.exec.IsClosed() {
		t.Fatal("expected stale abort on a reattached session to leave the live execution session open")
	}
	if state := getCodexManagedRuntimeStateLocked(managedB.exec); state.attachment != managedB.attachment {
		t.Fatal("expected stale abort on a reattached session not to clear current attachment ownership")
	}
}

func TestCodexManagedRealtimeStaleObserverFactoryCannotMutateReattachedSession(t *testing.T) {
	var connections atomic.Int32
	server := newCodexRealtimeCountingServer(t, &connections)
	defer server.Close()

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-stale-observer-reattach-session",
	})
	providerA.Context.Set("token_id", 132)
	providerA.Channel.BaseURL = stringPtr(server.URL)

	sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected initial managed realtime session to open, got %v", errWithCode)
	}
	managedA, ok := sessionA.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionA)
	}
	sessionA.Detach("test_detach")

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-stale-observer-reattach-session",
	})
	providerB.Context.Set("token_id", 132)
	providerB.Channel.BaseURL = stringPtr(server.URL)

	sessionB, errWithCode := providerB.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected reattached managed realtime session to open, got %v", errWithCode)
	}
	managedB, ok := sessionB.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionB)
	}
	defer managedB.Detach("test_close")
	defer cleanupCodexManagedSession(t, providerB, "gpt-5")

	waitForAtomicCount(t, &connections, 1, 2*time.Second, "expected observer takeover to retain one upstream transport")
	if got := connections.Load(); got != 1 {
		t.Fatalf("reattach unexpectedly replaced upstream transport: %d connections", got)
	}

	staleRecorder := &recordingTurnObserver{}
	managedA.SetTurnObserverFactory(func() runtimesession.TurnObserver { return staleRecorder })

	managedB.exec.Lock()
	defer managedB.exec.Unlock()
	state := getCodexManagedRuntimeStateLocked(managedB.exec)
	if state.turnObserverFactory != nil {
		t.Fatal("expected stale session not to overwrite the live turn observer factory")
	}
	if state.turnObserver != nil {
		t.Fatal("expected stale session not to install a live turn observer")
	}
}

func TestCodexManagedRealtimeDetachedOwnerCanAbortBeforeReattach(t *testing.T) {
	upstreamURL := codexIdleWSTestServer(t)
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-detached-abort-session",
	})
	provider.Channel.BaseURL = stringPtr(upstreamURL)
	provider.Context.Set("token_id", 133)

	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime session to open, got %v", errWithCode)
	}
	managed, ok := session.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", session)
	}

	session.Detach("test_detach")
	managed.Abort("detached_abort")

	if binding, ok := resolveTestRealtimeBinding(provider.Context); ok || binding != nil {
		t.Fatalf("expected detached owner abort to remove the managed binding, got %+v", binding)
	}

	reopened, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected detached owner abort to allow a fresh reopen, got %v", errWithCode)
	}
	reopened.Abort("test_cleanup")
}

func TestCodexManagedRealtimeReattachDoesNotInheritPriorObserverFactory(t *testing.T) {
	upstreamURL := codexIdleWSTestServer(t)
	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-reattach-observer-reset-session",
	})
	providerA.Channel.BaseURL = stringPtr(upstreamURL)
	providerA.Context.Set("token_id", 134)

	sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected initial managed realtime session to open, got %v", errWithCode)
	}
	sessionA.SetTurnObserverFactory(func() runtimesession.TurnObserver { return &recordingTurnObserver{} })
	sessionA.Detach("test_detach")

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-reattach-observer-reset-session",
	})
	providerB.Channel.BaseURL = stringPtr(upstreamURL)
	providerB.Context.Set("token_id", 134)

	sessionB, errWithCode := providerB.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected reattached managed realtime session to open, got %v", errWithCode)
	}
	managedB, ok := sessionB.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionB)
	}
	defer managedB.Detach("test_close")
	defer cleanupCodexManagedSession(t, providerB, "gpt-5")

	managedB.exec.Lock()
	defer managedB.exec.Unlock()
	state := getCodexManagedRuntimeStateLocked(managedB.exec)
	if state.turnObserverFactory != nil {
		t.Fatal("expected reattached session not to inherit the prior observer factory")
	}
	if state.turnObserver != nil {
		t.Fatal("expected idle reattached session not to inherit the prior turn observer")
	}
}

func TestCodexManagedRealtimeRejectsExecutionSessionReuseAcrossChannelsWithoutSharedPool(t *testing.T) {
	var connections atomic.Int32
	server := newCodexRealtimeCountingServer(t, &connections)
	defer server.Close()

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-cross-channel-session",
	})
	providerA.Context.Set("token_id", 112)
	providerA.Channel.BaseURL = stringPtr(server.URL)

	sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected initial managed realtime session to open, got %v", errWithCode)
	}
	managedA, ok := sessionA.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionA)
	}
	defer managedA.Abort("test_cleanup_a")
	sessionA.Detach("test_detach")

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-cross-channel-session",
	})
	providerB.Context.Set("token_id", 112)
	providerB.Channel.Id = 424300
	providerB.Channel.BaseURL = stringPtr(server.URL)

	sessionB, errWithCode := providerB.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected reconnect across different channels to open a fresh execution session, got %v", errWithCode)
	}
	managedB, ok := sessionB.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionB)
	}
	defer managedB.Abort("test_cleanup_b")

	if managedB.exec.Key == managedA.exec.Key {
		t.Fatal("expected incompatible reconnect to mint a fresh execution session key")
	}
	if binding, ok := resolveTestRealtimeBinding(providerB.Context); !ok || binding == nil || binding.SessionKey != managedB.exec.Key {
		t.Fatalf("expected binding to move onto the fresh execution session, got %+v", binding)
	}
	waitForAtomicCount(t, &connections, 2, 2*time.Second, "expected fresh reconnect to open a second upstream websocket")
	if managedA.exec.IsClosed() {
		t.Fatal("expected incompatible reconnect not to force-close the original execution session")
	}
}

func TestCodexManagedRealtimeRejectsExecutionSessionReuseAcrossDifferentModelHeadersWithinSameChannel(t *testing.T) {
	var connections atomic.Int32
	server := newCodexRealtimeCountingServer(t, &connections)
	defer server.Close()

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-cross-header-policy-session",
	})
	providerA.Context.Set("token_id", 213)
	providerA.Channel.BaseURL = stringPtr(server.URL)
	providerA.Channel.ModelHeaders = stringPtr(`{"x-codex-beta-features":"feature-a"}`)

	sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected initial managed realtime session to open, got %v", errWithCode)
	}
	managedA, ok := sessionA.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionA)
	}
	defer managedA.Abort("test_cleanup_a")
	sessionA.Detach("test_detach")

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-cross-header-policy-session",
	})
	providerB.Context.Set("token_id", 213)
	providerB.Channel.BaseURL = stringPtr(server.URL)
	providerB.Channel.ModelHeaders = stringPtr(`{"x-codex-beta-features":"feature-b"}`)

	sessionB, errWithCode := providerB.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected reconnect with different model header policy to open a fresh execution session, got %v", errWithCode)
	}
	managedB, ok := sessionB.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionB)
	}
	defer managedB.Abort("test_cleanup_b")

	if managedB.exec.Key == managedA.exec.Key {
		t.Fatal("expected different model header policy to force a fresh execution session key")
	}
	if binding, ok := resolveTestRealtimeBinding(providerB.Context); !ok || binding == nil || binding.SessionKey != managedB.exec.Key {
		t.Fatalf("expected binding to move onto the fresh execution session, got %+v", binding)
	}
	waitForAtomicCount(t, &connections, 2, 2*time.Second, "expected fresh reconnect to open a second upstream websocket")
}

func TestCodexManagedRealtimeRejectsExecutionSessionReuseAcrossDifferentRequestHandshakeHeadersWithinSameChannel(t *testing.T) {
	testCases := []struct {
		name      string
		headerKey string
		valueA    string
		valueB    string
	}{
		{
			name:      "version",
			headerKey: "Version",
			valueA:    "2026-03-28",
			valueB:    "2026-03-29",
		},
		{
			name:      "originator",
			headerKey: "Originator",
			valueA:    "codex-cli-a",
			valueB:    "codex-cli-b",
		},
		{
			name:      "beta_features",
			headerKey: "X-Codex-Beta-Features",
			valueA:    "feature-a",
			valueB:    "feature-b",
		},
		{
			name:      "turn_state",
			headerKey: "X-Codex-Turn-State",
			valueA:    "state-a",
			valueB:    "state-b",
		},
		{
			name:      "timing_metrics",
			headerKey: "X-ResponsesAPI-Include-Timing-Metrics",
			valueA:    "true",
			valueB:    "false",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var connections atomic.Int32
			server := newCodexRealtimeCountingServer(t, &connections)
			defer server.Close()

			sessionID := fmt.Sprintf("managed-request-header-session-%s", testCase.name)

			providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
				"X-Session-Id":     sessionID,
				testCase.headerKey: testCase.valueA,
			})
			providerA.Context.Set("token_id", 215)
			providerA.Channel.BaseURL = stringPtr(server.URL)

			sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5")
			if errWithCode != nil {
				t.Fatalf("expected initial managed realtime session to open, got %v", errWithCode)
			}
			managedA, ok := sessionA.(*codexManagedRealtimeSession)
			if !ok {
				t.Fatalf("expected managed realtime session type, got %T", sessionA)
			}
			defer managedA.Abort("test_cleanup_a")
			sessionA.Detach("test_detach")

			providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
				"X-Session-Id":     sessionID,
				testCase.headerKey: testCase.valueB,
			})
			providerB.Context.Set("token_id", 215)
			providerB.Channel.BaseURL = stringPtr(server.URL)

			sessionB, errWithCode := providerB.OpenRealtimeSession("gpt-5")
			if errWithCode != nil {
				t.Fatalf("expected reconnect with different %s handshake header to open a fresh execution session, got %v", testCase.headerKey, errWithCode)
			}
			managedB, ok := sessionB.(*codexManagedRealtimeSession)
			if !ok {
				t.Fatalf("expected managed realtime session type, got %T", sessionB)
			}
			defer managedB.Abort("test_cleanup_b")
			if managedB.exec.Key == managedA.exec.Key {
				t.Fatalf("expected different %s handshake header to force a fresh execution session key", testCase.headerKey)
			}
			if binding, ok := resolveTestRealtimeBinding(providerB.Context); !ok || binding == nil || binding.SessionKey != managedB.exec.Key {
				t.Fatalf("expected binding to move onto the fresh execution session, got %+v", binding)
			}
			waitForAtomicCount(t, &connections, 2, 2*time.Second, "expected fresh reconnect to open a second upstream websocket")
		})
	}
}

func TestCodexManagedRealtimeRejectsExecutionSessionReuseAcrossDifferentChannelUserAgentsWithinSameChannel(t *testing.T) {
	var connections atomic.Int32
	server := newCodexRealtimeCountingServer(t, &connections)
	defer server.Close()

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-cross-channel-ua-session",
	})
	providerA.Context.Set("token_id", 214)
	providerA.Channel.BaseURL = stringPtr(server.URL)
	providerA.Channel.ModelHeaders = stringPtr(`{"User-Agent":"channel-codex-ua-a"}`)

	sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected initial managed realtime session to open, got %v", errWithCode)
	}
	managedA, ok := sessionA.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionA)
	}
	defer managedA.Abort("test_cleanup_a")
	sessionA.Detach("test_detach")

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-cross-channel-ua-session",
	})
	providerB.Context.Set("token_id", 214)
	providerB.Channel.BaseURL = stringPtr(server.URL)
	providerB.Channel.ModelHeaders = stringPtr(`{"User-Agent":"channel-codex-ua-b"}`)

	sessionB, errWithCode := providerB.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected reconnect with different channel user agent policy to open a fresh execution session, got %v", errWithCode)
	}
	managedB, ok := sessionB.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionB)
	}
	defer managedB.Abort("test_cleanup_b")

	if managedB.exec.Key == managedA.exec.Key {
		t.Fatal("expected different channel user agent policy to force a fresh execution session key")
	}
	if binding, ok := resolveTestRealtimeBinding(providerB.Context); !ok || binding == nil || binding.SessionKey != managedB.exec.Key {
		t.Fatalf("expected binding to move onto the fresh execution session, got %+v", binding)
	}
	waitForAtomicCount(t, &connections, 2, 2*time.Second, "expected fresh reconnect to open a second upstream websocket")
}

func TestCodexManagedRealtimeRejectsExecutionSessionReuseAcrossDifferentBaseURLs(t *testing.T) {
	var connectionsA atomic.Int32
	var connectionsB atomic.Int32

	newServer := func(counter *atomic.Int32) *httptest.Server {
		return newCodexRealtimeCountingServer(t, counter)
	}

	serverA := newServer(&connectionsA)
	defer serverA.Close()
	serverB := newServer(&connectionsB)
	defer serverB.Close()

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-cross-upstream-session",
	})
	providerA.Context.Set("token_id", 113)
	providerA.Channel.BaseURL = stringPtr(serverA.URL)

	sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected initial managed realtime session to open, got %v", errWithCode)
	}
	managedA, ok := sessionA.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionA)
	}
	defer managedA.Abort("test_cleanup_a")
	sessionA.Detach("test_detach")

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-cross-upstream-session",
	})
	providerB.Context.Set("token_id", 113)
	providerB.Channel.Id = 424301
	providerB.Channel.BaseURL = stringPtr(serverB.URL)

	sessionB, errWithCode := providerB.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected reconnect against a different base url to open a fresh execution session, got %v", errWithCode)
	}
	managedB, ok := sessionB.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionB)
	}
	defer managedB.Abort("test_cleanup_b")

	if managedB.exec.Key == managedA.exec.Key {
		t.Fatal("expected different base url to force a fresh execution session key")
	}
	if binding, ok := resolveTestRealtimeBinding(providerB.Context); !ok || binding == nil || binding.SessionKey != managedB.exec.Key {
		t.Fatalf("expected binding to move onto the fresh execution session, got %+v", binding)
	}
	waitForAtomicCount(t, &connectionsA, 1, 2*time.Second, "expected initial upstream websocket connection")
	waitForAtomicCount(t, &connectionsB, 1, 2*time.Second, "expected fresh reconnect to open a websocket against the new base url")
	if managedA.exec.IsClosed() {
		t.Fatal("expected incompatible reconnect not to force-close the original execution session")
	}
}

func TestCodexManagedRealtimeRejectsExecutionSessionReuseAcrossDifferentUpstreamCredentials(t *testing.T) {
	var connections atomic.Int32
	server := newCodexRealtimeCountingServer(t, &connections)
	defer server.Close()

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token-a","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-cross-credential-session",
	})
	providerA.Context.Set("token_id", 114)
	providerA.Channel.BaseURL = stringPtr(server.URL)

	sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected initial managed realtime session to open, got %v", errWithCode)
	}
	managedA, ok := sessionA.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionA)
	}
	defer managedA.Abort("test_cleanup_a")
	sessionA.Detach("test_detach")

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token-b","account_id":"acct-456"}`, "", map[string]string{
		"X-Session-Id": "managed-cross-credential-session",
	})
	providerB.Context.Set("token_id", 114)
	providerB.Channel.BaseURL = stringPtr(server.URL)

	sessionB, errWithCode := providerB.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected reconnect with different upstream credentials to open a fresh execution session, got %v", errWithCode)
	}
	managedB, ok := sessionB.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionB)
	}
	defer managedB.Abort("test_cleanup_b")

	if managedB.exec.Key == managedA.exec.Key {
		t.Fatal("expected different upstream credentials to force a fresh execution session key")
	}
	if binding, ok := resolveTestRealtimeBinding(providerB.Context); !ok || binding == nil || binding.SessionKey != managedB.exec.Key {
		t.Fatalf("expected binding to move onto the fresh execution session, got %+v", binding)
	}
	waitForAtomicCount(t, &connections, 2, 2*time.Second, "expected fresh reconnect to open a second upstream websocket")
}

func TestCodexManagedRealtimeReclaimsAttachedExecutionSession(t *testing.T) {
	var connections atomic.Int32
	server := newCodexRealtimeCountingServer(t, &connections)
	defer server.Close()

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-takeover-session",
	})
	providerA.Context.Set("token_id", 109)
	providerA.Channel.BaseURL = stringPtr(server.URL)

	sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected initial managed realtime session to open, got %v", errWithCode)
	}

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-takeover-session",
	})
	providerB.Context.Set("token_id", 109)
	providerB.Channel.BaseURL = stringPtr(server.URL)

	sessionB, errWithCode := providerB.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected reconnect to reclaim attached execution session, got %v", errWithCode)
	}
	defer sessionB.Detach("test_detach")
	defer cleanupCodexManagedSession(t, providerB, "gpt-5")

	waitForAtomicCount(t, &connections, 1, 2*time.Second, "expected reconnect to reuse upstream websocket")
	if got := connections.Load(); got != 1 {
		t.Fatalf("expected reconnect to reuse upstream websocket, got %d connections", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _, _, _, err := codexTestRecv(ctx, sessionA)
	if !errors.Is(err, runtimerealtime.ErrSessionClosed) {
		t.Fatalf("expected reclaimed attachment to close the stale session, got %v", err)
	}
}

func TestCodexManagedRealtimeDifferentModelsUseDistinctTransports(t *testing.T) {
	var connections atomic.Int32
	server := newCodexRealtimeCountingServer(t, &connections)
	defer server.Close()

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-alias-session",
	})
	providerA.Context.Set("token_id", 107)
	providerA.Channel.BaseURL = stringPtr(server.URL)

	sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5-mini")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime session to open with aliased model, got %v", errWithCode)
	}
	sessionA.Detach("test_detach")

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-alias-session",
	})
	providerB.Context.Set("token_id", 107)
	providerB.Channel.BaseURL = stringPtr(server.URL)

	sessionB, errWithCode := providerB.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected a distinct managed realtime session for a different model, got %v", errWithCode)
	}
	defer sessionB.Detach("test_detach")

	waitForAtomicCount(t, &connections, 2, 2*time.Second, "expected distinct compatibility to open two upstream transports")
	if got := connections.Load(); got != 2 {
		t.Fatalf("distinct model compatibility unexpectedly shared upstream transport: %d connections", got)
	}

	cleanupCodexManagedSession(t, providerB, "gpt-5")
}

func TestCodexManagedRealtimeWebsocketPreservesRawCreate(t *testing.T) {
	eventCh := make(chan []byte, 1)
	errCh := make(chan error, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		defer conn.Close()

		_, payload, err := conn.ReadMessage()
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
			return
		}

		select {
		case eventCh <- payload:
		default:
		}

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-normalize-session",
	})
	provider.Context.Set("token_id", 105)
	provider.Channel.BaseURL = stringPtr(server.URL)

	session, errWithCode := provider.OpenRealtimeSession("gpt-5-mini")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime websocket session to open, got %v", errWithCode)
	}
	defer session.Detach("test_close")
	defer cleanupCodexManagedSession(t, provider, "gpt-5-mini")

	createEvent := []byte(`{ "type":"response.create", "event_id":"evt_raw", "model":"gpt-5-mini", "input":"hello", "store":true, "include":"output_text.annotations", "temperature":2e-1, "top_p":0.9, "truncation":"auto", "context_management":{"mode":"manual"}, "tools":[{"type":"web_search_preview"}], "tool_choice":{"type":"web_search_preview_2025_03_11"}, "future":{"n":1e3}, "future":{"n":2e3} }`)
	if err := session.SendClient(context.Background(), codexTestTextFrame(createEvent)); err != nil {
		t.Fatalf("expected websocket dispatch to succeed, got %v", err)
	}

	var event []byte
	select {
	case err := <-errCh:
		t.Fatalf("expected upstream websocket to capture request, got %v", err)
	case event = <-eventCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for upstream websocket request")
	}

	if !bytes.Equal(event, createEvent) {
		t.Fatalf("native WS request was rewritten: got=%s want=%s", event, createEvent)
	}
}

func TestCodexManagedRealtimeHandshakeFailureDoesNotFallbackToHTTP(t *testing.T) {
	var upgrades, httpRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			upgrades.Add(1)
		} else {
			httpRequests.Add(1)
		}
		http.Error(w, "websocket disabled", http.StatusBadRequest)
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-force-session",
	})
	provider.Context.Set("token_id", 103)
	provider.Channel.BaseURL = stringPtr(server.URL)

	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode == nil {
		session.Detach("test_close")
		t.Fatal("expected websocket handshake failure to surface")
	}
	if session != nil || upgrades.Load() != 1 || httpRequests.Load() != 0 {
		t.Fatalf("handshake failure must not open a replacement transport: session=%T upgrades=%d HTTP=%d", session, upgrades.Load(), httpRequests.Load())
	}
	if binding, ok := resolveTestRealtimeBinding(provider.Context); ok || binding != nil {
		t.Fatalf("failed handshake left a session binding: %+v", binding)
	}

	cleanupCodexManagedSession(t, provider, "gpt-5")
}

func TestCodexManagedRealtimeCancelUsesSameWebsocketAndProviderTerminal(t *testing.T) {
	var upgrades, httpRequests atomic.Int32
	frames := make(chan []byte, 2)
	allowTerminal := make(chan struct{})
	releaseTerminal := sync.OnceFunc(func() { close(allowTerminal) })
	defer releaseTerminal()
	terminal := []byte(`{"type":"response.cancelled","response":{"id":"resp_cancel","status":"cancelled","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			httpRequests.Add(1)
			http.Error(w, "HTTP is not a websocket transport", http.StatusBadRequest)
			return
		}
		upgrades.Add(1)
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		defer conn.Close()
		for range 2 {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			frames <- payload
		}
		<-allowTerminal
		if err := conn.WriteMessage(wsconn.TextMessage, terminal); err != nil {
			return
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, nil)
	provider.Context.Set("token_id", 138)
	provider.Channel.BaseURL = stringPtr(server.URL)
	session, apiErr := provider.OpenRealtimeSession("gpt-5")
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	defer session.Abort("test_cleanup")
	recorder := &recordingTurnObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })
	create := []byte(`{"type":"response.create","event_id":"evt_create","model":"gpt-5","input":"hello"}`)
	cancelFrame := []byte(`{ "type":"response.cancel", "event_id":"evt_cancel", "future":{"n":1e3} }`)
	for _, raw := range [][]byte{create, cancelFrame} {
		if err := session.SendClient(t.Context(), codexTestTextFrame(raw)); err != nil {
			t.Fatal(err)
		}
		select {
		case received := <-frames:
			if !bytes.Equal(received, raw) {
				t.Fatalf("native WS frame was rewritten: got=%s want=%s", received, raw)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("upstream did not receive the frame on the original websocket")
		}
	}
	if recorder.finalizeCount() != 0 {
		t.Fatal("local cancel must wait for provider terminal before finalizing")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	_, payload, _, _, err := codexTestRecv(ctx, session)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("local cancel fabricated a provider event: payload=%s err=%v", payload, err)
	}
	releaseTerminal()
	ctx, cancel = context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, payload, usage, _, err := codexTestRecv(ctx, session)
	if err != nil || !bytes.Equal(payload, terminal) || usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("provider terminal was not preserved: payload=%s usage=%+v err=%v", payload, usage, err)
	}
	session.Abort("test_cleanup")
	if recorder.finalizeCount() != 1 || recorder.observeCount() != 1 || upgrades.Load() != 1 || httpRequests.Load() != 0 {
		t.Fatalf("unexpected cancel lifecycle: finalized=%d observed=%d WS=%d HTTP=%d", recorder.finalizeCount(), recorder.observeCount(), upgrades.Load(), httpRequests.Load())
	}
}

func TestCodexManagedRealtimeFailedInitialOpenDoesNotLeaveStaleBinding(t *testing.T) {
	failedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "websocket disabled", http.StatusBadRequest)
	}))
	defer failedServer.Close()

	var successfulConnections atomic.Int32
	successServer := newCodexRealtimeCountingServer(t, &successfulConnections)
	defer successServer.Close()

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-open-cleanup-session",
	})
	providerA.Context.Set("token_id", 116)
	providerA.Channel.BaseURL = stringPtr(failedServer.URL)
	defer cleanupCodexManagedSession(t, providerA, "gpt-5")

	session, errWithCode := providerA.OpenRealtimeSession("gpt-5")
	if errWithCode == nil {
		session.Detach("test_close")
		t.Fatalf("expected initial managed realtime open to fail when the websocket handshake fails")
	}
	if binding, ok := resolveTestRealtimeBinding(providerA.Context); ok || binding != nil {
		t.Fatalf("expected failed initial open to leave no managed binding, got %+v", binding)
	}

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-open-cleanup-session",
	})
	providerB.Context.Set("token_id", 116)
	providerB.Channel.BaseURL = stringPtr(successServer.URL)

	sessionB, errWithCode := providerB.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected retry after failed initial open to succeed on a replacement upstream, got %v", errWithCode)
	}
	defer sessionB.Detach("test_close")
	defer cleanupCodexManagedSession(t, providerB, "gpt-5")

	waitForAtomicCount(t, &successfulConnections, 1, 2*time.Second, "expected replacement upstream websocket connection after failed initial open")
}

func TestCodexManagedRealtimeAbortIgnoresStaleReplacedHandle(t *testing.T) {
	upstreamURL := codexIdleWSTestServer(t)
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-stale-abort-session",
	})
	provider.Channel.BaseURL = stringPtr(upstreamURL)
	provider.Context.Set("token_id", 117)

	sessionA, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected initial managed realtime session to open, got %v", errWithCode)
	}
	managedA, ok := sessionA.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionA)
	}

	managedA.exec.Lock()
	managedA.exec.MarkClosed("test_replaced")
	managedA.exec.Unlock()

	sessionB, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected replacement managed realtime session to open, got %v", errWithCode)
	}
	managedB, ok := sessionB.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionB)
	}
	defer managedB.Detach("test_close")
	defer cleanupCodexManagedSession(t, provider, "gpt-5")

	managedA.Abort("stale_abort")

	if binding, ok := resolveTestRealtimeBinding(provider.Context); !ok || binding == nil {
		t.Fatal("expected replacement session binding to survive stale abort")
	}

	managedB.exec.Lock()
	defer managedB.exec.Unlock()
	if managedB.exec.IsClosed() {
		t.Fatal("expected stale abort to leave replacement execution session alive")
	}
}

func TestCodexRealtimeForceFreshReplacesBindingWithoutStaleConflict(t *testing.T) {
	upstreamURL := codexIdleWSTestServer(t)
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "force-fresh-rebind-session",
	})
	provider.Channel.BaseURL = stringPtr(upstreamURL)
	provider.Context.Set("token_id", 118)

	sessionA, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected initial realtime session to open, got %v", errWithCode)
	}
	managedA, ok := sessionA.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionA)
	}
	sessionA.Detach("test_detach")

	oldKey := managedA.exec.Key
	oldUpstreamSessionID := managedA.exec.SessionID

	sessionB, errWithCode := provider.OpenRealtimeSessionWithOptions("gpt-5", runtimerealtime.RealtimeOpenOptions{
		ClientSessionID: "force-fresh-rebind-session",
		ForceFresh:      true,
	})
	if errWithCode != nil {
		t.Fatalf("expected force-fresh realtime session to open, got %v", errWithCode)
	}
	managedB, ok := sessionB.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionB)
	}
	defer managedB.Detach("test_close")
	defer cleanupCodexManagedSession(t, provider, "gpt-5")

	if managedB.exec.Key == oldKey {
		t.Fatalf("expected force-fresh open to replace execution session key %q", oldKey)
	}
	if managedB.exec.SessionID == oldUpstreamSessionID {
		t.Fatalf("expected force-fresh open to mint a new upstream session id, still got %q", oldUpstreamSessionID)
	}

	binding, ok := resolveTestRealtimeBinding(provider.Context)
	if !ok || binding == nil {
		t.Fatal("expected force-fresh open to publish a live binding")
	}
	if binding.SessionKey != managedB.exec.Key {
		t.Fatalf("expected binding to move onto new execution session key %q, got %q", managedB.exec.Key, binding.SessionKey)
	}
	if !managedA.exec.IsClosed() {
		t.Fatal("expected force-fresh open to close the stale execution session immediately")
	}
	if removed := currentCodexExecutionSessions().DeleteIf(oldKey, managedA.exec); removed != nil {
		t.Fatalf("expected stale execution session key %q to already be removed from the manager", oldKey)
	}
	if binding, ok := currentCodexExecutionSessions().Resolve(managedA.exec.BindingKey); !ok || binding == nil || binding.SessionKey != managedB.exec.Key {
		t.Fatalf("expected stale binding key to resolve only to the replacement execution session, got %+v", binding)
	}

	managedA.Abort("stale_abort")

	binding, ok = resolveTestRealtimeBinding(provider.Context)
	if !ok || binding == nil {
		t.Fatal("expected stale aborted handle not to remove replacement binding")
	}
	if binding.SessionKey != managedB.exec.Key {
		t.Fatalf("expected replacement binding to survive stale abort on key %q, got %q", managedB.exec.Key, binding.SessionKey)
	}
	if managedB.exec.IsClosed() {
		t.Fatal("expected replacement execution session to remain open after stale abort")
	}
}

func TestCodexRealtimeForceFreshReleasesPerCallerCapacity(t *testing.T) {
	upstreamURL := codexIdleWSTestServer(t)
	testManager := runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{
		DefaultTTL:           time.Minute,
		MaxSessions:          8,
		MaxSessionsPerCaller: 1,
		Cleanup:              cleanupCodexExecutionSession,
	})
	replaceCodexExecutionSessionsForTest(t, testManager)

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "force-fresh-capacity-session",
	})
	provider.Channel.BaseURL = stringPtr(upstreamURL)
	provider.Context.Set("token_id", 118)
	provider.Context.Set("id", 8001)

	sessionA, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected initial realtime session to open, got %v", errWithCode)
	}
	managedA, ok := sessionA.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionA)
	}
	sessionA.Detach("test_detach")

	sessionB, errWithCode := provider.OpenRealtimeSessionWithOptions("gpt-5", runtimerealtime.RealtimeOpenOptions{
		ClientSessionID: "force-fresh-capacity-session",
		ForceFresh:      true,
	})
	if errWithCode != nil {
		t.Fatalf("expected force-fresh reopen to reuse caller capacity, got %v", errWithCode)
	}
	managedB, ok := sessionB.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionB)
	}
	defer managedB.Abort("test_cleanup")

	if removed := currentCodexExecutionSessions().DeleteIf(managedA.exec.Key, managedA.exec); removed != nil {
		t.Fatalf("expected force-fresh reopen to free the stale execution session capacity slot, removed=%+v", removed)
	}

	otherSessionProvider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "force-fresh-capacity-session-other",
	})
	otherSessionProvider.Context.Set("token_id", 119)
	otherSessionProvider.Context.Set("id", 8001)

	_, errWithCode = otherSessionProvider.OpenRealtimeSession("gpt-5")
	if errWithCode == nil {
		t.Fatal("expected caller capacity to remain occupied by the replacement execution session")
	}
	if got := codexRealtimeErrorCodeString(errWithCode.Code, ""); got != "session_caller_capacity_exceeded" {
		t.Fatalf("expected session_caller_capacity_exceeded after replacement session claims capacity, got %q", got)
	}
	if errWithCode.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected caller capacity status 429, got %d", errWithCode.StatusCode)
	}
}

func TestCodexManagedRealtimePropagatesRealtimeHeaderErrors(t *testing.T) {
	var connections atomic.Int32
	server := newCodexRealtimeCountingServer(t, &connections)
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-auto-header-error-session",
	})
	provider.Context.Set("token_id", 117)
	provider.Channel.BaseURL = stringPtr(server.URL)
	defer cleanupCodexManagedSession(t, provider, "gpt-5")

	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode == nil {
		session.Detach("test_close")
		t.Fatal("expected realtime header errors to surface before dialing")
	}
	if got := codexRealtimeErrorCodeString(errWithCode.Code, ""); got != "codex_token_error" {
		t.Fatalf("expected codex_token_error, got %q", got)
	}
	if binding, ok := resolveTestRealtimeBinding(provider.Context); ok || binding != nil {
		t.Fatalf("expected failed open to leave no managed binding, got %+v", binding)
	}
	if got := connections.Load(); got != 0 {
		t.Fatalf("expected realtime header preflight errors to avoid websocket dials, got %d connections", got)
	}
}

func TestCodexManagedRealtimeRejectsConcurrentResponseCreate(t *testing.T) {
	upstreamURL := codexIdleWSTestServer(t)
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-busy-session",
	})
	provider.Channel.BaseURL = stringPtr(upstreamURL)
	provider.Context.Set("token_id", 104)

	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime session to open, got %v", errWithCode)
	}
	defer session.Detach("test_close")
	defer cleanupCodexManagedSession(t, provider, "gpt-5")

	createEvent := []byte(`{"type":"response.create","event_id":"evt_busy","model":"gpt-5","input":"hello"}`)
	if err := session.SendClient(context.Background(), codexTestTextFrame(createEvent)); err != nil {
		t.Fatalf("expected first response.create to succeed, got %v", err)
	}

	err := session.SendClient(context.Background(), codexTestTextFrame(createEvent))
	if err == nil {
		t.Fatalf("expected second inflight response.create to fail")
	}
	if got := err.Error(); got == "" || !containsAll(got, "session_busy") {
		t.Fatalf("expected session_busy error, got %q", got)
	}
}

func TestCodexManagedRealtimeReplacesWebsocketAfterConnectionScopedProviderError(t *testing.T) {
	var connections atomic.Int32
	firstConnectionClosed := make(chan struct{}, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		defer conn.Close()

		connectionID := connections.Add(1)

		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}

		if connectionID == 1 {
			if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"error","code":"upstream_failed","message":"upstream failed"}`)); err != nil {
				return
			}
			_, _, _ = conn.ReadMessage()
			select {
			case firstConnectionClosed <- struct{}{}:
			default:
			}
			return
		} else {
			if err := conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_recovered","status":"completed"}}`)); err != nil {
				return
			}
		}

		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-error-close-session",
	})
	provider.Context.Set("token_id", 106)
	provider.Channel.BaseURL = stringPtr(server.URL)

	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime websocket session to open, got %v", errWithCode)
	}
	defer session.Detach("test_close")
	defer cleanupCodexManagedSession(t, provider, "gpt-5")

	createEvent := []byte(`{"type":"response.create","event_id":"evt_error","model":"gpt-5","input":"hello"}`)
	if err := session.SendClient(context.Background(), codexTestTextFrame(createEvent)); err != nil {
		t.Fatalf("expected first websocket dispatch to succeed, got %v", err)
	}

	_, payload, _, _, err := codexTestRecv(context.Background(), session)
	if err != nil {
		t.Fatalf("expected provider error event after first dispatch, got %v", err)
	}
	if got := string(payload); !containsAll(got, "upstream_failed") {
		t.Fatalf("expected provider error event payload, got %q", got)
	}

	select {
	case <-firstConnectionClosed:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected response-unscoped provider error to close the upstream websocket")
	}

	if err := session.SendClient(context.Background(), codexTestTextFrame(createEvent)); err != nil {
		t.Fatalf("expected second websocket dispatch to open a replacement provider websocket, got %v", err)
	}

	_, payload, _, _, err = codexTestRecv(context.Background(), session)
	if err != nil {
		t.Fatalf("expected response from reused websocket, got %v", err)
	}
	if got := string(payload); !containsAll(got, "response.completed", "resp_recovered") {
		t.Fatalf("expected completed event from replacement websocket, got %q", got)
	}
	if got := connections.Load(); got != 2 {
		t.Fatalf("expected exactly one replacement websocket dial after connection-scoped provider error, got %d connections", got)
	}
}

func TestCodexManagedRealtimeObservesProviderInitiatedTurn(t *testing.T) {
	sendProviderTurn := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		defer conn.Close()
		<-sendProviderTurn
		_ = conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_provider_initiated","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`))
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-provider-initiated-session",
	})
	provider.Context.Set("token_id", 137)
	provider.Channel.BaseURL = stringPtr(server.URL)
	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime session to open, got %v", errWithCode)
	}
	defer session.Abort("test_cleanup")
	defer cleanupCodexManagedSession(t, provider, "gpt-5")

	recorder := &recordingTurnObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })
	close(sendProviderTurn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, payload, usage, _, err := codexTestRecv(ctx, session)
	if err != nil {
		t.Fatalf("expected provider-initiated terminal event, got %v", err)
	}
	if got := string(payload); !containsAll(got, "response.completed", "resp_provider_initiated") {
		t.Fatalf("unexpected provider-initiated payload %q", got)
	}
	if usage == nil || usage.TotalTokens != 8 {
		t.Fatalf("expected provider-initiated usage, got %+v", usage)
	}
	if recorder.providerInitiatedCount() != 1 || recorder.observeCount() != 1 || recorder.finalizeCount() != 1 {
		t.Fatalf("provider-initiated turn did not use one observation owner: admission=%d observed=%d finalized=%d", recorder.providerInitiatedCount(), recorder.observeCount(), recorder.finalizeCount())
	}
}

func TestCodexManagedRealtimeDetachClosesInflightWebsocket(t *testing.T) {
	var connections atomic.Int32
	createSeen := make(chan struct{}, 1)
	allowComplete := make(chan struct{}, 1)

	defer func() {
		select {
		case allowComplete <- struct{}{}:
		default:
		}
	}()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		connections.Add(1)
		go func() {
			defer conn.Close()

			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if !containsAll(string(payload), "response.create", `"model":"gpt-5"`) {
				t.Errorf("expected response.create payload, got %q", string(payload))
				return
			}

			select {
			case createSeen <- struct{}{}:
			default:
			}

			<-allowComplete
			_ = conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_after_reattach","status":"completed"}}`))

			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
	defer server.Close()

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-ws-reattach-session",
	})
	providerA.Context.Set("token_id", 110)
	providerA.Channel.BaseURL = stringPtr(server.URL)

	sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime websocket session to open, got %v", errWithCode)
	}

	createEvent := []byte(`{"type":"response.create","event_id":"evt_inflight","model":"gpt-5","input":"hello"}`)
	if err := sessionA.SendClient(context.Background(), codexTestTextFrame(createEvent)); err != nil {
		t.Fatalf("expected websocket dispatch to succeed, got %v", err)
	}

	select {
	case <-createSeen:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for upstream websocket request")
	}

	sessionA.Detach("test_detach")

	ctxClosed, cancelClosed := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelClosed()
	_, _, _, _, err := codexTestRecv(ctxClosed, sessionA)
	if !errors.Is(err, runtimerealtime.ErrSessionClosed) {
		t.Fatalf("expected detached websocket session to close locally, got %v", err)
	}

	allowComplete <- struct{}{}
	if got := connections.Load(); got != 1 {
		t.Fatalf("detach changed the logical execution transport count, got %d", got)
	}
}

func TestCodexRealtimeErrorFromOpenAIErrorFallsBackWhenCodeIsNil(t *testing.T) {
	err := codexRealtimeErrorFromOpenAIError("evt_nil_code", &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Code:    nil,
			Message: "upstream failed",
		},
	})

	event, ok := err.(*types.Event)
	if !ok {
		t.Fatalf("expected realtime error to be an event, got %T", err)
	}
	if event.ErrorDetail == nil {
		t.Fatalf("expected realtime error detail")
	}

	code, ok := event.ErrorDetail.Code.(string)
	if !ok {
		t.Fatalf("expected realtime error code string, got %T", event.ErrorDetail.Code)
	}
	if code != "provider_error" {
		t.Fatalf("expected fallback provider_error code, got %q", code)
	}
}

func TestCodexManagedRealtimeRecvPrefersBufferedFramesAfterAttachmentClose(t *testing.T) {
	attachment := newCodexAttachment()
	if ok := enqueueCodexOutbound(attachment, codexRealtimeOutbound{
		messageType: wsconn.TextMessage,
		payload:     []byte(`{"type":"response.completed"}`),
	}); !ok {
		t.Fatal("expected outbound frame to enqueue before attachment close")
	}
	attachment.close()

	session := &codexManagedRealtimeSession{attachment: attachment}
	messageType, payload, usage, _, err := codexTestRecv(context.Background(), session)
	if err != nil {
		t.Fatalf("expected buffered outbound to survive attachment close, got %v", err)
	}
	if messageType != wsconn.TextMessage {
		t.Fatalf("expected buffered websocket text payload, got %d", messageType)
	}
	if got := string(payload); got != `{"type":"response.completed"}` {
		t.Fatalf("expected buffered payload to survive attachment close, got %q", got)
	}
	if usage != nil {
		t.Fatalf("expected buffered payload not to invent usage, got %+v", usage)
	}

	_, payload, usage, _, err = codexTestRecv(context.Background(), session)
	if !errors.Is(err, runtimerealtime.ErrSessionClosed) {
		t.Fatalf("expected closed attachment to stop Recv after buffered outbound is drained, got %v", err)
	}
	if payload != nil {
		t.Fatalf("expected attachment close to stop Recv after buffered outbound is drained, got %q", string(payload))
	}
	if usage != nil {
		t.Fatalf("expected closed attachment to stop usage delivery after buffered outbound is drained, got %+v", usage)
	}
}

func TestEnqueueCodexOutboundRejectsClosedAttachment(t *testing.T) {
	attachment := newCodexAttachment()
	attachment.close()

	for attempt := 0; attempt < 64; attempt++ {
		if ok := enqueueCodexOutbound(attachment, codexRealtimeOutbound{
			messageType: wsconn.TextMessage,
			payload:     []byte(`{"type":"response.completed"}`),
		}); ok {
			t.Fatalf("expected closed attachment to reject outbound enqueue on attempt %d", attempt+1)
		}
	}

	_, err := recvCodexAttachmentOutboundWithTimeout(attachment, 50*time.Millisecond)
	if !errors.Is(err, runtimerealtime.ErrSessionClosed) {
		t.Fatalf("expected closed attachment to stay drained, got %v", err)
	}
}

func TestEnqueueCodexOutboundReservesTerminalWhenOrdinaryQueueIsFull(t *testing.T) {
	originalTimeout := codexRealtimeOutboundBackpressureTimeout
	codexRealtimeOutboundBackpressureTimeout = 20 * time.Millisecond
	defer func() {
		codexRealtimeOutboundBackpressureTimeout = originalTimeout
	}()

	attachment := newCodexAttachmentWithCapacity(1)
	if ok := enqueueCodexOutbound(attachment, codexRealtimeOutbound{
		messageType: wsconn.TextMessage,
		payload:     []byte(`{"type":"response.created"}`),
	}); !ok {
		t.Fatal("expected first outbound enqueue to succeed")
	}

	if ok := enqueueCodexOutbound(attachment, codexRealtimeOutbound{
		messageType: wsconn.TextMessage,
		payload:     []byte(`{"type":"response.completed"}`),
	}); !ok {
		t.Fatal("accepted terminal was dropped when the ordinary queue was full")
	}
	first := recvCodexAttachmentOutbound(t, attachment)
	second := recvCodexAttachmentOutbound(t, attachment)
	if !strings.Contains(string(first.payload), "response.created") || !strings.Contains(string(second.payload), "response.completed") {
		t.Fatalf("reserved terminal changed wire order: first=%s second=%s", first.payload, second.payload)
	}
	if attachment.isClosed() {
		t.Fatal("reserved terminal unnecessarily closed attachment")
	}
}

func TestCodexAttachmentByteBudgetReleasesOnConsumptionAndRejectsOversize(t *testing.T) {
	attachment := newCodexAttachmentWithLimits(2, 4)
	if ok := enqueueCodexOutbound(attachment, codexRealtimeOutbound{
		messageType: wsconn.TextMessage,
		payload:     []byte("four"),
	}); !ok {
		t.Fatal("expected payload within attachment byte budget")
	}
	if got := attachment.byteBudget.Used(); got != 4 {
		t.Fatalf("expected queued payload to own four bytes, got %d", got)
	}
	if got := string(recvCodexAttachmentOutbound(t, attachment).payload); got != "four" {
		t.Fatalf("unexpected consumed payload %q", got)
	}
	if got := attachment.byteBudget.Used(); got != 0 {
		t.Fatalf("attachment consumption leaked byte credit: %d", got)
	}

	oversize := newCodexAttachmentWithLimits(2, 3)
	if ok := enqueueCodexOutbound(oversize, codexRealtimeOutbound{
		messageType: wsconn.TextMessage,
		payload:     []byte("four"),
	}); ok {
		t.Fatal("attachment admitted a frame larger than its byte budget")
	}
	if !oversize.isClosed() || oversize.byteBudget.Used() != 0 {
		t.Fatalf("oversize failure did not close cleanly: closed=%v used=%d", oversize.isClosed(), oversize.byteBudget.Used())
	}
}

func TestCodexAttachmentTakeoverTransfersQueuedEventsInOrder(t *testing.T) {
	oldAttachment := newCodexAttachmentWithLimits(2, 128)
	replacement := newCodexAttachmentWithLimits(2, 128)
	for _, payload := range []string{`{"type":"response.created"}`, `{"type":"response.completed"}`} {
		if !enqueueCodexOutbound(oldAttachment, codexRealtimeOutbound{messageType: wsconn.TextMessage, payload: []byte(payload)}) {
			t.Fatalf("enqueue old attachment payload %s", payload)
		}
	}
	if !oldAttachment.takeoverTo(replacement) {
		t.Fatal("attachment takeover failed")
	}
	if got := oldAttachment.byteBudget.Used(); got != 0 {
		t.Fatalf("takeover leaked source byte credits: %d", got)
	}
	if got := replacement.byteBudget.Used(); got == 0 {
		t.Fatal("takeover did not acquire destination byte credits")
	}
	if !oldAttachment.isClosed() {
		t.Fatal("old physical attachment remained consumable after takeover")
	}
	first := recvCodexAttachmentOutbound(t, replacement)
	second := recvCodexAttachmentOutbound(t, replacement)
	if !strings.Contains(string(first.payload), "response.created") || !strings.Contains(string(second.payload), "response.completed") {
		t.Fatalf("takeover changed queued event order: first=%s second=%s", first.payload, second.payload)
	}
	if got := replacement.byteBudget.Used(); got != 0 {
		t.Fatalf("replacement consumption leaked byte credits: %d", got)
	}
}

func TestCodexManagedRealtimeNativeAbortFinalizesClaimedTurnOnce(t *testing.T) {
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "token:1/native-abort",
		SessionID: "native-abort",
		Model:     "gpt-5",
	})
	attachment := newCodexAttachment()
	recorder := &recordingTurnObserver{}
	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	ownerSeq := assignCodexAttachmentOwnerLocked(state, attachment)
	state.turnObserverFactory = func() runtimesession.TurnObserver { return recorder }
	beginCodexTurnLocked(state, time.Now())
	exec.Attached = true
	exec.Inflight = true
	exec.State = runtimesession.SessionStateActive
	exec.Unlock()

	session := &codexManagedRealtimeSession{exec: exec, attachment: attachment, ownerSeq: ownerSeq}
	session.Abort("client_backpressure")
	session.Abort("duplicate")
	if recorder.finalizeCount() != 1 || recorder.lastPayload().TerminationReason != "client_backpressure" {
		t.Fatalf("native Abort did not settle exactly once: count=%d payload=%+v", recorder.finalizeCount(), recorder.lastPayload())
	}
}

func TestCodexManagedRealtimeWebsocketCarriesUsageOnFailedTerminalEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}

		_ = conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.failed","response":{"id":"resp_failed","status":"failed","usage":{"input_tokens":3,"output_tokens":0,"total_tokens":3}}}`))
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-failed-usage-session",
	})
	provider.Context.Set("token_id", 115)
	provider.Channel.BaseURL = stringPtr(server.URL)

	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime websocket session to open, got %v", errWithCode)
	}
	defer session.Detach("test_close")
	defer cleanupCodexManagedSession(t, provider, "gpt-5")

	recorder := &recordingTurnObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })

	createEvent := []byte(`{"type":"response.create","event_id":"evt_failed","model":"gpt-5","input":"hello"}`)
	if err := session.SendClient(context.Background(), codexTestTextFrame(createEvent)); err != nil {
		t.Fatalf("expected websocket dispatch to succeed, got %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, payload, usage, _, err := codexTestRecv(ctx, session)
	if err != nil {
		t.Fatalf("expected failed terminal payload, got %v", err)
	}
	if got := string(payload); !containsAll(got, "response.failed", "resp_failed") {
		t.Fatalf("expected failed terminal payload, got %q", got)
	}
	if usage == nil || usage.TotalTokens != 3 || usage.InputTokens != 3 || usage.OutputTokens != 0 {
		t.Fatalf("expected failed terminal payload to carry prompt usage, got %+v", usage)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if recorder.finalizeCount() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for websocket turn finalization")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := recorder.observeCount(); got != 1 {
		t.Fatalf("expected failed websocket turn usage to be observed once, got %d", got)
	}
	finalizePayload := recorder.lastPayload()
	if finalizePayload.TerminationReason != "response.failed" {
		t.Fatalf("expected failed websocket turn to finalize with response.failed, got %q", finalizePayload.TerminationReason)
	}
	if finalizePayload.Usage == nil || finalizePayload.Usage.TotalTokens != 3 || finalizePayload.Usage.InputTokens != 3 {
		t.Fatalf("expected failed websocket turn to finalize with prompt usage, got %+v", finalizePayload.Usage)
	}
}

func TestCodexManagedRealtimeWebsocketRejectsConflictingImageIdentityBeforeDelivery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.output_item.done","item_id":"ws_1","output_index":0,"item":{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search"}}}`))
		_ = conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.output_item.done","item_id":"img_top","output_index":1,"item":{"id":"img_item","type":"image_generation_call","status":"completed","quality":"high","size":"1024x1024"}}`))
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-image-conflict-session",
	})
	provider.Context.Set("token_id", 117)
	provider.Channel.BaseURL = stringPtr(server.URL)
	session, apiErr := provider.OpenRealtimeSession("gpt-5")
	if apiErr != nil {
		t.Fatalf("open managed realtime session: %v", apiErr)
	}
	defer session.Detach("test_close")
	defer cleanupCodexManagedSession(t, provider, "gpt-5")
	recorder := &recordingTurnObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })

	if err := session.SendClient(context.Background(), codexTestTextFrame([]byte(`{"type":"response.create","event_id":"evt_image_conflict","model":"gpt-5","input":"hello"}`))); err != nil {
		t.Fatalf("send response.create: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, prefix, prefixUsage, prefixOrigin, err := codexTestRecv(ctx, session)
	toolKey := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	if err != nil || prefixOrigin != runtimerealtime.RealtimePayloadOriginProvider || !strings.Contains(string(prefix), "web_search_call") || prefixUsage == nil || prefixUsage.ExtraBilling[toolKey].CallCount != 1 || !prefixUsage.ProviderExtraBilling[toolKey] {
		t.Fatalf("unexpected accepted prefix search evidence: payload=%s usage=%+v origin=%v err=%v", prefix, prefixUsage, prefixOrigin, err)
	}
	_, errorPayload, errorUsage, errorOrigin, err := codexTestRecv(ctx, session)
	if err != nil || errorUsage != nil || errorOrigin != runtimerealtime.RealtimePayloadOriginProxyLocal ||
		!containsAll(string(errorPayload), `"type":"error"`, "provider_protocol_error") ||
		strings.Contains(string(errorPayload), "img_top") || strings.Contains(string(errorPayload), "img_item") {
		t.Fatalf("unexpected native tracker error event: payload=%s usage=%+v origin=%v err=%v", errorPayload, errorUsage, errorOrigin, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for recorder.finalizeCount() != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := recorder.finalizeCount(); got != 1 {
		t.Fatalf("expected native turn to finalize once, got %d", got)
	}
	finalized := recorder.lastPayload()
	if finalized.TerminationReason != "provider_protocol_error" || finalized.Usage == nil || finalized.Usage.ExtraBilling[toolKey].CallCount != 1 {
		t.Fatalf("expected prefix billing and protocol termination, got %+v", finalized)
	}
	managed := session.(*codexManagedRealtimeSession)
	managed.exec.Lock()
	state := getCodexManagedRuntimeStateLocked(managed.exec)
	websocketAttached := state.wsConn != nil
	inflight := managed.exec.Inflight
	sessionState := managed.exec.State
	managed.exec.Unlock()
	if websocketAttached || inflight || sessionState != runtimesession.SessionStateIdle {
		t.Fatalf("native state not cleared after tracker error: websocket=%v inflight=%v state=%s", websocketAttached, inflight, sessionState)
	}
}

func TestCodexManagedRealtimeWebsocketDoesNotSynthesizeMissingTerminalUsage(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() {
		config.DisableTokenEncoders = originalDisable
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}

		_ = conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_backfilled","status":"completed","tools":[{"type":"web_search_preview","search_context_size":"high"}],"output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello from websocket"}]},{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search"}}]}}`))
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-backfilled-usage-session",
	})
	provider.Context.Set("token_id", 116)
	provider.Channel.BaseURL = stringPtr(server.URL)

	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime websocket session to open, got %v", errWithCode)
	}
	defer session.Detach("test_close")
	defer cleanupCodexManagedSession(t, provider, "gpt-5")

	recorder := &recordingTurnObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })

	createEvent := []byte(`{"type":"response.create","event_id":"evt_backfilled","model":"gpt-5","input":"hello"}`)
	if err := session.SendClient(context.Background(), codexTestTextFrame(createEvent)); err != nil {
		t.Fatalf("expected websocket dispatch to succeed, got %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, payload, usage, _, err := codexTestRecv(ctx, session)
	if err != nil {
		t.Fatalf("expected completed terminal payload, got %v", err)
	}
	if got := string(payload); !containsAll(got, "response.completed", "resp_backfilled") {
		t.Fatalf("expected completed terminal payload, got %q", got)
	}
	if usage == nil || usage.InputTokens <= 0 || usage.OutputTokens != 0 || usage.TotalTokens != usage.InputTokens || usage.ProviderTokenEvidence {
		t.Fatalf("provider-missing websocket content produced token evidence: %+v", usage)
	}
	billing, ok := usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok || billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected completed websocket turn to preserve a single web search charge, got %+v", usage.ExtraBilling)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if recorder.finalizeCount() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for websocket turn finalization")
		}
		time.Sleep(10 * time.Millisecond)
	}

	finalizePayload := recorder.lastPayload()
	if finalizePayload.Usage == nil || finalizePayload.Usage.InputTokens <= 0 || finalizePayload.Usage.OutputTokens != 0 || finalizePayload.Usage.ProviderTokenEvidence {
		t.Fatalf("finalized websocket turn gained synthetic token evidence: %+v", finalizePayload.Usage)
	}
}

func TestCodexManagedRealtimeWebsocketDuplicateTerminalEventsDoNotDoubleBill(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}

		_ = conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_duplicate","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`))
		_ = conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_duplicate","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`))
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-duplicate-terminal-session",
	})
	provider.Context.Set("token_id", 117)
	provider.Channel.BaseURL = stringPtr(server.URL)

	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime websocket session to open, got %v", errWithCode)
	}
	defer session.Detach("test_close")
	defer cleanupCodexManagedSession(t, provider, "gpt-5")

	recorder := &recordingTurnObserver{}
	session.SetTurnObserverFactory(func() runtimesession.TurnObserver { return recorder })

	createEvent := []byte(`{"type":"response.create","event_id":"evt_duplicate","model":"gpt-5","input":"hello"}`)
	if err := session.SendClient(context.Background(), codexTestTextFrame(createEvent)); err != nil {
		t.Fatalf("expected websocket dispatch to succeed, got %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for _, want := range []string{"response.completed", "response.done"} {
		_, payload, _, _, err := codexTestRecv(ctx, session)
		if err != nil {
			t.Fatalf("expected duplicate terminal payload %q, got %v", want, err)
		}
		if got := string(payload); !containsAll(got, want, "resp_duplicate") {
			t.Fatalf("expected duplicate terminal payload %q, got %q", want, got)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if recorder.finalizeCount() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for duplicate-terminal finalization")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := recorder.observeCount(); got != 1 {
		t.Fatalf("expected duplicate terminal usage to be observed once, got %d", got)
	}
	if got := recorder.finalizeCount(); got != 1 {
		t.Fatalf("expected duplicate terminal events to finalize once, got %d", got)
	}
	finalizePayload := recorder.lastPayload()
	if finalizePayload.LastResponseID != "resp_duplicate" {
		t.Fatalf("expected finalized turn to preserve duplicate response id, got %q", finalizePayload.LastResponseID)
	}
	if finalizePayload.Usage == nil || finalizePayload.Usage.TotalTokens != 8 {
		t.Fatalf("expected finalized turn to preserve usage once, got %+v", finalizePayload.Usage)
	}
}

func TestCodexOpenRealtimeSessionReleasesLeaseOnModelMismatch(t *testing.T) {
	testManager := runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{
		DefaultTTL: time.Minute,
	})
	replaceCodexExecutionSessionsForTest(t, testManager)

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "managed-model-mismatch-session",
	})
	provider.Context.Set("token_id", 118)

	meta, errWithCode := provider.buildExecutionSessionMetadata("gpt-5", runtimerealtime.RealtimeOpenOptions{})
	if errWithCode != nil {
		t.Fatalf("expected execution session metadata, got %v", errWithCode)
	}

	exec, _, _, releaseLease, err := testManager.AcquireOrCreateBound(meta)
	if err != nil {
		t.Fatalf("expected execution session fixture, got %v", err)
	}
	exec.Lock()
	exec.Model = "o4-mini"
	exec.Attached = false
	exec.Inflight = false
	exec.State = runtimesession.SessionStateIdle
	exec.IdleTTL = time.Minute
	exec.Touch(time.Now())
	exec.Unlock()
	releaseLease()

	_, errWithCode = provider.OpenRealtimeSession("gpt-5")
	if errWithCode == nil {
		t.Fatal("expected model mismatch to fail session open")
	}
	if errWithCode.Code != "session_model_mismatch" {
		t.Fatalf("expected model mismatch code, got %q", errWithCode.Code)
	}

	exec.Lock()
	exec.Attached = false
	exec.Inflight = false
	exec.State = runtimesession.SessionStateIdle
	exec.IdleTTL = time.Millisecond
	exec.Touch(time.Now().Add(-time.Minute))
	exec.Unlock()

	if swept := testManager.Sweep(time.Now()); swept != 1 {
		t.Fatalf("expected mismatched execution session to expire after lease release, swept %d sessions", swept)
	}
	if binding, ok := testManager.Resolve(meta.BindingKey); ok {
		t.Fatalf("expected mismatched binding to be removed after sweep, got %+v", binding)
	}
}

func TestCodexDetachDeletesGeneratedExecutionSessionImmediately(t *testing.T) {
	upstreamURL := codexIdleWSTestServer(t)
	testManager := runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{
		DefaultTTL: time.Minute,
	})
	replaceCodexExecutionSessionsForTest(t, testManager)

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, nil)
	provider.Channel.BaseURL = stringPtr(upstreamURL)
	provider.Context.Set("token_id", 119)

	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime session to open, got %v", errWithCode)
	}
	managed, ok := session.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", session)
	}

	if managed.exec.ClientSuppliedID {
		t.Fatal("expected missing x-session-id to remain marked as server-generated after open")
	}
	if managed.exec.BindingKey != "" {
		t.Fatalf("expected missing x-session-id not to create a resumable binding, got %q", managed.exec.BindingKey)
	}

	managed.Detach("test_detach")
	waitForCodexAttachmentClosed(t, managed.attachment, time.Second)

	if removed := testManager.DeleteIf(managed.exec.Key, managed.exec); removed != nil {
		t.Fatalf("expected generated detached execution session to already be removed, got %+v", removed)
	}
}

func TestCodexDetachDeletesGeneratedExecutionSessionAfterInflightTurnCompletes(t *testing.T) {
	testManager := runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{
		DefaultTTL: time.Minute,
	})
	replaceCodexExecutionSessionsForTest(t, testManager)

	allowTerminalEvent := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, ok := acceptCodexRealtimeTestConn(t, w, r)
		if !ok {
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		<-allowTerminalEvent
		_ = conn.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_detached_terminal","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`))
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, nil)
	provider.Context.Set("token_id", 119)
	provider.Context.Set("id", 7001)
	provider.Channel.BaseURL = stringPtr(server.URL)

	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime session to open, got %v", errWithCode)
	}
	managed, ok := session.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", session)
	}

	if managed.exec.ClientSuppliedID {
		t.Fatal("expected missing x-session-id to remain marked as server-generated after open")
	}
	if managed.exec.BindingKey != "" {
		t.Fatalf("expected missing x-session-id not to create a resumable binding, got %q", managed.exec.BindingKey)
	}

	createEvent := []byte(`{"type":"response.create","event_id":"evt_detached_terminal","model":"gpt-5","input":"hello"}`)
	if err := session.SendClient(context.Background(), codexTestTextFrame(createEvent)); err != nil {
		t.Fatalf("expected websocket dispatch to succeed, got %v", err)
	}

	managed.Detach("test_detach")
	waitForCodexAttachmentClosed(t, managed.attachment, time.Second)
	close(allowTerminalEvent)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if managed.exec.IsClosed() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected generated detached inflight execution session to be deleted after terminal event")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if removed := testManager.DeleteIf(managed.exec.Key, managed.exec); removed != nil {
		t.Fatalf("expected generated detached inflight execution session to already be removed, got %+v", removed)
	}
}

func TestCodexOpenRealtimeSessionEnforcesPerCallerCapacity(t *testing.T) {
	upstreamURL := codexIdleWSTestServer(t)
	testManager := runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{
		DefaultTTL:           time.Minute,
		MaxSessions:          8,
		MaxSessionsPerCaller: 1,
	})
	replaceCodexExecutionSessionsForTest(t, testManager)

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "caller-cap-session-a",
	})
	providerA.Channel.BaseURL = stringPtr(upstreamURL)
	providerA.Context.Set("token_id", 120)
	providerA.Context.Set("id", 8001)

	sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected initial caller session to open, got %v", errWithCode)
	}
	defer sessionA.Abort("test_cleanup")

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "caller-cap-session-b",
	})
	providerB.Channel.BaseURL = stringPtr(upstreamURL)
	providerB.Context.Set("token_id", 121)
	providerB.Context.Set("id", 8001)

	_, errWithCode = providerB.OpenRealtimeSession("gpt-5")
	if errWithCode == nil {
		t.Fatal("expected second session for the same user capacity namespace to hit caller capacity")
	}
	if got := codexRealtimeErrorCodeString(errWithCode.Code, ""); got != "session_caller_capacity_exceeded" {
		t.Fatalf("expected session_caller_capacity_exceeded, got %q", got)
	}
	if errWithCode.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected caller capacity status 429, got %d", errWithCode.StatusCode)
	}

	providerC := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "caller-cap-session-c",
	})
	providerC.Channel.BaseURL = stringPtr(upstreamURL)
	providerC.Context.Set("token_id", 122)
	providerC.Context.Set("id", 8002)

	sessionC, errWithCode := providerC.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected a different user capacity namespace to retain independent capacity, got %v", errWithCode)
	}
	sessionC.Abort("test_cleanup")
}

type recordingTurnObserver struct {
	mu                     sync.Mutex
	observed               []*types.UsageEvent
	finalized              []runtimesession.TurnFinalizePayload
	providerInitiated      int
	providerInitiatedError error
}

func (r *recordingTurnObserver) ObserveProviderInitiatedTurn(runtimesession.TurnAdmission) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providerInitiated++
	return r.providerInitiatedError
}

func (r *recordingTurnObserver) ObserveTurnUsage(usage *types.UsageEvent) error {
	if r == nil || usage == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observed = append(r.observed, cloneTestUsageEvent(usage))
	return nil
}

func (r *recordingTurnObserver) FinalizeTurn(payload runtimesession.TurnFinalizePayload) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	payload.Usage = cloneTestUsageEvent(payload.Usage)
	r.finalized = append(r.finalized, payload)
}

func (r *recordingTurnObserver) observeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.observed)
}

func (r *recordingTurnObserver) finalizeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.finalized)
}

func (r *recordingTurnObserver) providerInitiatedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.providerInitiated
}

func (r *recordingTurnObserver) lastPayload() runtimesession.TurnFinalizePayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.finalized) == 0 {
		return runtimesession.TurnFinalizePayload{}
	}
	return r.finalized[len(r.finalized)-1]
}

type failingTurnObserver struct {
	recordingTurnObserver
	observeErr error
}

func (r *failingTurnObserver) ObserveTurnUsage(usage *types.UsageEvent) error {
	_ = r.recordingTurnObserver.ObserveTurnUsage(usage)
	return r.observeErr
}

func cloneTestUsageEvent(usage *types.UsageEvent) *types.UsageEvent {
	if usage == nil {
		return nil
	}

	cloned := *usage
	if usage.ExtraTokens != nil {
		cloned.ExtraTokens = make(map[string]int, len(usage.ExtraTokens))
		for key, value := range usage.ExtraTokens {
			cloned.ExtraTokens[key] = value
		}
	}
	if usage.ExtraBilling != nil {
		cloned.ExtraBilling = make(map[string]types.ExtraBilling, len(usage.ExtraBilling))
		for key, value := range usage.ExtraBilling {
			cloned.ExtraBilling[key] = value
		}
	}
	return &cloned
}

func TestCodexRealtimeReopenLocalOnlySessionPromotesToShared(t *testing.T) {
	upstreamURL := codexIdleWSTestServer(t)
	testManager := runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{
		DefaultTTL: time.Minute,
		Cleanup:    cleanupCodexExecutionSession,
	})
	replaceCodexExecutionSessionsForTest(t, testManager)

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{}`, map[string]string{
		"X-Session-Id": "local-only-promote-session",
	})
	provider.Channel.BaseURL = stringPtr(upstreamURL)
	provider.Context.Set("token_id", 118)

	meta, errWithCode := provider.buildExecutionSessionMetadata("gpt-5", runtimerealtime.RealtimeOpenOptions{})
	if errWithCode != nil {
		t.Fatalf("expected execution session metadata, got %v", errWithCode)
	}

	exec, created, releaseLease, err := testManager.AcquireOrCreate(meta)
	if err != nil {
		t.Fatalf("expected initial execution session, got %v", err)
	}
	if !created {
		t.Fatal("expected initial execution session creation")
	}
	exec.Lock()
	exec.Visibility = runtimesession.VisibilityLocalOnly
	exec.PublishIntent = runtimesession.PublishIntentCreateIfAbsent
	exec.Unlock()
	releaseLease()

	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected local-only reopen to succeed, got %v", errWithCode)
	}
	managed, ok := session.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", session)
	}
	defer managed.Abort("test_cleanup")

	if managed.exec != exec {
		t.Fatalf("expected reopen to reuse the local-only execution session instance")
	}
	managed.exec.Lock()
	defer managed.exec.Unlock()
	if managed.exec.Visibility != runtimesession.VisibilityShared {
		t.Fatalf("expected local-only reopen to promote execution session to shared, got %q", managed.exec.Visibility)
	}
	if managed.exec.PublishIntent != runtimesession.PublishIntentNone {
		t.Fatalf("expected promotion to clear publish intent, got %q", managed.exec.PublishIntent)
	}
}

func cleanupCodexManagedSession(t *testing.T, provider *CodexProvider, model string) {
	t.Helper()
	if binding, ok := resolveTestRealtimeBinding(provider.Context); ok && binding != nil {
		currentCodexExecutionSessions().Delete(binding.SessionKey)
		return
	}
	meta, errWithCode := provider.buildExecutionSessionMetadata(model, runtimerealtime.RealtimeOpenOptions{})
	if errWithCode != nil {
		return
	}
	currentCodexExecutionSessions().Delete(meta.Key)
}

func stringPtr(value string) *string {
	return &value
}

func containsAll(s string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(s, fragment) {
			return false
		}
	}
	return true
}

func countHeadersByKey(headers map[string]string, key string) int {
	count := 0
	for existingKey := range headers {
		if strings.EqualFold(existingKey, key) {
			count++
		}
	}
	return count
}

func waitForAtomicCount(t *testing.T, counter *atomic.Int32, want int32, timeout time.Duration, message string) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		if got := counter.Load(); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s, got %d want %d", message, counter.Load(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
