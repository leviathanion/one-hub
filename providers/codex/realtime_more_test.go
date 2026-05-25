package codex

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common/logger"
	"one-api/common/wsconn"
	"one-api/types"

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
	if _, err := buildCodexRealtimeURL("http://example.com/backend-api/codex/responses"); err == nil {
		t.Fatal("expected plaintext codex realtime url to be rejected by default")
	}
	if wsURL, err := buildCodexRealtimeURLWithPolicy("http://127.0.0.1:8080/backend-api/codex/responses", true, false); err != nil || wsURL != "ws://127.0.0.1:8080/backend-api/codex/responses" {
		t.Fatalf("expected self-hosted http codex realtime url rewrite, url=%q err=%v", wsURL, err)
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

	if shouldContinue, usage, rewritten, err := provider.handleRealtimeSupplierMessage(wsconn.BinaryMessage, []byte("ignored"), nil, "gpt-5"); !shouldContinue || usage != nil || rewritten != nil || err != nil {
		t.Fatalf("expected non-text realtime supplier messages to be ignored, continue=%v usage=%+v rewritten=%v err=%v", shouldContinue, usage, rewritten, err)
	}
	if shouldContinue, usage, rewritten, err := provider.handleRealtimeSupplierMessage(wsconn.TextMessage, []byte("bad-json"), nil, "gpt-5"); !shouldContinue || usage != nil || rewritten != nil || err != nil {
		t.Fatalf("expected invalid realtime supplier json to be ignored, continue=%v usage=%+v rewritten=%v err=%v", shouldContinue, usage, rewritten, err)
	}
	if shouldContinue, usage, rewritten, err := provider.handleRealtimeSupplierMessage(wsconn.TextMessage, []byte(`{"type":"error"}`), nil, "gpt-5"); !shouldContinue || usage != nil || rewritten != nil || err != nil {
		t.Fatalf("expected provider error events to pass through unchanged, continue=%v usage=%+v rewritten=%v err=%v", shouldContinue, usage, rewritten, err)
	}

	nestedErrorPayload := []byte(`{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"slow down","param":"model"},"response":{"id":"resp_error_1"}}`)
	shouldContinue, usage, rewritten, err := provider.handleRealtimeSupplierMessage(wsconn.TextMessage, nestedErrorPayload, nil, "gpt-5")
	if !shouldContinue || usage != nil || rewritten != nil || err != nil {
		t.Fatalf("expected nested provider error event to pass through unchanged, continue=%v usage=%+v rewritten=%v err=%v", shouldContinue, usage, rewritten, err)
	}
	detail := codexRealtimeProviderErrorDetailFromPayload(nil, nestedErrorPayload)
	if detail.Type != "rate_limit_error" || detail.Code != "rate_limit_exceeded" || detail.Message != "slow down" {
		t.Fatalf("expected nested provider error detail to be preserved for logging, got %+v", detail)
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

	t.Run("dial honors cancellable context", func(t *testing.T) {
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer server.Close()
		defer close(release)

		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()

		done := make(chan *types.OpenAIErrorWithStatusCode, 1)
		go func() {
			conn, errWithCode := provider.dialChatRealtimeConnWithContext(ctx, &codexRealtimeConnPlan{
				wsURL: "ws" + server.URL[len("http"):],
			})
			if conn != nil {
				conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
			}
			done <- errWithCode
		}()

		select {
		case errWithCode := <-done:
			if errWithCode == nil || errWithCode.Code != "ws_request_failed" {
				t.Fatalf("expected cancelled websocket dial to map to ws_request_failed, got %+v", errWithCode)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatal("expected cancellable websocket dial to return promptly")
		}
	})

	t.Run("handshake status mapping preserves provider semantics", func(t *testing.T) {
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
			errWithCode := mapCodexRealtimeWSDialError(&wsconn.DialError{
				URL:        "wss://provider.example/realtime",
				StatusCode: tc.statusCode,
				Err:        errors.New("handshake failed"),
			})
			gotCode, _ := errWithCode.Code.(string)
			if gotCode != tc.wantCode || errWithCode.StatusCode != tc.wantStatus {
				t.Fatalf("%s: expected %s/%d, got code=%v status=%d", tc.name, tc.wantCode, tc.wantStatus, errWithCode.Code, errWithCode.StatusCode)
			}
		}

		errWithCode := mapCodexRealtimeWSDialError(errors.New("dial tcp: no route"))
		gotCode, _ := errWithCode.Code.(string)
		if gotCode != "ws_request_failed" || errWithCode.StatusCode != http.StatusInternalServerError {
			t.Fatalf("expected transport errors without HTTP status to remain ws_request_failed, got %+v", errWithCode)
		}
	})

	t.Run("handshake diagnostic log sanitizes url and includes response context", func(t *testing.T) {
		message := codexRealtimeWSDialFailureLogMessage(&wsconn.DialError{
			URL:           "wss://provider.example/backend-api/codex/responses?api_key=secret#fragment",
			StatusCode:    http.StatusBadGateway,
			Header:        http.Header{"X-Request-Id": []string{"req_123"}, "Cf-Ray": []string{"ray_456"}, "Retry-After": []string{"3"}},
			BodySnippet:   []byte("bad gateway\nupstream said no"),
			BodyTruncated: true,
			Err:           errors.New("websocket: bad handshake"),
		})
		for _, expected := range []string{
			"status=502",
			"url=wss://provider.example/backend-api/codex/responses",
			"x_request_id=req_123",
			"cf_ray=ray_456",
			"retry_after=3",
			"body_truncated=true",
			"bad gateway\\nupstream said no",
		} {
			if !strings.Contains(message, expected) {
				t.Fatalf("expected diagnostic log to contain %q, got %q", expected, message)
			}
		}
		for _, forbidden := range []string{"api_key=secret", "#fragment"} {
			if strings.Contains(message, forbidden) {
				t.Fatalf("expected diagnostic log to redact %q, got %q", forbidden, message)
			}
		}
	})

	t.Run("createChatRealtimeConn dials websocket plan", func(t *testing.T) {
		headerCh := make(chan http.Header, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			headerCh <- r.Header.Clone()
			conn, err := wsconn.AcceptManaged(w, r, wsconn.Config{Label: "codex realtime test accept"}, wsconn.AcceptOptions{
				CheckOrigin: func(*http.Request) bool { return true },
			})
			if err != nil {
				t.Errorf("upgrade failed: %v", err)
				return
			}
			<-conn.Done()
		}))
		defer server.Close()

		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
		provider.Channel.BaseURL = stringPtr(server.URL)

		conn, errWithCode := provider.createChatRealtimeConn("gpt-5", "execution-session-456")
		if errWithCode != nil {
			t.Fatalf("expected realtime websocket connect to succeed, got %v", errWithCode)
		}
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})

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
