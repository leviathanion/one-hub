package relay_util

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
	"one-api/model"
	"one-api/providers/base"
	codexprovider "one-api/providers/codex"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
	"one-api/types"
)

func TestIssue048CodexSearchUsageSettlesThroughAttemptOnce(t *testing.T) {
	for _, test := range []struct {
		name        string
		serviceType string
		termination issue048CodexTermination
		withTokens  bool
	}{
		{name: "web_search provider close", serviceType: types.APIToolTypeWebSearch, termination: issue048CodexProviderClose},
		{name: "web_search_preview provider close", serviceType: types.APIToolTypeWebSearchPreview, termination: issue048CodexProviderClose},
		{name: "web_search_preview top level error", serviceType: types.APIToolTypeWebSearchPreview, termination: issue048CodexTopLevelError},
		{name: "web_search top level error", serviceType: types.APIToolTypeWebSearch, termination: issue048CodexTopLevelError},
		{name: "web_search client abort", serviceType: types.APIToolTypeWebSearch, termination: issue048CodexClientAbort},
		{name: "web_search_preview client abort", serviceType: types.APIToolTypeWebSearchPreview, termination: issue048CodexClientAbort},
		{name: "web_search_preview cancelled", serviceType: types.APIToolTypeWebSearchPreview, termination: issue048CodexCancelled},
		{name: "web_search cancelled", serviceType: types.APIToolTypeWebSearch, termination: issue048CodexCancelled},
		{name: "web_search completed with tokens", serviceType: types.APIToolTypeWebSearch, termination: issue048CodexCompleted, withTokens: true},
		{name: "web_search_preview failed with tokens", serviceType: types.APIToolTypeWebSearchPreview, termination: issue048CodexFailed, withTokens: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := realtimeBillingFixture(t)
			const startingQuota = 100000
			if err := model.DB.Model(&model.User{}).Where("id = ?", 1).Update("quota", startingQuota).Error; err != nil {
				t.Fatalf("set Codex integration user quota: %v", err)
			}
			if err := model.DB.Model(&model.Token{}).Where("id = ?", 1).Update("remain_quota", startingQuota).Error; err != nil {
				t.Fatalf("set Codex integration token quota: %v", err)
			}
			originalLog := config.LogConsumeEnabled
			config.LogConsumeEnabled = true
			t.Cleanup(func() { config.LogConsumeEnabled = originalLog })
			c.Set("self_hosted", true)
			c.Request.Header.Set("x-session-id", "issue-048-"+strings.ReplaceAll(test.name, " ", "-"))
			upstreamURL, terminationSent, cleanupUpstream := issue048CodexRealtimeUpstream(t, test.serviceType, test.termination)
			t.Cleanup(cleanupUpstream)
			httpBaseURL := "http" + strings.TrimPrefix(upstreamURL, "ws")
			proxy := ""
			channel := &model.Channel{
				Id:      48048,
				Key:     `{"access_token":"issue-048-static-token","account_id":"issue-048-account"}`,
				Other:   `{"self_hosted":true}`,
				Proxy:   &proxy,
				BaseURL: &httpBaseURL,
			}
			channel.SetProxy()
			provider, ok := codexprovider.CodexProviderFactory{}.Create(channel).(*codexprovider.CodexProvider)
			if !ok || provider == nil {
				t.Fatalf("create Codex provider: %T", provider)
			}
			provider.SetContext(c)
			models := runtimesession.ModelBinding{RequestedModel: "gpt-test", ProviderModel: "gpt-test", BillingModel: "gpt-test"}
			realtimeProvider, ok := any(provider).(base.RealtimeSessionProviderWithOptions)
			if !ok {
				t.Fatalf("Codex provider lost Realtime session interface: %T", provider)
			}
			session, apiErr := realtimeProvider.OpenRealtimeSessionWithOptions("gpt-test", runtimerealtime.RealtimeOpenOptions{
				Models:     models,
				Context:    context.Background(),
				ForceFresh: true,
			})
			if apiErr != nil || session == nil {
				t.Fatalf("open Codex Realtime session: session=%T error=%+v", session, apiErr)
			}
			t.Cleanup(func() { session.Abort("issue_048_test_cleanup") })
			finalized := make(chan struct{})
			observerFactory := NewRealtimeTurnObserverFactory(c, models, nil)
			session.SetTurnObserverFactory(func() runtimesession.TurnObserver {
				return &issue048NotifyingTurnObserver{RealtimeTurnObserver: observerFactory().(*RealtimeTurnObserver), done: finalized}
			})

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := session.SendClient(ctx, runtimerealtime.NewTextFrame([]byte(`{"type":"response.create","event_id":"issue-048-create","model":"gpt-test","input":"hello"}`))); err != nil {
				t.Fatalf("send Codex response.create: %v", err)
			}
			billingKey := types.BuildExtraBillingKey(test.serviceType, "medium")
			usage := issue048ReceiveCodexSearchUsage(t, ctx, session, billingKey)
			if usage.ProviderTokenEvidence || usage.ExtraBilling[billingKey].CallCount != 1 || !usage.ProviderExtraBilling[billingKey] {
				t.Fatalf("reader did not return one provider-backed search delta: %+v", usage)
			}
			if test.termination == issue048CodexClientAbort {
				session.Abort("issue_048_client_abort")
			}
			select {
			case <-terminationSent:
			case <-ctx.Done():
				t.Fatalf("Codex upstream termination control was not exercised: %s", test.termination)
			}
			// 先等待真实结算完成，避免测试轮询与 SQLite 余额事务争抢表锁。
			select {
			case <-finalized:
			case <-ctx.Done():
				t.Fatalf("Codex turn settlement did not finish: %s", test.termination)
			}

			wantCharge := int64(5000)
			if test.withTokens {
				wantCharge += 5
			}
			waitIssue048CodexSQLSettlement(t, startingQuota, wantCharge, 3*boolInt(test.withTokens), 2*boolInt(test.withTokens))
			// A provider close can race the downstream cleanup, and a terminal
			// event can be followed by a cleanup abort. Both must keep the same
			// Attempt settlement result.
			session.Abort("issue_048_duplicate_close")
			waitIssue048CodexSQLSettlement(t, startingQuota, wantCharge, 3*boolInt(test.withTokens), 2*boolInt(test.withTokens))
		})
	}
}

type issue048NotifyingTurnObserver struct {
	*RealtimeTurnObserver
	done chan struct{}
	once sync.Once
}

func (o *issue048NotifyingTurnObserver) FinalizeTurn(payload runtimesession.TurnFinalizePayload) {
	o.RealtimeTurnObserver.FinalizeTurn(payload)
	o.once.Do(func() { close(o.done) })
}

type issue048CodexTermination string

const (
	issue048CodexProviderClose issue048CodexTermination = "provider_close"
	issue048CodexTopLevelError issue048CodexTermination = "top_level_error"
	issue048CodexClientAbort   issue048CodexTermination = "client_abort"
	issue048CodexCancelled     issue048CodexTermination = "cancelled"
	issue048CodexCompleted     issue048CodexTermination = "completed"
	issue048CodexFailed        issue048CodexTermination = "failed"
)

func issue048CodexRealtimeUpstream(t *testing.T, serviceType string, termination issue048CodexTermination) (string, <-chan struct{}, func()) {
	t.Helper()
	terminationSent := make(chan struct{})
	pumpDone := make(chan struct{})
	accepted := make(chan *wsconn.ManagedConn, 1)
	var terminationOnce sync.Once
	markTermination := func() {
		terminationOnce.Do(func() { close(terminationSent) })
	}
	upstreamURL, closeServer := wstest.Server(t, func(conn *wsconn.ManagedConn) {
		accepted <- conn
		defer close(pumpDone)
		defer func() {
			if termination == issue048CodexClientAbort {
				markTermination()
			}
		}()
		var sent bool
		pump := wsconn.Pump{
			Conn: conn,
			Handle: func(_ context.Context, messageType wsconn.MessageType, payload []byte) {
				if sent || messageType != wsconn.TextMessage {
					return
				}
				var envelope struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(payload, &envelope) != nil || strings.TrimSpace(envelope.Type) != "response.create" {
					return
				}
				sent = true
				responseID := "issue-048-response"
				write := func(body string) {
					if err := conn.WriteMessage(wsconn.TextMessage, []byte(body)); err != nil {
						t.Errorf("write Codex I048 upstream event: %v", err)
					}
				}
				write(fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"status":"in_progress","tools":[{"type":%q,"search_context_size":"medium"}]}}`, responseID, serviceType))
				write(`{"type":"response.output_item.done","item_id":"issue-048-search","output_index":0,"item":{"id":"issue-048-search","type":"web_search_call","status":"completed","action":{"type":"search"}}}`)
				write(`{"type":"response.output_item.done","item_id":"issue-048-search","output_index":0,"item":{"id":"issue-048-search","type":"web_search_call","status":"completed","action":{"type":"search"}}}`)
				switch termination {
				case issue048CodexProviderClose:
					conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindNormal, Code: wsconn.CloseNormalClosure, Reason: "issue_048_provider_close"})
					markTermination()
				case issue048CodexTopLevelError:
					write(`{"type":"error","error":{"type":"server_error","message":"issue_048_provider_error"}}`)
					markTermination()
				case issue048CodexCancelled:
					write(fmt.Sprintf(`{"type":"response.cancelled","response":{"id":%q,"status":"cancelled"}}`, responseID))
					markTermination()
				case issue048CodexCompleted:
					write(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`, responseID))
					markTermination()
				case issue048CodexFailed:
					write(fmt.Sprintf(`{"type":"response.failed","response":{"id":%q,"status":"failed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`, responseID))
					markTermination()
				case issue048CodexClientAbort:
					// The test drives the local client abort after it has received done.
				}
			},
		}
		pump.Run(context.Background())
	})
	cleanup := func() {
		var conn *wsconn.ManagedConn
		select {
		case conn = <-accepted:
		case <-pumpDone:
		case <-time.After(2 * time.Second):
			t.Errorf("timed out waiting for Codex I048 upstream websocket before cleanup")
		}
		if conn != nil {
			conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "issue_048_test_cleanup"})
		}
		closeServer()
		select {
		case <-pumpDone:
		case <-time.After(2 * time.Second):
			t.Errorf("timed out waiting for Codex I048 upstream Pump.Run cleanup")
		}
	}
	return upstreamURL, terminationSent, cleanup
}

func issue048ReceiveCodexSearchUsage(t *testing.T, ctx context.Context, session runtimerealtime.RealtimeSession, billingKey string) *types.UsageEvent {
	t.Helper()
	for {
		event, err := session.Recv(ctx)
		if err != nil {
			t.Fatalf("receive Codex Realtime event before search usage: %v", err)
		}
		if event.Usage == nil || event.Usage.ExtraBilling[billingKey].CallCount == 0 {
			continue
		}
		return event.Usage
	}
}

func waitIssue048CodexSQLSettlement(t *testing.T, startingQuota int, wantCharge int64, wantPrompt, wantCompletion int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var user model.User
		var token model.Token
		var logs []model.Log
		if userErr := model.DB.First(&user, 1).Error; userErr == nil && model.DB.First(&token, 1).Error == nil && model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error == nil && len(logs) == 1 && int64(logs[0].Quota) == wantCharge && logs[0].PromptTokens == wantPrompt && logs[0].CompletionTokens == wantCompletion && logs[0].IsStream && user.Quota == startingQuota-int(wantCharge) && token.RemainQuota == startingQuota-int(wantCharge) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Codex reader settlement did not reach SQL exactly once: user=%+v token=%+v logs=%+v want_charge=%d", user, token, logs, wantCharge)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
