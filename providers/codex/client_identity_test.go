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
	"testing"
	"time"

	"one-api/common/requestctx"
	commonresponses "one-api/common/responses"
	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/providers/codex/wire"
	"one-api/types"

	"github.com/gorilla/websocket"
)

func requireRealtimeHandshakeSignature(t *testing.T, p *CodexProvider) string {
	t.Helper()
	signature, err := p.buildRealtimeHandshakePolicySignature()
	if err != nil {
		t.Fatal(err)
	}
	return signature
}

func requireRealtimeCompatibilityHash(t *testing.T, p *CodexProvider, model, identity string) string {
	t.Helper()
	hash, err := p.buildRealtimeCompatibilityHash(model, identity)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestCodexClientIdentityOnHTTPAndWebSocketWire(t *testing.T) {
	for _, tc := range []struct {
		name, ua, originator, fallbackUA, fallbackOriginator, wantUA, wantOriginator string
	}{
		{"PI defaults", "", "", "", "", DefaultUserAgent(), "pi"},
		{"config pair", "", "", "custom/1", "custom", "custom/1", "custom"},
		{"config originator only", "", "", "", "custom", DefaultUserAgent(), "custom"},
		{"config Desktop originator", "", "", "custom/1", "Codex Desktop", "custom/1", "Codex Desktop"},
		{"config UA only", "", "", "custom/1", "", "custom/1", "pi"},
		{"Codex UA only", "codex_cli_rs/1.0", "", "custom/1", "custom", "codex_cli_rs/1.0", ""},
		{"Codex originator only", "", "Codex Desktop", "custom/1", "custom", "", "Codex Desktop"},
		{"Codex pair", "codex-tui/1.0", "arbitrary/client", "custom/1", "custom", "codex-tui/1.0", "arbitrary/client"},
		{"Codex override", "cccc/1.0 (Linux; x64) foot (codex-tui; 1.0)", "cccc", "custom/1", "custom", "cccc/1.0 (Linux; x64) foot (codex-tui; 1.0)", "cccc"},
		{"non Codex config", "Mozilla/5.0", "random", "custom/1", "custom", "custom/1", "custom"},
		{"non Codex PI defaults", "curl/8.0", "random", "", "", DefaultUserAgent(), "pi"},
	} {
		for _, entry := range []string{"responses", "compact", "chat", "responses-ws", "realtime", "usage"} {
			t.Run(tc.name+"/"+entry, func(t *testing.T) {
				seen := make(chan http.Header, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seen <- r.Header.Clone()
					if websocket.IsWebSocketUpgrade(r) {
						conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
						if err != nil {
							t.Error(err)
							return
						}
						defer conn.Close()
						return
					}
					if entry == "responses" {
						body, _ := io.ReadAll(r.Body)
						if !strings.Contains(string(body), `"future_field":{"number":12345678901234567890123}`) {
							t.Errorf("unknown field lost: %s", body)
						}
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{}`))
				}))
				defer server.Close()
				other, _ := json.Marshal(map[string]any{
					"self_hosted": true, "responses_ws_self_hosted": true,
					"codex": map[string]string{"default_user_agent": tc.fallbackUA, "default_originator": tc.fallbackOriginator},
				})
				p := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, string(other), map[string]string{"User-Agent": tc.ua, "originator": tc.originator})
				p.Channel.BaseURL = stringPtr(server.URL)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				var req *http.Request
				var apiErr *types.OpenAIErrorWithStatusCode
				if entry == "responses-ws" || entry == "realtime" {
					var plan *codexRealtimeConnPlan
					if entry == "responses-ws" {
						frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi"}`))
						if err != nil {
							t.Fatal(err)
						}
						openPlan, errWithCode := p.prepareResponsesWSOfficialConn(ctx, &responsesws.OpenRequest{FirstFrame: frame, InboundHeaders: requestctx.NewHeaderSnapshot(p.Context.Request.Header)}, "gpt-5", "session")
						if errWithCode != nil {
							t.Fatal(errWithCode)
						}
						plan = openPlan.conn
					} else {
						plan, apiErr = p.prepareChatRealtimeConn("gpt-5", "session")
						if apiErr != nil {
							t.Fatal(apiErr)
						}
					}
					conn, errWithCode := p.dialChatRealtimeConnWithContext(ctx, plan)
					if errWithCode != nil {
						t.Fatal(errWithCode)
					}
					defer conn.Close(wsconn.CloseInfo{})
				} else {
					switch entry {
					case "usage":
						headers, err := p.getUsageRequestHeaders(ctx)
						if err != nil {
							t.Fatal(err)
						}
						httpRequester := p.codexRequester()
						req, err = httpRequester.NewRequest(http.MethodGet, server.URL, httpRequester.WithHeader(headers), httpRequester.WithContext(ctx))
						if err != nil {
							t.Fatal(err)
						}
					case "chat":
						raw, errWithCode := p.chatResponsesRequestFromTyped(&types.OpenAIResponsesRequest{Model: "gpt-5"})
						if errWithCode != nil {
							t.Fatal(errWithCode)
						}
						req, apiErr = p.prepareResponsesCreateRequest(ctx, raw)
					default:
						body, err := commonresponses.ParseRawEnvelope([]byte(`{"model":"gpt-5","input":"hi","future_field":{"number":12345678901234567890123}}`))
						if err != nil {
							t.Fatal(err)
						}
						raw := &commonresponses.Request{Model: "gpt-5", Body: body, Headers: requestctx.NewHeaderSnapshot(p.Context.Request.Header)}
						if entry == "compact" {
							req, apiErr = p.prepareResponsesCompactRequest(ctx, raw)
						} else {
							req, apiErr = p.prepareResponsesCreateRequest(ctx, raw)
						}
					}
					if apiErr != nil {
						t.Fatal(apiErr)
					}
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					resp.Body.Close()
				}
				select {
				case headers := <-seen:
					if headers.Get("User-Agent") != tc.wantUA || headers.Get("originator") != tc.wantOriginator {
						t.Fatalf("wire UA=%q originator=%q, want %q %q", headers.Get("User-Agent"), headers.Get("originator"), tc.wantUA, tc.wantOriginator)
					}
					if tc.wantUA == "" && len(headers.Values("User-Agent")) != 0 {
						t.Fatal("missing UA was emitted")
					}
					if tc.wantOriginator == "" && len(headers.Values("originator")) != 0 {
						t.Fatal("missing originator was emitted")
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			})
		}
	}
}

func TestCodexClientIdentityRejectedBeforeCredentials(t *testing.T) {
	p := newTestCodexProviderWithContext(t, `{}`, "", map[string]string{"User-Agent": "codex-tui/1\r\nInjected: bad"})
	body, _ := commonresponses.ParseRawEnvelope([]byte(`{"model":"gpt-5","input":"hi"}`))
	raw := &commonresponses.Request{Model: "gpt-5", Body: body, Headers: requestctx.NewHeaderSnapshot(p.Context.Request.Header)}
	_, apiErr := p.prepareResponsesCreateRequest(context.Background(), raw)
	if apiErr == nil || apiErr.StatusCode != 400 || apiErr.Param != "User-Agent" {
		t.Fatalf("expected identity rejection before token error: %+v", apiErr)
	}
	_, apiErr = p.prepareChatRealtimeConn("gpt-5", "session")
	if apiErr == nil || apiErr.StatusCode != 400 || apiErr.Param != "User-Agent" {
		t.Fatalf("expected realtime identity rejection before token error: %+v", apiErr)
	}
	_, err := p.getUsageRequestHeaders(context.Background())
	if _, ok := err.(*wire.Violation); !ok {
		t.Fatalf("expected usage identity rejection before token error: %v", err)
	}
}

func TestRealtimeHeaderErrorsPreserveTheirSource(t *testing.T) {
	for _, tc := range []struct {
		name, other, ua, wantCode string
		wantStatus                int
		wantNotAttempted          bool
	}{
		{"client identity", "", "codex-tui/1\r\nInjected: bad", "invalid_request_error", 400, false},
		{"policy", `{"codex":{"default_user_agent":true}}`, "codex-tui/1", "channel_config_error", 503, false},
		{"credentials", "", "codex-tui/1", "codex_token_error", 503, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 空凭据同时验证身份／配置错误在获取 token 前返回。
			p := newTestCodexProviderWithContext(t, `{}`, tc.other, map[string]string{"User-Agent": tc.ua})
			_, err := p.getRealtimeHeaders("session")
			if err == nil {
				t.Fatal("expected header preparation failure")
			}
			mapped := p.requestHeaderError(fmt.Errorf("header preparation: %w", err))
			_, actual := p.prepareChatRealtimeConn("gpt-5", "session")
			for _, apiErr := range []*types.OpenAIErrorWithStatusCode{mapped, actual} {
				if apiErr == nil || apiErr.StatusCode != tc.wantStatus || apiErr.Code != tc.wantCode ||
					!apiErr.LocalError || apiErr.UpstreamNotAttempted != tc.wantNotAttempted || apiErr.ProviderOpenRetrySafe {
					t.Fatalf("incorrect error disposition: %+v", apiErr)
				}
			}
		})
	}

	p := &CodexProvider{}
	err := fmt.Errorf("header preparation: %w", &codexHeaderTokenError{cause: ErrOAuthRefreshOutcomeAmbiguous})
	if !errors.Is(err, ErrOAuthRefreshOutcomeAmbiguous) {
		t.Fatal("lost credential error chain")
	}
	apiErr := p.requestHeaderError(err)
	if apiErr.StatusCode != http.StatusUnauthorized || !apiErr.ProviderAuthRejected || apiErr.UpstreamNotAttempted || apiErr.ProviderOpenRetrySafe {
		t.Fatalf("ambiguous refresh changed error disposition: %+v", apiErr)
	}
}
