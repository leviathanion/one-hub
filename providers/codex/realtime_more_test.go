package codex

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/logger"
	"one-api/types"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

func TestCodexRealtimeHelperFunctionsAndCompatibilityHeaders(t *testing.T) {
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	if wsURL, err := buildCodexRealtimeURL("https://example.com/backend-api/codex/responses?model=gpt-5"); err != nil || wsURL != "wss://example.com/backend-api/codex/responses?model=gpt-5" {
		t.Fatalf("expected https codex realtime url rewrite, url=%q err=%v", wsURL, err)
	}
	if wsURL, err := buildCodexRealtimeURL("http://example.com/backend-api/codex/responses"); err != nil || wsURL != "ws://example.com/backend-api/codex/responses" {
		t.Fatalf("expected http codex realtime url rewrite, url=%q err=%v", wsURL, err)
	}
	if _, err := buildCodexRealtimeURL("://bad"); err == nil {
		t.Fatal("expected invalid realtime url parse to fail")
	}

	if got := resolveCodexExecutionSessionID(newCodexHeaderBagFromMap(map[string]string{
		"x-session-id": "header-session",
		"session_id":   "legacy-session",
	}), " explicit-session "); got != "explicit-session" {
		t.Fatalf("expected explicit execution session id to win, got %q", got)
	}
	if got := resolveCodexExecutionSessionID(newCodexHeaderBagFromMap(map[string]string{
		"x-session-id": "header-session",
		"session_id":   "legacy-session",
	}), ""); got != "header-session" {
		t.Fatalf("expected x-session-id fallback, got %q", got)
	}
	if got := resolveCodexExecutionSessionID(newCodexHeaderBagFromMap(map[string]string{
		"session_id": "legacy-session",
	}), ""); got != "legacy-session" {
		t.Fatalf("expected session_id fallback, got %q", got)
	}

	applyCodexExecutionSessionHeader(nil, "ignored")
	headers := newCodexHeaderBag()
	applyCodexExecutionSessionHeader(headers, " ")
	if headers.Has("session_id") || headers.Has("x-session-id") {
		t.Fatalf("expected blank execution session id not to mutate headers, got %+v", headers.Map())
	}

	headers = newCodexHeaderBag()
	applyCodexExecutionSessionHeader(headers, "execution-session-1")
	if headers.Get("session_id") != "execution-session-1" || headers.Get("x-session-id") != "execution-session-1" {
		t.Fatalf("expected execution session headers to be backfilled, got %+v", headers.Map())
	}
	headers = newCodexHeaderBagFromMap(map[string]string{"session_id": "existing-session"})
	applyCodexExecutionSessionHeader(headers, "new-session")
	if headers.Get("session_id") != "existing-session" || headers.Get("x-session-id") != "new-session" {
		t.Fatalf("expected existing session_id to be preserved while x-session-id is backfilled, got %+v", headers.Map())
	}

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", map[string]string{
		"Version":                               "2026-03-28",
		"Originator":                            "codex-cli",
		"X-Codex-Turn-State":                    "turn-state",
		"X-ResponsesAPI-Include-Timing-Metrics": "true",
		"X-Codex-Beta-Features":                 "beta-flag",
		"X-Ignored":                             "skip-me",
	})
	compatHeaders := provider.buildRealtimeRequestCompatibilityHeaders()
	if got := compatHeaders["version"]; got != "2026-03-28" {
		t.Fatalf("expected compatibility version header, got %q", got)
	}
	if got := compatHeaders["originator"]; got != "codex-cli" {
		t.Fatalf("expected compatibility originator header, got %q", got)
	}
	if got := compatHeaders["x-codex-turn-state"]; got != "turn-state" {
		t.Fatalf("expected compatibility turn-state header, got %q", got)
	}
	if got := compatHeaders["x-responsesapi-include-timing-metrics"]; got != "true" {
		t.Fatalf("expected compatibility timing header, got %q", got)
	}
	if got := compatHeaders["x-codex-beta-features"]; got != "beta-flag" {
		t.Fatalf("expected compatibility beta header, got %q", got)
	}
	if _, ok := compatHeaders["x-ignored"]; ok {
		t.Fatalf("expected unsupported compatibility headers to be dropped, got %+v", compatHeaders)
	}
	if got := (*CodexProvider)(nil).buildRealtimeRequestCompatibilityHeaders(); len(got) != 0 {
		t.Fatalf("expected nil provider compatibility headers to be empty, got %+v", got)
	}

	overrideHeaders := newCodexHeaderBag()
	provider.applyRealtimeRequestHeaderOverrides(overrideHeaders)
	if overrideHeaders.Get("x-codex-beta-features") != "beta-flag" || overrideHeaders.Get("x-codex-turn-state") != "turn-state" || overrideHeaders.Get("x-responsesapi-include-timing-metrics") != "true" {
		t.Fatalf("expected realtime override headers to be applied, got %+v", overrideHeaders.Map())
	}
	if overrideHeaders.Get("version") != "" {
		t.Fatalf("expected compatibility-only headers not to be forced as realtime overrides, got %+v", overrideHeaders.Map())
	}
	provider.applyRealtimeRequestHeaderOverrides(nil)
	if got := (*CodexProvider)(nil).getPassthroughRealtimeHeader("version"); got != "" {
		t.Fatalf("expected nil provider passthrough header lookup to be empty, got %q", got)
	}

	if !isCodexRealtimeTerminalStatus(types.ResponseStatusCompleted) || isCodexRealtimeTerminalStatus("in_progress") {
		t.Fatal("expected terminal status helper to distinguish completed from in-progress states")
	}
	if isCodexRealtimeTerminalEvent(nil) {
		t.Fatal("expected nil realtime event not to be terminal")
	}
	if !isCodexRealtimeTerminalEvent(&types.OpenAIResponsesStreamResponses{
		Type: "response.updated",
		Response: &types.OpenAIResponsesResponses{
			Status: types.ResponseStatusCancelled,
		},
	}) {
		t.Fatal("expected cancelled response status to be treated as terminal")
	}

	if usage := codexRealtimeUsageEvent(nil, nil, "gpt-5"); usage != nil {
		t.Fatalf("expected nil realtime response usage input to stay nil, got %+v", usage)
	}
	if usage := codexRealtimeUsageEvent(&types.OpenAIResponsesResponses{Status: types.ResponseStatusCancelled}, nil, "gpt-5"); usage != nil {
		t.Fatalf("expected cancelled realtime responses without usage not to emit usage events, got %+v", usage)
	}

	if shouldContinue, usage, rewritten, err := provider.handleRealtimeSupplierMessage(websocket.BinaryMessage, []byte("ignored"), nil, "gpt-5"); !shouldContinue || usage != nil || rewritten != nil || err != nil {
		t.Fatalf("expected non-text realtime supplier messages to be ignored, continue=%v usage=%+v rewritten=%v err=%v", shouldContinue, usage, rewritten, err)
	}
	if shouldContinue, usage, rewritten, err := provider.handleRealtimeSupplierMessage(websocket.TextMessage, []byte("bad-json"), nil, "gpt-5"); !shouldContinue || usage != nil || rewritten != nil || err != nil {
		t.Fatalf("expected invalid realtime supplier json to be ignored, continue=%v usage=%+v rewritten=%v err=%v", shouldContinue, usage, rewritten, err)
	}
	if shouldContinue, usage, rewritten, err := provider.handleRealtimeSupplierMessage(websocket.TextMessage, []byte(`{"type":"error"}`), nil, "gpt-5"); shouldContinue || usage != nil || rewritten != nil || err == nil {
		t.Fatalf("expected provider error events to stop the bridge, continue=%v usage=%+v rewritten=%v err=%v", shouldContinue, usage, rewritten, err)
	}
}

func TestCodexRealtimeConnectionPlanningAndDialPaths(t *testing.T) {
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	t.Run("unsupported realtime api bubbles through create", func(t *testing.T) {
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
		provider.Config.ChatRealtime = ""

		conn, errWithCode := provider.createChatRealtimeConn("gpt-5", "session-123")
		if conn != nil {
			t.Fatalf("expected unsupported realtime api to return no connection, got %#v", conn)
		}
		if errWithCode == nil || errWithCode.Code != "unsupported_api" {
			t.Fatalf("expected unsupported_api error, got %+v", errWithCode)
		}
	})

	t.Run("token failures wrap during plan preparation", func(t *testing.T) {
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
		provider.Credentials = nil

		plan, errWithCode := provider.prepareChatRealtimeConn("gpt-5", "session-123")
		if plan != nil {
			t.Fatalf("expected missing credentials to fail plan preparation, got %#v", plan)
		}
		if errWithCode == nil || errWithCode.Code != "codex_token_error" || errWithCode.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected codex token error, got %+v", errWithCode)
		}
	})

	t.Run("nil dial plans fail fast", func(t *testing.T) {
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
		conn, errWithCode := provider.dialChatRealtimeConn(nil)
		if conn != nil {
			t.Fatalf("expected nil realtime dial plan to fail, got %#v", conn)
		}
		if errWithCode == nil || errWithCode.Code != "ws_request_failed" {
			t.Fatalf("expected ws_request_failed for nil plan, got %+v", errWithCode)
		}
	})

	t.Run("createChatRealtimeConn dials websocket plan", func(t *testing.T) {
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

		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
		provider.Channel.BaseURL = stringPtr(server.URL)

		conn, errWithCode := provider.createChatRealtimeConn("gpt-5", "execution-session-456")
		if errWithCode != nil {
			t.Fatalf("expected realtime websocket connect to succeed, got %v", errWithCode)
		}
		if err := conn.Close(); err != nil {
			t.Fatalf("expected realtime websocket close to succeed, got %v", err)
		}

		headers := <-headerCh
		if got := headers.Get("Authorization"); got != "Bearer access-token" {
			t.Fatalf("expected websocket authorization header, got %q", got)
		}
		if got := headers.Get("OpenAI-Beta"); got != codexResponsesWebsocketBetaHeaderValue {
			t.Fatalf("expected websocket beta header, got %q", got)
		}
		if got := headers.Get("X-Session-Id"); got != "execution-session-456" {
			t.Fatalf("expected websocket x-session-id header, got %q", got)
		}
		if got := headers.Get("Session_id"); got != "execution-session-456" {
			t.Fatalf("expected websocket session_id header, got %q", got)
		}
	})
}
