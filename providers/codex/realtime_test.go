package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/requester"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
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
	return codexExecutionSessions.Resolve(bindingKey)
}

func TestCodexRealtimeBootstrapMessageDetection(t *testing.T) {
	payload := []byte(`{"type":"session.created","session":{"id":"execution-session-456"}}`)
	if !isCodexRealtimeBootstrapMessage(websocket.TextMessage, payload) {
		t.Fatal("expected Codex realtime bootstrap detector to match session.created events")
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
			shouldContinue, usage, newMessage, err := provider.handleRealtimeSupplierMessage(websocket.TextMessage, testCase.payload, newCodexTurnUsageAccumulator(), "gpt-5")
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
			"output":[{"type":"web_search_call","id":"ws_123","status":"completed"}]
		}
	}`)

	shouldContinue, usage, _, err := provider.handleRealtimeSupplierMessage(websocket.TextMessage, payload, newCodexTurnUsageAccumulator(), "gpt-5")
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

func TestCodexRealtimeHandlerBackfillsMissingTerminalUsageFromSeededRequest(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() {
		config.DisableTokenEncoders = originalDisable
	})

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	var requestEvent codexRealtimeClientEvent
	err := json.Unmarshal([]byte(`{"type":"response.create","event_id":"evt_seed","response":{"model":"gpt-5","input":"hello"}}`), &requestEvent)
	if err != nil {
		t.Fatalf("expected realtime request seed payload to parse, got %v", err)
	}
	accumulator := newCodexTurnUsageAccumulator()
	accumulator.SeedPromptFromRequest(requestEvent.Response, provider.Channel.PreCost)

	payload := []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_seeded",
			"status":"completed",
			"tools":[{"type":"web_search_preview","search_context_size":"high"}],
			"output":[
				{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello from realtime"}]},
				{"id":"ws_1","type":"web_search_call","status":"completed"}
			]
		}
	}`)

	shouldContinue, usage, _, err := provider.handleRealtimeSupplierMessage(websocket.TextMessage, payload, accumulator, "gpt-5")
	if err != nil {
		t.Fatalf("expected no handler error, got %v", err)
	}
	if !shouldContinue {
		t.Fatal("expected terminal response to keep the stream alive")
	}
	if usage == nil || usage.InputTokens <= 0 || usage.OutputTokens <= 0 || usage.TotalTokens <= usage.InputTokens {
		t.Fatalf("expected terminal realtime usage to be backfilled from request seed and output content, got %+v", usage)
	}
	billing, ok := usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected backfilled realtime usage to preserve web search extra billing, got %+v", usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected a single high web search tool charge, got %+v", billing)
	}
}

func TestCodexRealtimeHandlerIgnoresUsageOnNonTerminalResponseEvents(t *testing.T) {
	provider := &CodexProvider{}
	payload := []byte(`{"type":"response.created","response":{"status":"in_progress","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`)

	shouldContinue, usage, newMessage, err := provider.handleRealtimeSupplierMessage(websocket.TextMessage, payload, newCodexTurnUsageAccumulator(), "gpt-5")
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
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created","event_id":"evt_bootstrap"}`)); err != nil {
			return
		}

		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}

		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_after_bootstrap","status":"completed"}}`))
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, map[string]string{
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

	createEvent := []byte(`{"type":"response.create","event_id":"evt_create","response":{"model":"gpt-5","input":"hello"}}`)
	if err := session.SendClient(context.Background(), websocket.TextMessage, createEvent); err != nil {
		t.Fatalf("expected websocket dispatch to succeed, got %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, payload, _, err := session.Recv(ctx)
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
	upgrader := websocket.Upgrader{}
	accepted := make(chan *websocket.Conn, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		accepted <- conn
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn1, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("expected first websocket dial to succeed, got %v", err)
	}
	defer conn1.Close()

	serverConn1 := <-accepted
	defer serverConn1.Close()

	conn2, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("expected replacement websocket dial to succeed, got %v", err)
	}
	defer conn2.Close()

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
	exec.Transport = runtimesession.TransportModeResponsesWS
	provider.startRealtimeWSReaderLocked(exec, state)
	replaced := clearCodexManagedWebsocketLocked(state)
	if replaced != conn1 {
		exec.Unlock()
		t.Fatalf("expected websocket replacement to clear the original connection")
	}
	state.wsConn = conn2
	state.skipBootstrapConn = conn2
	provider.startRealtimeWSReaderLocked(exec, state)
	exec.Unlock()

	_ = serverConn1.Close()
	time.Sleep(100 * time.Millisecond)

	exec.Lock()
	state = getCodexManagedRuntimeStateLocked(exec)
	if state.wsConn != conn2 {
		exec.Unlock()
		t.Fatalf("expected replacement websocket to remain current")
	}
	if state.wsReaderConn != conn2 {
		exec.Unlock()
		t.Fatalf("expected stale reader cleanup to preserve the replacement reader owner")
	}
	if state.skipBootstrapConn != conn2 {
		exec.Unlock()
		t.Fatalf("expected stale reader cleanup to preserve replacement bootstrap ownership")
	}
	exec.Unlock()

	if err := serverConn2.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created","event_id":"evt_replacement_bootstrap"}`)); err != nil {
		t.Fatalf("expected replacement bootstrap frame to be delivered, got %v", err)
	}
	if err := serverConn2.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_after_replacement","status":"completed"}}`)); err != nil {
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
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connections.Add(1)
		go func() {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
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

	waitForAtomicCount(t, &connections, 1, 2*time.Second, "expected websocket dial to be reused")
	if got := connections.Load(); got != 1 {
		t.Fatalf("expected websocket dial to be reused, got %d upstream connections", got)
	}

	cleanupCodexManagedSession(t, providerB, "gpt-5")
}

func TestCodexManagedRealtimeStaleAbortDoesNotCloseReattachedExecutionSession(t *testing.T) {
	var connections atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connections.Add(1)
		go func() {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
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

	waitForAtomicCount(t, &connections, 1, 2*time.Second, "expected websocket dial to be reused after reattach")

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
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connections.Add(1)
		go func() {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
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

	waitForAtomicCount(t, &connections, 1, 2*time.Second, "expected websocket dial to be reused after reattach")

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
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{
		"X-Session-Id": "managed-detached-abort-session",
	})
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
	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{
		"X-Session-Id": "managed-reattach-observer-reset-session",
	})
	providerA.Context.Set("token_id", 134)

	sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected initial managed realtime session to open, got %v", errWithCode)
	}
	sessionA.SetTurnObserverFactory(func() runtimesession.TurnObserver { return &recordingTurnObserver{} })
	sessionA.Detach("test_detach")

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{
		"X-Session-Id": "managed-reattach-observer-reset-session",
	})
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
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connections.Add(1)
		go func() {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
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
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connections.Add(1)
		go func() {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
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
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				connections.Add(1)
				go func() {
					defer conn.Close()
					for {
						if _, _, err := conn.ReadMessage(); err != nil {
							return
						}
					}
				}()
			}))
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

func TestCodexManagedRealtimeRejectsExecutionSessionReuseAcrossDifferentLegacyUserAgentsWithinSameChannel(t *testing.T) {
	var connections atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connections.Add(1)
		go func() {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
	defer server.Close()

	optionsA := `{"user_agent":"legacy-codex-ua-a"}`
	optionsB := `{"user_agent":"legacy-codex-ua-b"}`

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, optionsA, map[string]string{
		"X-Session-Id": "managed-cross-legacy-ua-session",
	})
	providerA.Context.Set("token_id", 214)
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

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, optionsB, map[string]string{
		"X-Session-Id": "managed-cross-legacy-ua-session",
	})
	providerB.Context.Set("token_id", 214)
	providerB.Channel.BaseURL = stringPtr(server.URL)

	sessionB, errWithCode := providerB.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected reconnect with different legacy user agent policy to open a fresh execution session, got %v", errWithCode)
	}
	managedB, ok := sessionB.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionB)
	}
	defer managedB.Abort("test_cleanup_b")

	if managedB.exec.Key == managedA.exec.Key {
		t.Fatal("expected different legacy user agent policy to force a fresh execution session key")
	}
	if binding, ok := resolveTestRealtimeBinding(providerB.Context); !ok || binding == nil || binding.SessionKey != managedB.exec.Key {
		t.Fatalf("expected binding to move onto the fresh execution session, got %+v", binding)
	}
	waitForAtomicCount(t, &connections, 2, 2*time.Second, "expected fresh reconnect to open a second upstream websocket")
}

func TestCodexManagedRealtimeRejectsExecutionSessionReuseAcrossDifferentBaseURLs(t *testing.T) {
	var connectionsA atomic.Int32
	var connectionsB atomic.Int32
	upgrader := websocket.Upgrader{}

	newServer := func(counter *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			counter.Add(1)
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
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connections.Add(1)
		go func() {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
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
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connections.Add(1)
		go func() {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
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

	_, _, _, err := sessionA.Recv(ctx)
	if !errors.Is(err, runtimesession.ErrSessionClosed) {
		t.Fatalf("expected reclaimed attachment to close the stale session, got %v", err)
	}
}

func TestCodexManagedRealtimeReusesExecutionSessionAcrossNormalizedModelAliases(t *testing.T) {
	var connections atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connections.Add(1)
		go func() {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
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
		t.Fatalf("expected managed realtime session to reopen across normalized model aliases, got %v", errWithCode)
	}
	defer sessionB.Detach("test_detach")

	waitForAtomicCount(t, &connections, 1, 2*time.Second, "expected websocket dial to be reused across normalized model aliases")
	if got := connections.Load(); got != 1 {
		t.Fatalf("expected websocket dial to be reused across normalized model aliases, got %d upstream connections", got)
	}

	cleanupCodexManagedSession(t, providerB, "gpt-5")
}

func TestCodexManagedRealtimeWebsocketNormalizesCodexRequestBeforeDispatch(t *testing.T) {
	upgrader := websocket.Upgrader{}
	eventCh := make(chan map[string]any, 1)
	errCh := make(chan error, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
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

		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			select {
			case errCh <- err:
			default:
			}
			return
		}

		select {
		case eventCh <- event:
		default:
		}

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force","prompt_cache_key_strategy":"auto"}`, map[string]string{
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

	createEvent := []byte(`{"type":"response.create","event_id":"evt_normalize","response":{"model":"gpt-5-mini","input":"hello","include":"output_text.annotations","temperature":0.2,"top_p":0.9,"truncation":"auto","context_management":{"mode":"manual"},"tools":[{"type":"web_search_preview"}],"tool_choice":{"type":"web_search_preview_2025_03_11"}}}`)
	if err := session.SendClient(context.Background(), websocket.TextMessage, createEvent); err != nil {
		t.Fatalf("expected websocket dispatch to succeed, got %v", err)
	}

	var event map[string]any
	select {
	case err := <-errCh:
		t.Fatalf("expected upstream websocket to capture request, got %v", err)
	case event = <-eventCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for upstream websocket request")
	}

	response, ok := event["response"].(map[string]any)
	if !ok {
		t.Fatalf("expected response payload map, got %T", event["response"])
	}
	if got := response["model"]; got != "gpt-5" {
		t.Fatalf("expected websocket request model to normalize to gpt-5, got %#v", got)
	}
	if got, ok := response["store"].(bool); !ok || got {
		t.Fatalf("expected websocket request store=false, got %#v", response["store"])
	}
	if _, ok := response["top_p"]; ok {
		t.Fatalf("expected websocket request to drop top_p when temperature is set, got %#v", response["top_p"])
	}
	if _, ok := response["truncation"]; ok {
		t.Fatalf("expected websocket request to strip truncation, got %#v", response["truncation"])
	}
	if _, ok := response["context_management"]; ok {
		t.Fatalf("expected websocket request to strip context_management, got %#v", response["context_management"])
	}
	if got, ok := response["prompt_cache_key"].(string); !ok || strings.TrimSpace(got) == "" {
		t.Fatalf("expected websocket request to include generated prompt_cache_key, got %#v", response["prompt_cache_key"])
	}

	includes, ok := response["include"].([]any)
	if !ok || len(includes) != 2 {
		t.Fatalf("expected websocket request includes to normalize, got %#v", response["include"])
	}
	if includes[0] != "output_text.annotations" || includes[1] != codexReasoningEncryptedContentInclude {
		t.Fatalf("unexpected websocket request includes %#v", includes)
	}

	tools, ok := response["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("expected websocket request tools to survive normalization, got %#v", response["tools"])
	}
	firstTool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("expected websocket request tool map, got %T", tools[0])
	}
	if got := firstTool["type"]; got != "web_search" {
		t.Fatalf("expected websocket request tool alias to normalize, got %#v", got)
	}

	toolChoice, ok := response["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("expected websocket request tool_choice map, got %T", response["tool_choice"])
	}
	if got := toolChoice["type"]; got != "web_search" {
		t.Fatalf("expected websocket request tool_choice alias to normalize, got %#v", got)
	}
}

func TestCodexManagedRealtimeFallsBackToHTTPBridgeInAutoMode(t *testing.T) {
	requester.InitHttpClient()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "websocket disabled", http.StatusBadRequest)
			return
		}

		if got := r.Header.Get("X-Session-Id"); got != "managed-fallback-session" {
			t.Fatalf("expected session header to be forwarded, got %q", got)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.created\",\"response\":{\"id\":\"resp_bridge\",\"status\":\"in_progress\"}}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bridge\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"))
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"X-Session-Id": "managed-fallback-session",
	})
	provider.Context.Set("token_id", 102)
	provider.Channel.BaseURL = stringPtr(server.URL)

	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime session to fall back to bridge, got %v", errWithCode)
	}
	defer session.Detach("test_close")
	defer cleanupCodexManagedSession(t, provider, "gpt-5")

	createEvent := []byte(`{"type":"response.create","event_id":"evt_bridge","response":{"model":"gpt-5","input":"hello"}}`)
	if err := session.SendClient(context.Background(), websocket.TextMessage, createEvent); err != nil {
		t.Fatalf("expected bridge dispatch to succeed, got %v", err)
	}

	var usage *types.UsageEvent
	var completed bool
	for i := 0; i < 2; i++ {
		_, payload, eventUsage, err := session.Recv(context.Background())
		if err != nil {
			t.Fatalf("expected bridge event, got %v", err)
		}
		if string(payload) == `{"type":"response.completed","response":{"id":"resp_bridge","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}` {
			usage = eventUsage
			completed = true
		}
	}

	if !completed {
		t.Fatalf("expected completed bridge event to be forwarded")
	}
	if usage == nil || usage.TotalTokens != 8 {
		t.Fatalf("expected usage from completed bridge event, got %+v", usage)
	}
}

func TestCodexManagedRealtimeForceModeFailsWhenWebsocketHandshakeFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "websocket disabled", http.StatusBadRequest)
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, map[string]string{
		"X-Session-Id": "managed-force-session",
	})
	provider.Context.Set("token_id", 103)
	provider.Channel.BaseURL = stringPtr(server.URL)

	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode == nil {
		session.Detach("test_close")
		t.Fatalf("expected force mode websocket handshake failure to surface")
	}

	cleanupCodexManagedSession(t, provider, "gpt-5")
}

func TestCodexManagedRealtimeFailedInitialOpenDoesNotLeaveStaleBinding(t *testing.T) {
	failedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "websocket disabled", http.StatusBadRequest)
	}))
	defer failedServer.Close()

	var successfulConnections atomic.Int32
	upgrader := websocket.Upgrader{}
	successServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		successfulConnections.Add(1)
		go func() {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
	defer successServer.Close()

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, map[string]string{
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

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, map[string]string{
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
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{
		"X-Session-Id": "managed-stale-abort-session",
	})
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
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{
		"X-Session-Id": "force-fresh-rebind-session",
	})
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

	sessionB, errWithCode := provider.OpenRealtimeSessionWithOptions("gpt-5", runtimesession.RealtimeOpenOptions{
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
	if removed := codexExecutionSessions.DeleteIf(oldKey, managedA.exec); removed != nil {
		t.Fatalf("expected stale execution session key %q to already be removed from the manager", oldKey)
	}
	if binding, ok := codexExecutionSessions.Resolve(managedA.exec.BindingKey); !ok || binding == nil || binding.SessionKey != managedB.exec.Key {
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
	originalManager := codexExecutionSessions
	testManager := runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{
		DefaultTTL:           time.Minute,
		MaxSessions:          8,
		MaxSessionsPerCaller: 1,
	})
	codexExecutionSessions = testManager
	t.Cleanup(func() {
		codexExecutionSessions = originalManager
		testManager.Close()
	})

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{
		"X-Session-Id": "force-fresh-capacity-session",
	})
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

	sessionB, errWithCode := provider.OpenRealtimeSessionWithOptions("gpt-5", runtimesession.RealtimeOpenOptions{
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
	defer managedB.Detach("test_close")

	if removed := codexExecutionSessions.DeleteIf(managedA.exec.Key, managedA.exec); removed != nil {
		t.Fatalf("expected force-fresh reopen to free the stale execution session capacity slot, removed=%+v", removed)
	}

	otherSessionProvider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{
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

func TestCodexManagedRealtimeAutoModePropagatesRealtimeHeaderErrors(t *testing.T) {
	var connections atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connections.Add(1)
		go func() {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"account_id":"acct-123"}`, `{"websocket_mode":"auto"}`, map[string]string{
		"X-Session-Id": "managed-auto-header-error-session",
	})
	provider.Context.Set("token_id", 117)
	provider.Channel.BaseURL = stringPtr(server.URL)
	defer cleanupCodexManagedSession(t, provider, "gpt-5")

	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode == nil {
		session.Detach("test_close")
		t.Fatalf("expected auto mode to surface realtime header errors instead of silently falling back to bridge")
	}
	if got := codexRealtimeErrorCodeString(errWithCode.Code, ""); got != "codex_token_error" {
		t.Fatalf("expected codex_token_error, got %q", got)
	}
	if binding, ok := resolveTestRealtimeBinding(provider.Context); ok || binding != nil {
		t.Fatalf("expected failed auto open to leave no managed binding, got %+v", binding)
	}
	if got := connections.Load(); got != 0 {
		t.Fatalf("expected realtime header preflight errors to avoid websocket dials, got %d connections", got)
	}
}

func TestCodexManagedRealtimeForceModeBypassesBridgeCooldown(t *testing.T) {
	var connections atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connections.Add(1)
		go func() {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, map[string]string{
		"X-Session-Id": "managed-force-cooldown-session",
	})
	provider.Context.Set("token_id", 203)
	provider.Channel.BaseURL = stringPtr(server.URL)

	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "token:203/managed-force-cooldown-session",
		SessionID: "managed-force-cooldown-session",
		CallerNS:  "token:203",
		ChannelID: provider.Channel.Id,
		Model:     "gpt-5",
		Protocol:  codexRealtimeProtocolName,
	})

	var wsConn *websocket.Conn
	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	exec.Transport = runtimesession.TransportModeResponsesHTTPBridge
	exec.FallbackUntil = time.Now().Add(5 * time.Minute)

	if errWithCode := provider.ensureRealtimeTransportLocked(exec, state, time.Now()); errWithCode != nil {
		exec.Unlock()
		t.Fatalf("expected force mode to bypass bridge cooldown and redial websocket, got %v", errWithCode)
	}
	if exec.Transport != runtimesession.TransportModeResponsesWS {
		exec.Unlock()
		t.Fatalf("expected force mode to switch transport back to websocket, got %q", exec.Transport)
	}
	if !exec.FallbackUntil.IsZero() {
		exec.Unlock()
		t.Fatalf("expected force mode redial to clear bridge cooldown, got %v", exec.FallbackUntil)
	}
	if state.wsConn == nil {
		exec.Unlock()
		t.Fatal("expected force mode redial to attach a websocket connection")
	}
	wsConn = clearCodexManagedWebsocketLocked(state)
	exec.Unlock()

	if wsConn != nil {
		_ = wsConn.Close()
	}
	waitForAtomicCount(t, &connections, 1, 2*time.Second, "expected force mode to redial websocket during cooldown")
}

func TestCodexManagedRealtimeRejectsConcurrentResponseCreate(t *testing.T) {
	requester.InitHttpClient()

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.created\",\"response\":{\"id\":\"resp_busy\",\"status\":\"in_progress\"}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		<-release
	}))
	defer server.Close()
	defer close(release)

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{
		"X-Session-Id": "managed-busy-session",
	})
	provider.Context.Set("token_id", 104)
	provider.Channel.BaseURL = stringPtr(server.URL)

	session, errWithCode := provider.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime session to open, got %v", errWithCode)
	}
	defer session.Detach("test_close")
	defer cleanupCodexManagedSession(t, provider, "gpt-5")

	createEvent := []byte(`{"type":"response.create","event_id":"evt_busy","response":{"model":"gpt-5","input":"hello"}}`)
	if err := session.SendClient(context.Background(), websocket.TextMessage, createEvent); err != nil {
		t.Fatalf("expected first response.create to succeed, got %v", err)
	}

	err := session.SendClient(context.Background(), websocket.TextMessage, createEvent)
	if err == nil {
		t.Fatalf("expected second inflight response.create to fail")
	}
	if got := err.Error(); got == "" || !containsAll(got, "session_busy") {
		t.Fatalf("expected session_busy error, got %q", got)
	}
}

func TestCodexManagedRealtimeReconnectRejectsWebsocketModeMismatch(t *testing.T) {
	upgrader := websocket.Upgrader{}
	connReady := make(chan *websocket.Conn, 1)
	connClosed := make(chan struct{}, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		select {
		case connReady <- conn:
		default:
			_ = conn.Close()
			return
		}

		go func() {
			defer conn.Close()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					select {
					case connClosed <- struct{}{}:
					default:
					}
					return
				}
			}
		}()
	}))
	defer server.Close()

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, map[string]string{
		"X-Session-Id": "managed-ws-off-session",
	})
	providerA.Context.Set("token_id", 115)
	providerA.Channel.BaseURL = stringPtr(server.URL)

	sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime websocket session to open, got %v", errWithCode)
	}
	managedA, ok := sessionA.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionA)
	}
	defer managedA.Abort("test_cleanup_a")
	defer cleanupCodexManagedSession(t, providerA, "gpt-5")
	sessionA.Detach("test_detach")

	select {
	case <-connReady:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for upstream websocket connection")
	}

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{
		"X-Session-Id": "managed-ws-off-session",
	})
	providerB.Context.Set("token_id", 115)
	providerB.Channel.BaseURL = stringPtr(server.URL)

	sessionB, errWithCode := providerB.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected reconnect with websocket mode mismatch to open a fresh execution session, got %v", errWithCode)
	}
	managedB, ok := sessionB.(*codexManagedRealtimeSession)
	if !ok {
		t.Fatalf("expected managed realtime session type, got %T", sessionB)
	}
	defer managedB.Abort("test_cleanup_b")
	if managedB.exec.Key == managedA.exec.Key {
		t.Fatal("expected websocket mode mismatch to force a fresh execution session key")
	}
	if binding, ok := resolveTestRealtimeBinding(providerB.Context); !ok || binding == nil || binding.SessionKey != managedB.exec.Key {
		t.Fatalf("expected binding to move onto the fresh execution session, got %+v", binding)
	}

	select {
	case <-connClosed:
		t.Fatalf("expected incompatible reconnect to preserve the original upstream websocket")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestCodexManagedRealtimeBridgeCancelEnqueuesCancelledEvent(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
		closed:   make(chan struct{}),
	}
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "token:1/424299/managed-cancel-session",
		SessionID: "managed-cancel-session",
		CallerNS:  "token:1",
		ChannelID: 424299,
		Model:     "gpt-5",
		Protocol:  codexRealtimeProtocolName,
	})

	attachment := newCodexAttachment()
	var ownerSeq uint64

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	ownerSeq = assignCodexAttachmentOwnerLocked(state, attachment)
	state.bridgeStream = stream
	exec.Attached = true
	exec.Inflight = true
	exec.Transport = runtimesession.TransportModeResponsesHTTPBridge
	exec.State = runtimesession.SessionStateActive
	exec.LastResponseID = "resp_cancelled"
	exec.Unlock()

	session := &codexManagedRealtimeSession{
		provider:   provider,
		exec:       exec,
		attachment: attachment,
		ownerSeq:   ownerSeq,
	}

	if err := session.SendClient(context.Background(), websocket.TextMessage, []byte(`{"type":"response.cancel","event_id":"evt_cancel"}`)); err != nil {
		t.Fatalf("expected bridge cancel to succeed, got %v", err)
	}

	select {
	case <-stream.closed:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for bridge stream close")
	}

	_, payload, usage, err := session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected cancelled event after bridge cancel, got %v", err)
	}
	if usage != nil {
		t.Fatalf("expected cancelled event without usage, got %+v", usage)
	}
	if got := string(payload); !containsAll(got, "response.cancelled", `"status":"cancelled"`, "resp_cancelled") {
		t.Fatalf("expected cancelled realtime payload, got %q", got)
	}

	exec.Lock()
	defer exec.Unlock()
	state = getCodexManagedRuntimeStateLocked(exec)
	if state.bridgeStream != nil {
		t.Fatalf("expected bridge stream to be cleared after cancel")
	}
	if exec.Inflight {
		t.Fatalf("expected inflight flag to be cleared after cancel")
	}
	if exec.State != runtimesession.SessionStateIdle {
		t.Fatalf("expected session state to return idle after cancel, got %q", exec.State)
	}
}

func TestCodexManagedRealtimeClosesWebsocketAfterProviderErrorFrame(t *testing.T) {
	var connections atomic.Int32
	firstClosed := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		connectionID := connections.Add(1)

		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}

		if connectionID == 1 {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","code":"upstream_failed","message":"upstream failed"}`)); err != nil {
				return
			}
		} else {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_recovered","status":"completed"}}`)); err != nil {
				return
			}
		}

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				if connectionID == 1 {
					select {
					case firstClosed <- struct{}{}:
					default:
					}
				}
				return
			}
		}
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, map[string]string{
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

	createEvent := []byte(`{"type":"response.create","event_id":"evt_error","response":{"model":"gpt-5","input":"hello"}}`)
	if err := session.SendClient(context.Background(), websocket.TextMessage, createEvent); err != nil {
		t.Fatalf("expected first websocket dispatch to succeed, got %v", err)
	}

	_, payload, _, err := session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected provider error event after first dispatch, got %v", err)
	}
	if got := string(payload); !containsAll(got, "upstream_failed") {
		t.Fatalf("expected provider error event payload, got %q", got)
	}

	select {
	case <-firstClosed:
	case <-time.After(2 * time.Second):
		t.Fatalf("expected provider error frame to close the abandoned upstream websocket")
	}

	if err := session.SendClient(context.Background(), websocket.TextMessage, createEvent); err != nil {
		t.Fatalf("expected second websocket dispatch to redial cleanly, got %v", err)
	}

	_, payload, _, err = session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected response from redialed websocket, got %v", err)
	}
	if got := string(payload); !containsAll(got, "response.completed", "resp_recovered") {
		t.Fatalf("expected completed event from replacement websocket, got %q", got)
	}
	waitForAtomicCount(t, &connections, 2, 2*time.Second, "expected replacement websocket dial after provider error")
	if got := connections.Load(); got != 2 {
		t.Fatalf("expected replacement websocket dial after provider error, got %d connections", got)
	}
}

func TestCodexManagedRealtimeReconnectReceivesInflightWebsocketResponse(t *testing.T) {
	var connections atomic.Int32
	createSeen := make(chan struct{}, 1)
	allowComplete := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{}

	defer func() {
		select {
		case allowComplete <- struct{}{}:
		default:
		}
	}()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
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
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_after_reattach","status":"completed"}}`))

			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
	}))
	defer server.Close()

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, map[string]string{
		"X-Session-Id": "managed-ws-reattach-session",
	})
	providerA.Context.Set("token_id", 110)
	providerA.Channel.BaseURL = stringPtr(server.URL)

	sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected managed realtime websocket session to open, got %v", errWithCode)
	}

	createEvent := []byte(`{"type":"response.create","event_id":"evt_inflight","response":{"model":"gpt-5","input":"hello"}}`)
	if err := sessionA.SendClient(context.Background(), websocket.TextMessage, createEvent); err != nil {
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
	_, _, _, err := sessionA.Recv(ctxClosed)
	if !errors.Is(err, runtimesession.ErrSessionClosed) {
		t.Fatalf("expected detached websocket session to close locally, got %v", err)
	}

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, map[string]string{
		"X-Session-Id": "managed-ws-reattach-session",
	})
	providerB.Context.Set("token_id", 110)
	providerB.Channel.BaseURL = stringPtr(server.URL)

	sessionB, errWithCode := providerB.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected reconnect during inflight websocket turn to succeed, got %v", errWithCode)
	}
	defer sessionB.Detach("test_close")
	defer cleanupCodexManagedSession(t, providerB, "gpt-5")

	allowComplete <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, payload, _, err := sessionB.Recv(ctx)
	if err != nil {
		t.Fatalf("expected reattached session to receive inflight websocket response, got %v", err)
	}
	if got := string(payload); !containsAll(got, "response.completed", "resp_after_reattach") {
		t.Fatalf("expected completed event after websocket reattach, got %q", got)
	}
	waitForAtomicCount(t, &connections, 1, 2*time.Second, "expected inflight websocket reconnect to reuse upstream connection")
	if got := connections.Load(); got != 1 {
		t.Fatalf("expected inflight websocket reconnect to reuse upstream connection, got %d connections", got)
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
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.completed"}`),
	}); !ok {
		t.Fatal("expected outbound frame to enqueue before attachment close")
	}
	attachment.close()

	session := &codexManagedRealtimeSession{attachment: attachment}
	messageType, payload, usage, err := session.Recv(context.Background())
	if err != nil {
		t.Fatalf("expected buffered outbound to survive attachment close, got %v", err)
	}
	if messageType != websocket.TextMessage {
		t.Fatalf("expected buffered websocket text payload, got %d", messageType)
	}
	if got := string(payload); got != `{"type":"response.completed"}` {
		t.Fatalf("expected buffered payload to survive attachment close, got %q", got)
	}
	if usage != nil {
		t.Fatalf("expected buffered payload not to invent usage, got %+v", usage)
	}

	_, payload, usage, err = session.Recv(context.Background())
	if !errors.Is(err, runtimesession.ErrSessionClosed) {
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
			messageType: websocket.TextMessage,
			payload:     []byte(`{"type":"response.completed"}`),
		}); ok {
			t.Fatalf("expected closed attachment to reject outbound enqueue on attempt %d", attempt+1)
		}
	}

	_, err := recvCodexAttachmentOutboundWithTimeout(attachment, 50*time.Millisecond)
	if !errors.Is(err, runtimesession.ErrSessionClosed) {
		t.Fatalf("expected closed attachment to stay drained, got %v", err)
	}
}

func TestEnqueueCodexOutboundClosesAttachmentWhenBackpressured(t *testing.T) {
	originalTimeout := codexRealtimeOutboundBackpressureTimeout
	codexRealtimeOutboundBackpressureTimeout = 20 * time.Millisecond
	defer func() {
		codexRealtimeOutboundBackpressureTimeout = originalTimeout
	}()

	attachment := newCodexAttachmentWithCapacity(1)
	if ok := enqueueCodexOutbound(attachment, codexRealtimeOutbound{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.created"}`),
	}); !ok {
		t.Fatal("expected first outbound enqueue to succeed")
	}

	startedAt := time.Now()
	if ok := enqueueCodexOutbound(attachment, codexRealtimeOutbound{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.completed"}`),
	}); ok {
		t.Fatal("expected backpressured outbound enqueue to fail")
	}
	if elapsed := time.Since(startedAt); elapsed < 20*time.Millisecond {
		t.Fatalf("expected enqueue to wait for bounded backpressure window, elapsed=%s", elapsed)
	}

	waitForCodexAttachmentClosed(t, attachment, time.Second)

	if ok := enqueueCodexOutbound(attachment, codexRealtimeOutbound{
		messageType: websocket.TextMessage,
		payload:     []byte(`{"type":"response.failed"}`),
	}); ok {
		t.Fatal("expected closed attachment to reject subsequent outbound frames")
	}
}

func TestCloneCodexResponsesRequestDoesNotAliasMutableFields(t *testing.T) {
	request := &types.OpenAIResponsesRequest{
		Model:    "gpt-5",
		Include:  []string{"output_text.annotations"},
		Metadata: map[string]string{"request_id": "req_1"},
		Tools: []types.ResponsesTools{
			{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: "medium"},
		},
		ToolChoice: map[string]any{
			"type": "web_search_preview_2025_03_11",
			"tools": []any{
				map[string]any{"type": types.APIToolTypeWebSearchPreview},
			},
		},
	}

	cloned, err := cloneCodexResponsesRequest(request)
	if err != nil {
		t.Fatalf("expected request clone to succeed, got %v", err)
	}

	cloned.Tools[0].Type = "web_search"
	cloned.Metadata["request_id"] = "req_2"
	clonedInclude := cloned.Include.([]string)
	clonedInclude[0] = codexReasoningEncryptedContentInclude
	cloned.Include = clonedInclude
	clonedToolChoice := cloned.ToolChoice.(map[string]any)
	clonedToolChoice["type"] = "web_search"
	clonedToolChoice["tools"].([]any)[0].(map[string]any)["type"] = "web_search"

	if request.Tools[0].Type != types.APIToolTypeWebSearchPreview {
		t.Fatalf("expected cloned tools mutation not to affect original request, got %q", request.Tools[0].Type)
	}
	if request.Metadata["request_id"] != "req_1" {
		t.Fatalf("expected cloned metadata mutation not to affect original request, got %#v", request.Metadata)
	}
	if request.Include.([]string)[0] != "output_text.annotations" {
		t.Fatalf("expected cloned include mutation not to affect original request, got %#v", request.Include)
	}
	originalToolChoice := request.ToolChoice.(map[string]any)
	if originalToolChoice["type"] != "web_search_preview_2025_03_11" {
		t.Fatalf("expected cloned tool_choice mutation not to affect original request, got %#v", originalToolChoice["type"])
	}
	if nestedType := originalToolChoice["tools"].([]any)[0].(map[string]any)["type"]; nestedType != types.APIToolTypeWebSearchPreview {
		t.Fatalf("expected cloned nested tool_choice mutation not to affect original request, got %#v", nestedType)
	}
}

func TestCodexManagedRealtimePreservesInflightBridgeTransportOnReattach(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, map[string]string{
		"X-Session-Id": "managed-bridge-transport-session",
	})
	provider.Context.Set("token_id", 111)
	provider.Channel.BaseURL = stringPtr("http://127.0.0.1:1")

	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "token:111/424299/managed-bridge-transport-session",
		SessionID: "managed-bridge-transport-session",
		CallerNS:  "token:111",
		ChannelID: 424299,
		Model:     "gpt-5",
		Protocol:  codexRealtimeProtocolName,
	})
	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	state.bridgeStream = stream
	exec.Inflight = true
	exec.Transport = runtimesession.TransportModeResponsesHTTPBridge

	if errWithCode := provider.ensureRealtimeTransportLocked(exec, state, time.Now()); errWithCode != nil {
		exec.Unlock()
		t.Fatalf("expected active bridge transport to be preserved during reattach, got %v", errWithCode)
	}
	if exec.Transport != runtimesession.TransportModeResponsesHTTPBridge {
		exec.Unlock()
		t.Fatalf("expected active bridge transport to remain HTTP bridge, got %q", exec.Transport)
	}
	if state.bridgeStream != stream {
		exec.Unlock()
		t.Fatal("expected bridge stream to remain attached")
	}
	exec.Unlock()
}

func TestPumpRealtimeHTTPBridgeReattachesInflightDelivery(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "token:1/424299/managed-bridge-reattach-session",
		SessionID: "managed-bridge-reattach-session",
		CallerNS:  "token:1",
		ChannelID: 424299,
		Model:     "gpt-5",
		Protocol:  codexRealtimeProtocolName,
	})

	originalAttachment := newCodexAttachment()
	session := &codexManagedRealtimeSession{
		provider:   provider,
		exec:       exec,
		attachment: originalAttachment,
	}

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	session.ownerSeq = assignCodexAttachmentOwnerLocked(state, originalAttachment)
	state.bridgeStream = stream
	exec.Attached = true
	exec.Inflight = true
	exec.Transport = runtimesession.TransportModeResponsesHTTPBridge
	exec.State = runtimesession.SessionStateActive
	exec.Unlock()

	done := make(chan struct{})
	go func() {
		provider.pumpRealtimeHTTPBridge(exec, stream)
		close(done)
	}()

	session.Detach("test_detach")

	exec.Lock()
	state = getCodexManagedRuntimeStateLocked(exec)
	if state.bridgeStream != stream {
		exec.Unlock()
		t.Fatal("expected detach to preserve inflight bridge stream")
	}
	if !exec.Inflight {
		exec.Unlock()
		t.Fatal("expected detach to preserve inflight bridge state")
	}
	if exec.Attached {
		exec.Unlock()
		t.Fatal("expected detach to clear attachment ownership")
	}
	exec.Unlock()

	replacementAttachment := newCodexAttachment()

	exec.Lock()
	state = getCodexManagedRuntimeStateLocked(exec)
	assignCodexAttachmentOwnerLocked(state, replacementAttachment)
	exec.Attached = true
	exec.Unlock()

	stream.dataChan <- "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_reattached\",\"status\":\"completed\"}}\n\n"
	stream.errChan <- io.EOF

	outbound := recvCodexAttachmentOutbound(t, replacementAttachment)
	if got := string(outbound.payload); !containsAll(got, "response.completed", "resp_reattached") {
		t.Fatalf("expected reattached bridge payload, got %q", got)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for bridge reader to exit")
	}

	exec.Lock()
	defer exec.Unlock()
	state = getCodexManagedRuntimeStateLocked(exec)
	if state.bridgeStream != nil {
		t.Fatalf("expected bridge stream to clear after terminal payload")
	}
	if exec.Inflight {
		t.Fatalf("expected inflight bridge state to clear after completion")
	}
	if exec.State != runtimesession.SessionStateIdle {
		t.Fatalf("expected bridge session state to return idle, got %q", exec.State)
	}
	if got := exec.LastResponseID; got != "resp_reattached" {
		t.Fatalf("expected last response id to follow reattached bridge, got %q", got)
	}
}

func TestPumpRealtimeHTTPBridgeFinalizesTurnOnlyAfterTerminalEvent(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "token:1/managed-bridge-finalizer-session",
		SessionID: "managed-bridge-finalizer-session",
		CallerNS:  "token:1",
		ChannelID: 424299,
		Model:     "gpt-5",
		Protocol:  codexRealtimeProtocolName,
	})

	originalAttachment := newCodexAttachment()
	recorder := &recordingTurnObserver{}
	session := &codexManagedRealtimeSession{
		provider:   provider,
		exec:       exec,
		attachment: originalAttachment,
	}

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	session.ownerSeq = assignCodexAttachmentOwnerLocked(state, originalAttachment)
	state.bridgeStream = stream
	state.turnObserverFactory = func() runtimesession.TurnObserver { return recorder }
	beginCodexTurnLocked(state, time.Now())
	exec.Attached = true
	exec.Inflight = true
	exec.Transport = runtimesession.TransportModeResponsesHTTPBridge
	exec.State = runtimesession.SessionStateActive
	exec.Unlock()

	done := make(chan struct{})
	go func() {
		provider.pumpRealtimeHTTPBridge(exec, stream)
		close(done)
	}()

	session.Detach("test_detach")
	time.Sleep(50 * time.Millisecond)
	if got := recorder.finalizeCount(); got != 0 {
		t.Fatalf("expected detach to avoid finalizing the inflight turn, got %d finalizations", got)
	}

	replacementAttachment := newCodexAttachment()
	exec.Lock()
	state = getCodexManagedRuntimeStateLocked(exec)
	assignCodexAttachmentOwnerLocked(state, replacementAttachment)
	exec.Attached = true
	exec.Unlock()

	stream.dataChan <- "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_finalized\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"
	stream.errChan <- io.EOF

	outbound := recvCodexAttachmentOutbound(t, replacementAttachment)
	if got := string(outbound.payload); !containsAll(got, "response.completed", "resp_finalized") {
		t.Fatalf("expected reattached bridge payload, got %q", got)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for bridge reader to exit")
	}

	if got := recorder.finalizeCount(); got != 1 {
		t.Fatalf("expected exactly one turn finalization after terminal event, got %d", got)
	}
	payload := recorder.lastPayload()
	if payload.TerminationReason != "response.completed" {
		t.Fatalf("expected terminal finalization reason to follow the terminal event, got %q", payload.TerminationReason)
	}
	if payload.FirstResponseAt.IsZero() {
		t.Fatal("expected terminal finalization payload to freeze the first supplier response time")
	}
	if payload.FirstResponseAt.Before(payload.StartedAt) {
		t.Fatalf("expected first response time to be on or after the turn start, got start=%v first=%v", payload.StartedAt, payload.FirstResponseAt)
	}
	if payload.Usage == nil || payload.Usage.TotalTokens != 8 {
		t.Fatalf("expected terminal usage to be finalized once, got %+v", payload.Usage)
	}
	if got := recorder.observeCount(); got != 1 {
		t.Fatalf("expected turn usage observer to see the terminal usage once, got %d", got)
	}
}

func TestPumpRealtimeHTTPBridgeIgnoresNonTerminalUsageSnapshots(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string, 2),
		errChan:  make(chan error, 1),
	}
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "token:1/managed-bridge-snapshot-session",
		SessionID: "managed-bridge-snapshot-session",
		CallerNS:  "token:1",
		ChannelID: 424299,
		Model:     "gpt-5",
		Protocol:  codexRealtimeProtocolName,
	})

	attachment := newCodexAttachment()
	recorder := &recordingTurnObserver{}
	session := &codexManagedRealtimeSession{
		provider:   provider,
		exec:       exec,
		attachment: attachment,
	}
	defer session.Detach("test_close")

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	session.ownerSeq = assignCodexAttachmentOwnerLocked(state, attachment)
	state.bridgeStream = stream
	state.turnObserverFactory = func() runtimesession.TurnObserver { return recorder }
	beginCodexTurnLocked(state, time.Now())
	exec.Attached = true
	exec.Inflight = true
	exec.Transport = runtimesession.TransportModeResponsesHTTPBridge
	exec.State = runtimesession.SessionStateActive
	exec.Unlock()

	done := make(chan struct{})
	go func() {
		provider.pumpRealtimeHTTPBridge(exec, stream)
		close(done)
	}()

	stream.dataChan <- "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_snapshot\",\"status\":\"in_progress\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"
	stream.dataChan <- "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_snapshot\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, payload, usage, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("expected first bridge payload, got %v", err)
	}
	if got := string(payload); !containsAll(got, "response.created", "resp_snapshot") {
		t.Fatalf("expected non-terminal bridge snapshot to be forwarded, got %q", got)
	}
	if usage != nil {
		t.Fatalf("expected non-terminal bridge usage snapshot to be ignored, got %+v", usage)
	}

	_, payload, usage, err = session.Recv(ctx)
	if err != nil {
		t.Fatalf("expected terminal bridge payload, got %v", err)
	}
	if got := string(payload); !containsAll(got, "response.completed", "resp_snapshot") {
		t.Fatalf("expected terminal bridge payload, got %q", got)
	}
	if usage == nil || usage.TotalTokens != 8 {
		t.Fatalf("expected usage only on the terminal bridge payload, got %+v", usage)
	}

	stream.errChan <- io.EOF

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for bridge reader to exit")
	}

	if got := recorder.observeCount(); got != 1 {
		t.Fatalf("expected exactly one observed terminal usage update, got %d", got)
	}
	if got := recorder.finalizeCount(); got != 1 {
		t.Fatalf("expected exactly one bridge finalization, got %d", got)
	}
	finalizePayload := recorder.lastPayload()
	if finalizePayload.Usage == nil || finalizePayload.Usage.TotalTokens != 8 {
		t.Fatalf("expected finalized bridge usage to preserve the terminal total once, got %+v", finalizePayload.Usage)
	}
}

func TestPumpRealtimeHTTPBridgePropagatesTurnUsageObserverErrors(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
		closed:   make(chan struct{}),
	}
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "token:1/managed-bridge-quota-session",
		SessionID: "managed-bridge-quota-session",
		CallerNS:  "token:1",
		ChannelID: 424299,
		Model:     "gpt-5",
		Protocol:  codexRealtimeProtocolName,
	})

	attachment := newCodexAttachment()
	finalizer := &failingTurnObserver{observeErr: errors.New("user quota is not enough")}
	session := &codexManagedRealtimeSession{
		provider:   provider,
		exec:       exec,
		attachment: attachment,
	}
	defer session.Detach("test_close")

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	session.ownerSeq = assignCodexAttachmentOwnerLocked(state, attachment)
	state.bridgeStream = stream
	state.turnObserverFactory = func() runtimesession.TurnObserver { return finalizer }
	beginCodexTurnLocked(state, time.Now())
	exec.Attached = true
	exec.Inflight = true
	exec.Transport = runtimesession.TransportModeResponsesHTTPBridge
	exec.State = runtimesession.SessionStateActive
	exec.Unlock()

	done := make(chan struct{})
	go func() {
		provider.pumpRealtimeHTTPBridge(exec, stream)
		close(done)
	}()

	stream.dataChan <- "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_quota\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	messageType, payload, usage, err := session.Recv(ctx)
	if err == nil {
		t.Fatal("expected turn usage observer error to reach the managed session")
	}
	if messageType != websocket.TextMessage {
		t.Fatalf("expected websocket text payload, got %d", messageType)
	}
	if got := string(payload); !containsAll(got, "response.completed", "resp_quota") {
		t.Fatalf("expected original bridge payload before the terminal error, got %q", got)
	}
	if usage == nil || usage.TotalTokens != 8 {
		t.Fatalf("expected bridge usage to remain attached to the forwarded payload, got %+v", usage)
	}
	if got := err.Error(); !containsAll(got, "system_error", "user quota is not enough") {
		t.Fatalf("expected quota exhaustion error payload, got %q", got)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for bridge reader to exit after turn usage observer error")
	}

	if got := finalizer.finalizeCount(); got != 1 {
		t.Fatalf("expected exactly one turn finalization on observer error, got %d", got)
	}
	finalizePayload := finalizer.lastPayload()
	if finalizePayload.TerminationReason != "response.completed" {
		t.Fatalf("expected terminal bridge reason to remain response.completed, got %q", finalizePayload.TerminationReason)
	}
	if finalizePayload.FirstResponseAt.IsZero() {
		t.Fatal("expected quota exhaustion finalization to preserve the first supplier response time")
	}
	if finalizePayload.FirstResponseAt.Before(finalizePayload.StartedAt) {
		t.Fatalf("expected quota exhaustion first response time to be on or after the turn start, got start=%v first=%v", finalizePayload.StartedAt, finalizePayload.FirstResponseAt)
	}
	if finalizePayload.Usage == nil || finalizePayload.Usage.TotalTokens != 8 {
		t.Fatalf("expected finalized usage to preserve the observed usage, got %+v", finalizePayload.Usage)
	}
	if got := finalizer.observeCount(); got != 1 {
		t.Fatalf("expected observer to record the triggering usage once, got %d", got)
	}

	exec.Lock()
	defer exec.Unlock()
	state = getCodexManagedRuntimeStateLocked(exec)
	if state.bridgeStream != nil {
		t.Fatalf("expected bridge stream to clear after observer error")
	}
	if exec.Inflight {
		t.Fatalf("expected inflight bridge state to clear after observer error")
	}
	if exec.State != runtimesession.SessionStateIdle {
		t.Fatalf("expected bridge session state to return idle after observer error, got %q", exec.State)
	}
}

func TestPumpRealtimeHTTPBridgeBackfillsMissingTerminalUsage(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() {
		config.DisableTokenEncoders = originalDisable
	})

	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
		closed:   make(chan struct{}),
	}
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "token:1/managed-bridge-backfilled-session",
		SessionID: "managed-bridge-backfilled-session",
		CallerNS:  "token:1",
		ChannelID: 424399,
		Model:     "gpt-5",
		Protocol:  codexRealtimeProtocolName,
	})

	attachment := newCodexAttachment()
	recorder := &recordingTurnObserver{}
	session := &codexManagedRealtimeSession{
		provider:   provider,
		exec:       exec,
		attachment: attachment,
	}
	defer session.Detach("test_close")

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	session.ownerSeq = assignCodexAttachmentOwnerLocked(state, attachment)
	state.bridgeStream = stream
	state.turnObserverFactory = func() runtimesession.TurnObserver { return recorder }
	beginCodexTurnLocked(state, time.Now())
	state.turnAccumulator.SeedPromptFromRequest(&types.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	}, 0)
	exec.Attached = true
	exec.Inflight = true
	exec.Transport = runtimesession.TransportModeResponsesHTTPBridge
	exec.State = runtimesession.SessionStateActive
	exec.Unlock()

	done := make(chan struct{})
	go func() {
		provider.pumpRealtimeHTTPBridge(exec, stream)
		close(done)
	}()

	stream.dataChan <- "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bridge_backfilled\",\"status\":\"completed\",\"tools\":[{\"type\":\"web_search_preview\",\"search_context_size\":\"high\"}],\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello from bridge\"}]},{\"id\":\"ws_1\",\"type\":\"web_search_call\",\"status\":\"completed\"}]}}\n\n"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, payload, usage, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("expected terminal bridge payload, got %v", err)
	}
	if got := string(payload); !containsAll(got, "response.completed", "resp_bridge_backfilled") {
		t.Fatalf("expected terminal bridge payload, got %q", got)
	}
	if usage == nil || usage.InputTokens <= 0 || usage.OutputTokens <= 0 || usage.TotalTokens <= usage.InputTokens {
		t.Fatalf("expected terminal bridge payload to backfill usage, got %+v", usage)
	}
	billing, ok := usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok || billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected terminal bridge payload to preserve a single web search charge, got %+v", usage.ExtraBilling)
	}

	stream.errChan <- io.EOF

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for bridge reader to exit")
	}

	finalizePayload := recorder.lastPayload()
	if finalizePayload.Usage == nil || finalizePayload.Usage.InputTokens <= 0 || finalizePayload.Usage.OutputTokens <= 0 {
		t.Fatalf("expected finalized bridge turn to preserve backfilled usage, got %+v", finalizePayload.Usage)
	}
}

func TestCodexManagedRealtimeWebsocketCarriesUsageOnFailedTerminalEvent(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}

		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.failed","response":{"id":"resp_failed","status":"failed","usage":{"input_tokens":3,"output_tokens":0,"total_tokens":3}}}`))
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, map[string]string{
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

	createEvent := []byte(`{"type":"response.create","event_id":"evt_failed","response":{"model":"gpt-5","input":"hello"}}`)
	if err := session.SendClient(context.Background(), websocket.TextMessage, createEvent); err != nil {
		t.Fatalf("expected websocket dispatch to succeed, got %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, payload, usage, err := session.Recv(ctx)
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

func TestCodexManagedRealtimeWebsocketBackfillsMissingTerminalUsage(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() {
		config.DisableTokenEncoders = originalDisable
	})

	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}

		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_backfilled","status":"completed","tools":[{"type":"web_search_preview","search_context_size":"high"}],"output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello from websocket"}]},{"id":"ws_1","type":"web_search_call","status":"completed"}]}}`))
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, map[string]string{
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

	createEvent := []byte(`{"type":"response.create","event_id":"evt_backfilled","response":{"model":"gpt-5","input":"hello"}}`)
	if err := session.SendClient(context.Background(), websocket.TextMessage, createEvent); err != nil {
		t.Fatalf("expected websocket dispatch to succeed, got %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, payload, usage, err := session.Recv(ctx)
	if err != nil {
		t.Fatalf("expected completed terminal payload, got %v", err)
	}
	if got := string(payload); !containsAll(got, "response.completed", "resp_backfilled") {
		t.Fatalf("expected completed terminal payload, got %q", got)
	}
	if usage == nil || usage.InputTokens <= 0 || usage.OutputTokens <= 0 || usage.TotalTokens <= usage.InputTokens {
		t.Fatalf("expected completed websocket turn to backfill usage, got %+v", usage)
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
	if finalizePayload.Usage == nil || finalizePayload.Usage.InputTokens <= 0 || finalizePayload.Usage.OutputTokens <= 0 {
		t.Fatalf("expected finalized websocket turn to preserve backfilled usage, got %+v", finalizePayload.Usage)
	}
}

func TestPumpRealtimeHTTPBridgeDropsBufferedFramesAfterAttachmentReattach(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "token:1/424299/managed-bridge-session",
		SessionID: "managed-bridge-session",
		CallerNS:  "token:1",
		ChannelID: 424299,
		Model:     "gpt-5",
		Protocol:  codexRealtimeProtocolName,
	})

	originalAttachment := newCodexAttachment()

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	assignCodexAttachmentOwnerLocked(state, originalAttachment)
	state.bridgeStream = stream
	exec.Attached = true
	exec.Inflight = true
	exec.State = runtimesession.SessionStateActive
	exec.Unlock()

	done := make(chan struct{})
	go func() {
		provider.pumpRealtimeHTTPBridge(exec, stream)
		close(done)
	}()

	replacementAttachment := newCodexAttachment()

	exec.Lock()
	state = getCodexManagedRuntimeStateLocked(exec)
	assignCodexAttachmentOwnerLocked(state, replacementAttachment)
	state.bridgeStream = nil
	exec.Attached = true
	exec.Inflight = false
	exec.State = runtimesession.SessionStateIdle
	exec.Unlock()

	stream.dataChan <- "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_old\",\"status\":\"completed\"}}\n\n"
	stream.errChan <- io.EOF

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for abandoned bridge reader to exit")
	}

	if outbound, err := recvCodexAttachmentOutboundWithTimeout(replacementAttachment, 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected abandoned bridge frames to be dropped, got %q err=%v", string(outbound.payload), err)
	}

	exec.Lock()
	defer exec.Unlock()
	if got := exec.LastResponseID; got != "" {
		t.Fatalf("expected abandoned bridge frames to avoid mutating session state, got last response id %q", got)
	}
}

func TestPumpRealtimeHTTPBridgeReportsEOFBeforeTerminalEvent(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	exec := runtimesession.NewExecutionSession(runtimesession.Metadata{
		Key:       "token:1/managed-bridge-eof-session",
		SessionID: "managed-bridge-eof-session",
		CallerNS:  "token:1",
		ChannelID: 424299,
		Model:     "gpt-5",
		Protocol:  codexRealtimeProtocolName,
	})

	attachment := newCodexAttachment()

	exec.Lock()
	state := getCodexManagedRuntimeStateLocked(exec)
	assignCodexAttachmentOwnerLocked(state, attachment)
	state.bridgeStream = stream
	exec.Attached = true
	exec.Inflight = true
	exec.Transport = runtimesession.TransportModeResponsesHTTPBridge
	exec.State = runtimesession.SessionStateActive
	exec.Unlock()

	done := make(chan struct{})
	go func() {
		provider.pumpRealtimeHTTPBridge(exec, stream)
		close(done)
	}()

	stream.errChan <- io.EOF

	outbound := recvCodexAttachmentOutbound(t, attachment)
	if got := string(outbound.payload); !containsAll(got, "bridge_stream_failed", "closed before a terminal response event") {
		t.Fatalf("expected EOF before terminal event to surface a bridge failure, got %q", got)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for bridge reader to exit after EOF")
	}

	exec.Lock()
	defer exec.Unlock()
	state = getCodexManagedRuntimeStateLocked(exec)
	if state.bridgeStream != nil {
		t.Fatalf("expected bridge stream to clear after EOF")
	}
	if exec.Inflight {
		t.Fatalf("expected inflight bridge state to clear after EOF")
	}
	if exec.State != runtimesession.SessionStateIdle {
		t.Fatalf("expected bridge session state to return idle after EOF, got %q", exec.State)
	}
}

func TestCodexManagedRealtimeWebsocketDuplicateTerminalEventsDoNotDoubleBill(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}

		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_duplicate","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","response":{"id":"resp_duplicate","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`))
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, map[string]string{
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

	createEvent := []byte(`{"type":"response.create","event_id":"evt_duplicate","response":{"model":"gpt-5","input":"hello"}}`)
	if err := session.SendClient(context.Background(), websocket.TextMessage, createEvent); err != nil {
		t.Fatalf("expected websocket dispatch to succeed, got %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for _, want := range []string{"response.completed", "response.done"} {
		_, payload, _, err := session.Recv(ctx)
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
	originalManager := codexExecutionSessions
	testManager := runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{
		DefaultTTL: time.Millisecond,
	})
	codexExecutionSessions = testManager
	t.Cleanup(func() {
		codexExecutionSessions = originalManager
		testManager.Close()
	})

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, map[string]string{
		"X-Session-Id": "managed-model-mismatch-session",
	})
	provider.Context.Set("token_id", 118)

	meta, errWithCode := provider.buildExecutionSessionMetadata("gpt-5", runtimesession.RealtimeOpenOptions{})
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
	exec.IdleTTL = time.Millisecond
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
	originalManager := codexExecutionSessions
	testManager := runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{
		DefaultTTL: time.Minute,
	})
	codexExecutionSessions = testManager
	t.Cleanup(func() {
		codexExecutionSessions = originalManager
		testManager.Close()
	})

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, nil)
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
	originalManager := codexExecutionSessions
	testManager := runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{
		DefaultTTL: time.Minute,
	})
	codexExecutionSessions = testManager
	t.Cleanup(func() {
		codexExecutionSessions = originalManager
		testManager.Close()
	})

	allowTerminalEvent := make(chan struct{})
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		<-allowTerminalEvent
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_detached_terminal","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`))
	}))
	defer server.Close()

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"force"}`, nil)
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

	createEvent := []byte(`{"type":"response.create","event_id":"evt_detached_terminal","response":{"model":"gpt-5","input":"hello"}}`)
	if err := session.SendClient(context.Background(), websocket.TextMessage, createEvent); err != nil {
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
	originalManager := codexExecutionSessions
	testManager := runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{
		DefaultTTL:           time.Minute,
		MaxSessions:          8,
		MaxSessionsPerCaller: 1,
	})
	codexExecutionSessions = testManager
	t.Cleanup(func() {
		codexExecutionSessions = originalManager
		testManager.Close()
	})

	providerA := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{
		"X-Session-Id": "caller-cap-session-a",
	})
	providerA.Context.Set("token_id", 120)
	providerA.Context.Set("id", 8001)

	sessionA, errWithCode := providerA.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected initial caller session to open, got %v", errWithCode)
	}
	defer sessionA.Abort("test_cleanup")

	providerB := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{
		"X-Session-Id": "caller-cap-session-b",
	})
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

	providerC := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{
		"X-Session-Id": "caller-cap-session-c",
	})
	providerC.Context.Set("token_id", 122)
	providerC.Context.Set("id", 8002)

	sessionC, errWithCode := providerC.OpenRealtimeSession("gpt-5")
	if errWithCode != nil {
		t.Fatalf("expected a different user capacity namespace to retain independent capacity, got %v", errWithCode)
	}
	sessionC.Abort("test_cleanup")
}

type recordingTurnObserver struct {
	mu        sync.Mutex
	observed  []*types.UsageEvent
	finalized []runtimesession.TurnFinalizePayload
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
	originalManager := codexExecutionSessions
	testManager := runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{
		DefaultTTL: time.Minute,
	})
	codexExecutionSessions = testManager
	t.Cleanup(func() {
		codexExecutionSessions = originalManager
		testManager.Close()
	})

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{
		"X-Session-Id": "local-only-promote-session",
	})
	provider.Context.Set("token_id", 118)

	meta, errWithCode := provider.buildExecutionSessionMetadata("gpt-5", runtimesession.RealtimeOpenOptions{})
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
	defer managed.Detach("test_close")

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
		codexExecutionSessions.Delete(binding.SessionKey)
		return
	}
	meta, errWithCode := provider.buildExecutionSessionMetadata(model, runtimesession.RealtimeOpenOptions{})
	if errWithCode != nil {
		return
	}
	codexExecutionSessions.Delete(meta.Key)
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
