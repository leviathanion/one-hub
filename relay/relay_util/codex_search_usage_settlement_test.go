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

func TestCodexSearchUsageSettlesThroughAttemptOnce(t *testing.T) {
	for _, test := range []struct {
		name        string
		serviceType string
		termination toolUsageEvidenceCodexTermination
		withTokens  bool
	}{
		{name: "web_search provider close", serviceType: types.APIToolTypeWebSearch, termination: toolUsageEvidenceCodexProviderClose},
		{name: "web_search_preview provider close", serviceType: types.APIToolTypeWebSearchPreview, termination: toolUsageEvidenceCodexProviderClose},
		{name: "web_search_preview top level error", serviceType: types.APIToolTypeWebSearchPreview, termination: toolUsageEvidenceCodexTopLevelError},
		{name: "web_search top level error", serviceType: types.APIToolTypeWebSearch, termination: toolUsageEvidenceCodexTopLevelError},
		{name: "web_search client abort", serviceType: types.APIToolTypeWebSearch, termination: toolUsageEvidenceCodexClientAbort},
		{name: "web_search_preview client abort", serviceType: types.APIToolTypeWebSearchPreview, termination: toolUsageEvidenceCodexClientAbort},
		{name: "web_search_preview cancelled", serviceType: types.APIToolTypeWebSearchPreview, termination: toolUsageEvidenceCodexCancelled},
		{name: "web_search cancelled", serviceType: types.APIToolTypeWebSearch, termination: toolUsageEvidenceCodexCancelled},
		{name: "web_search completed with tokens", serviceType: types.APIToolTypeWebSearch, termination: toolUsageEvidenceCodexCompleted, withTokens: true},
		{name: "web_search_preview failed with tokens", serviceType: types.APIToolTypeWebSearchPreview, termination: toolUsageEvidenceCodexFailed, withTokens: true},
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
			upstreamURL, terminationSent, cleanupUpstream := toolUsageEvidenceCodexRealtimeUpstream(t, test.serviceType, test.termination)
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
			t.Cleanup(func() {
				session.Abort("issue_048_test_cleanup")
				// Detached readers and their pumps can outlive the finalizer.
				// Join them before the fixture restores process-wide DB/logger state.
				stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
				defer stopCancel()
				if err := codexprovider.StopExecutionSessionRuntime(stopCtx); err != nil {
					t.Errorf("stop Codex test runtime: %v", err)
				}
			})
			finalized := make(chan struct{})
			observerFactory := NewRealtimeTurnObserverFactory(c, models, nil)
			session.SetTurnObserverFactory(func() runtimesession.TurnObserver {
				return &toolUsageEvidenceNotifyingTurnObserver{RealtimeTurnObserver: observerFactory().(*RealtimeTurnObserver), done: finalized}
			})

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := session.SendClient(ctx, runtimerealtime.NewTextFrame([]byte(`{"type":"response.create","event_id":"issue-048-create","model":"gpt-test","input":"hello"}`))); err != nil {
				t.Fatalf("send Codex response.create: %v", err)
			}
			billingKey := types.BuildExtraBillingKey(test.serviceType, "medium")
			usage := toolUsageEvidenceReceiveCodexSearchUsage(t, ctx, session, billingKey)
			if usage.ProviderTokenEvidence || usage.ExtraBilling[billingKey].CallCount != 1 || !usage.ProviderExtraBilling[billingKey] {
				t.Fatalf("reader did not return one provider-backed search delta: %+v", usage)
			}
			if test.termination == toolUsageEvidenceCodexClientAbort {
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
			waitToolUsageEvidenceCodexSQLSettlement(t, startingQuota, wantCharge, 3*boolInt(test.withTokens), 2*boolInt(test.withTokens))
			// A provider close can race the downstream cleanup, and a terminal
			// event can be followed by a cleanup abort. Both must keep the same
			// Attempt settlement result.
			session.Abort("issue_048_duplicate_close")
			waitToolUsageEvidenceCodexSQLSettlement(t, startingQuota, wantCharge, 3*boolInt(test.withTokens), 2*boolInt(test.withTokens))
		})
	}
}

type toolUsageEvidenceNotifyingTurnObserver struct {
	*RealtimeTurnObserver
	done chan struct{}
	once sync.Once
}

func (o *toolUsageEvidenceNotifyingTurnObserver) FinalizeTurn(payload runtimesession.TurnFinalizePayload) {
	o.RealtimeTurnObserver.FinalizeTurn(payload)
	o.once.Do(func() { close(o.done) })
}

type toolUsageEvidenceCodexTermination string

const (
	toolUsageEvidenceCodexProviderClose toolUsageEvidenceCodexTermination = "provider_close"
	toolUsageEvidenceCodexTopLevelError toolUsageEvidenceCodexTermination = "top_level_error"
	toolUsageEvidenceCodexClientAbort   toolUsageEvidenceCodexTermination = "client_abort"
	toolUsageEvidenceCodexCancelled     toolUsageEvidenceCodexTermination = "cancelled"
	toolUsageEvidenceCodexCompleted     toolUsageEvidenceCodexTermination = "completed"
	toolUsageEvidenceCodexFailed        toolUsageEvidenceCodexTermination = "failed"
)

func toolUsageEvidenceCodexRealtimeUpstream(t *testing.T, serviceType string, termination toolUsageEvidenceCodexTermination) (string, <-chan struct{}, func()) {
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
			if termination == toolUsageEvidenceCodexClientAbort {
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
				case toolUsageEvidenceCodexProviderClose:
					conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindNormal, Code: wsconn.CloseNormalClosure, Reason: "issue_048_provider_close"})
					markTermination()
				case toolUsageEvidenceCodexTopLevelError:
					write(`{"type":"error","error":{"type":"server_error","message":"issue_048_provider_error"}}`)
					markTermination()
				case toolUsageEvidenceCodexCancelled:
					write(fmt.Sprintf(`{"type":"response.cancelled","response":{"id":%q,"status":"cancelled"}}`, responseID))
					markTermination()
				case toolUsageEvidenceCodexCompleted:
					write(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`, responseID))
					markTermination()
				case toolUsageEvidenceCodexFailed:
					write(fmt.Sprintf(`{"type":"response.failed","response":{"id":%q,"status":"failed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`, responseID))
					markTermination()
				case toolUsageEvidenceCodexClientAbort:
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

func toolUsageEvidenceReceiveCodexSearchUsage(t *testing.T, ctx context.Context, session runtimerealtime.RealtimeSession, billingKey string) *types.UsageEvent {
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

func waitToolUsageEvidenceCodexSQLSettlement(t *testing.T, startingQuota int, wantCharge int64, wantPrompt, wantCompletion int) {
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
