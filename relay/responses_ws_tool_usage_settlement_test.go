package relay

import (
	"bytes"
	"context"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
	"one-api/model"
	"one-api/providers/azure"
	azurev1 "one-api/providers/azure_v1"
	providersBase "one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"
)

type toolUsageEvidenceNativeProviderFactory func(baseURL string) providersBase.ResponsesWSProvider

func toolUsageEvidenceNativeProviderFactories() []struct {
	name    string
	factory toolUsageEvidenceNativeProviderFactory
} {
	return []struct {
		name    string
		factory toolUsageEvidenceNativeProviderFactory
	}{
		{
			name: "openai",
			factory: func(baseURL string) providersBase.ResponsesWSProvider {
				proxy := ""
				channel := &model.Channel{
					Type:    config.ChannelTypeOpenAI,
					Key:     "i048-openai-key",
					BaseURL: &baseURL,
					Proxy:   &proxy,
					Other:   `{"responses_ws_native":true,"responses_ws_self_hosted":true}`,
				}
				return openai.CreateOpenAIProvider(channel, baseURL)
			},
		},
		{
			name: "azure",
			factory: func(baseURL string) providersBase.ResponsesWSProvider {
				proxy := ""
				channel := &model.Channel{
					Type:    config.ChannelTypeAzure,
					Key:     "i048-azure-key",
					BaseURL: &baseURL,
					Proxy:   &proxy,
					Other:   `{"api_version":"2024-10-01-preview","responses_ws_self_hosted":true}`,
				}
				return azure.AzureProviderFactory{}.Create(channel).(providersBase.ResponsesWSProvider)
			},
		},
		{
			name: "azure_v1",
			factory: func(baseURL string) providersBase.ResponsesWSProvider {
				proxy := ""
				channel := &model.Channel{
					Type:    config.ChannelTypeAzureV1,
					Key:     "i048-azure-v1-key",
					BaseURL: &baseURL,
					Proxy:   &proxy,
					Other:   `{"api_version":"2024-10-01-preview","responses_ws_self_hosted":true}`,
				}
				return azurev1.AzureV1ProviderFactory{}.Create(channel).(providersBase.ResponsesWSProvider)
			},
		},
		{
			name: "custom",
			factory: func(baseURL string) providersBase.ResponsesWSProvider {
				proxy := ""
				channel := &model.Channel{Plugin: model.NewCustomEndpointPlugin(),
					Type:    config.ChannelTypeCustom,
					Key:     "i048-custom-key",
					BaseURL: &baseURL,
					Proxy:   &proxy,
					Other:   `{"responses_ws_native":true,"responses_ws_self_hosted":true}`,
				}
				return openai.CreateOpenAIProvider(channel, baseURL)
			},
		},
	}
}

func toolUsageEvidenceNativeProviderServer(t *testing.T, payloads [][]byte, closeAfter bool) (string, <-chan error, func()) {
	return toolUsageEvidenceNativeProviderServerWithClientGate(t, payloads, closeAfter, nil)
}

func toolUsageEvidenceNativeProviderServerWithClientGate(t *testing.T, payloads [][]byte, closeAfter bool, clientGate []byte) (string, <-chan error, func()) {
	return toolUsageEvidenceNativeProviderServerWithClientGateAt(t, payloads, closeAfter, clientGate, 2)
}

func toolUsageEvidenceNativeProviderServerWithClientGateAt(t *testing.T, payloads [][]byte, closeAfter bool, clientGate []byte, clientGateIndex int) (string, <-chan error, func()) {
	t.Helper()
	errCh := make(chan error, 1)
	url, cleanup := wstest.Server(t, func(conn *wsconn.ManagedConn) {
		first := make(chan struct{}, 1)
		gate := make(chan struct{}, 1)
		readDone := make(chan struct{})
		go func() {
			wsconn.Pump{
				Conn: conn,
				Handle: func(_ context.Context, _ wsconn.MessageType, payload []byte) {
					select {
					case first <- struct{}{}:
					default:
					}
					if len(clientGate) > 0 && bytes.Contains(payload, clientGate) {
						select {
						case gate <- struct{}{}:
						default:
						}
					}
				},
			}.Run(context.Background())
			close(readDone)
		}()
		select {
		case <-first:
		case <-time.After(2 * time.Second):
			errCh <- context.DeadlineExceeded
			conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "missing_response_create"})
			return
		}
		for index, payload := range payloads {
			if index == clientGateIndex && len(clientGate) > 0 {
				select {
				case <-gate:
				case <-time.After(2 * time.Second):
					errCh <- context.DeadlineExceeded
					conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "missing_client_gate"})
					return
				}
			}
			if err := conn.WriteMessage(wsconn.TextMessage, payload); err != nil {
				errCh <- err
				return
			}
		}
		if closeAfter {
			conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindPeerClose, Code: wsconn.CloseNormalClosure, Reason: "provider_done"})
			return
		}
		<-conn.Done()
		<-readDone
	})
	return url, errCh, cleanup
}

type toolUsageEvidenceNativeRelayHarness struct {
	actor          *ResponsesWSSessionActor
	attempt        *ResponsesWSTurnAttempt
	session        responsesws.Upstream
	pump           *ResponsesWSIOPump
	providerDone   <-chan error
	providerStop   func()
	userClient     *wsconn.ManagedConn
	userServer     *wsconn.ManagedConn
	userFrames     chan []byte
	userPumpDone   chan struct{}
	clientPumpDone chan struct{}
}

func newToolUsageEvidenceNativeRelayHarness(t *testing.T, providerFactory toolUsageEvidenceNativeProviderFactory, payloads [][]byte, closeAfter bool) *toolUsageEvidenceNativeRelayHarness {
	return newToolUsageEvidenceNativeRelayHarnessWithClientGate(t, providerFactory, payloads, closeAfter, nil, false)
}

func newToolUsageEvidenceNativeRelayHarnessWithClientGate(t *testing.T, providerFactory toolUsageEvidenceNativeProviderFactory, payloads [][]byte, closeAfter bool, clientGate []byte, multiAgentEnabled bool) *toolUsageEvidenceNativeRelayHarness {
	return newToolUsageEvidenceNativeRelayHarnessWithClientGateAt(t, providerFactory, payloads, closeAfter, clientGate, 2, multiAgentEnabled)
}

func newToolUsageEvidenceNativeRelayHarnessWithClientGateAt(t *testing.T, providerFactory toolUsageEvidenceNativeProviderFactory, payloads [][]byte, closeAfter bool, clientGate []byte, clientGateIndex int, multiAgentEnabled bool) *toolUsageEvidenceNativeRelayHarness {
	t.Helper()
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 100000, "attempt-"+t.Name())
	providerURL, providerDone, providerStop := toolUsageEvidenceNativeProviderServerWithClientGateAt(t, payloads, closeAfter, clientGate, clientGateIndex)
	provider := providerFactory(providerURL)
	session, apiErr := provider.OpenResponsesWS(context.Background(), &responsesws.OpenRequest{SelectedModel: "gpt-5", ChannelID: 17})
	if apiErr != nil {
		providerStop()
		t.Fatalf("open native Responses WS: %v", apiErr)
	}

	userClient, userServer := wstest.Pair(t)
	actor := NewResponsesWSSessionActor(ctx)
	attempt.MultiAgentEnabled = multiAgentEnabled
	pump := NewResponsesWSManagedPump(userServer, actor)
	actor.SetPump(pump)
	actor.SetClientConn(userServer)
	generation := actor.AttachUpstreamSession(session, 17)
	attempt.Session = session
	registerResponsesWSTestWork(t, actor, attempt, "")
	actor.state = responsesWSStateInFlight

	harness := &toolUsageEvidenceNativeRelayHarness{
		actor:          actor,
		attempt:        attempt,
		session:        session,
		pump:           pump,
		providerDone:   providerDone,
		providerStop:   providerStop,
		userClient:     userClient,
		userServer:     userServer,
		userFrames:     make(chan []byte, 16),
		userPumpDone:   make(chan struct{}),
		clientPumpDone: make(chan struct{}),
	}
	go func() {
		defer close(harness.clientPumpDone)
		wsconn.Pump{
			Conn: userServer,
			Handle: func(ctx context.Context, mt wsconn.MessageType, payload []byte) {
				actor.onClientFrame(ctx, mt, payload)
			},
			OnClose: actor.onClientConnClosed,
		}.Run(context.Background())
	}()
	go func() {
		defer close(harness.userPumpDone)
		wsconn.Pump{
			Conn: userClient,
			Handle: func(_ context.Context, _ wsconn.MessageType, payload []byte) {
				select {
				case harness.userFrames <- append([]byte(nil), payload...):
				default:
				}
			},
			OnClose: func(info wsconn.CloseInfo) {
				actor.Post(ResponsesWSEventClientClosed{Err: info.Err})
			},
		}.Run(context.Background())
	}()
	t.Cleanup(func() {
		if !actor.closing.closed.Load() {
			actor.close("test_cleanup")
		}
		select {
		case <-actor.Done():
		case <-time.After(time.Second):
			t.Errorf("timed out waiting for native Responses WS actor cleanup")
		}
		actor.waitStartedGoroutines()
		pump.Close()
		session.Abort("test_cleanup")
		providerStop()
		userServer.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
		userClient.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
		select {
		case <-harness.userPumpDone:
		case <-time.After(time.Second):
			t.Errorf("timed out waiting for native Responses WS client pump cleanup")
		}
		select {
		case <-harness.clientPumpDone:
		case <-time.After(time.Second):
			t.Errorf("timed out waiting for native Responses WS server pump cleanup")
		}
	})
	actor.Start()
	pump.ArmProviderRecvPump(generation, 17, session)
	result := session.SendClientWithResult(context.Background(), responsesws.SendRequest{
		AttemptID: attempt.AttemptID,
		Frame:     responsesws.NewTextFrame([]byte(`{"type":"response.create","model":"gpt-5","tools":[{"type":"web_search_preview","search_context_size":"medium"}]}`)),
	})
	if result.Status != responsesws.ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("native response.create send failed: %+v", result)
	}
	return harness
}

func toolUsageEvidenceWaitNativeFrame(t *testing.T, harness *toolUsageEvidenceNativeRelayHarness, want []byte) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case payload := <-harness.userFrames:
			if bytes.Equal(payload, want) {
				return
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for native downstream frame %q", want)
		}
	}
}

func toolUsageEvidenceWaitNativeRelayDone(t *testing.T, harness *toolUsageEvidenceNativeRelayHarness) {
	t.Helper()
	select {
	case <-harness.actor.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for native Responses WS relay settlement")
	}
	select {
	case err := <-harness.providerDone:
		if err != nil {
			t.Fatalf("native provider fixture failed: %v", err)
		}
	default:
	}
}

func toolUsageEvidenceCollectNativeProviderUsage(t *testing.T, providerFactory toolUsageEvidenceNativeProviderFactory, payloads [][]byte) *types.Usage {
	t.Helper()
	providerURL, _, providerStop := toolUsageEvidenceNativeProviderServer(t, payloads, false)
	provider := providerFactory(providerURL)
	session, apiErr := provider.OpenResponsesWS(context.Background(), &responsesws.OpenRequest{SelectedModel: "gpt-5", ChannelID: 17})
	if apiErr != nil {
		providerStop()
		t.Fatalf("open native Responses WS: %v", apiErr)
	}
	t.Cleanup(func() {
		session.Abort("test_cleanup")
		providerStop()
	})
	result := session.SendClientWithResult(context.Background(), responsesws.SendRequest{
		AttemptID: "attempt-" + t.Name(),
		Frame:     responsesws.NewTextFrame([]byte(`{"type":"response.create","model":"gpt-5"}`)),
	})
	if result.Status != responsesws.ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("native response.create send failed: %+v", result)
	}
	usage := &types.Usage{}
	for index, wantPayload := range payloads {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		event, err := session.Recv(ctx)
		cancel()
		if err != nil {
			t.Fatalf("receive native provider frame %d: %v", index, err)
		}
		if event.Frame == nil || !bytes.Equal(event.Frame.Payload(), wantPayload) {
			t.Fatalf("native provider frame %d changed or was lost: got=%+v want=%q", index, event, wantPayload)
		}
		mergeResponsesWSUsageEvent(usage, event.Usage)
	}
	return usage
}

func toolUsageEvidenceAssertNativeProviderSearchUsage(t *testing.T, usage *types.Usage, key string, wantCount int) {
	t.Helper()
	if usage == nil {
		t.Fatal("native provider usage is nil")
	}
	billing := usage.ExtraBilling[key]
	if billing.CallCount != wantCount || !usage.HasProviderExtraBilling(key) {
		t.Fatalf("native provider search evidence mismatch: usage=%+v key=%q want_count=%d", usage, key, wantCount)
	}
}

func toolUsageEvidenceAssertNativeSearchSettlement(t *testing.T, attempt *ResponsesWSTurnAttempt, key string, wantCharge int64) {
	t.Helper()
	if attempt == nil || attempt.Usage == nil {
		t.Fatalf("native attempt has no usage: %+v", attempt)
	}
	billing := attempt.Usage.ExtraBilling[key]
	if billing.CallCount != 1 || !attempt.Usage.HasProviderExtraBilling(key) {
		t.Fatalf("native search evidence mismatch: usage=%+v key=%q", attempt.Usage, key)
	}
	if attempt.AppliedSettlement == nil || attempt.AppliedSettlement.AppliedFinalQuota != wantCharge || !attempt.QuotaFinalized || attempt.RolledBack {
		t.Fatalf("native search settlement mismatch: attempt=%+v applied=%+v want_charge=%d", attempt, attempt.AppliedSettlement, wantCharge)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatalf("read native Responses WS consume logs: %v", err)
	}
	if len(logs) != 1 || logs[0].Quota != int(wantCharge) {
		t.Fatalf("native search settlement log mismatch: logs=%+v want=%d", logs, wantCharge)
	}
}

func toolUsageEvidenceAssertNativeNoCharge(t *testing.T, attempt *ResponsesWSTurnAttempt) {
	t.Helper()
	if attempt == nil || attempt.Usage == nil {
		t.Fatalf("native attempt has no usage: %+v", attempt)
	}
	if len(attempt.Usage.ExtraBilling) != 0 || len(attempt.Usage.ProviderExtraBilling) != 0 {
		t.Fatalf("unknown native tool produced billing evidence: %+v", attempt.Usage)
	}
	if attempt.AppliedSettlement == nil || !attempt.RolledBack || attempt.AppliedSettlement.AppliedFinalQuota != 0 {
		t.Fatalf("unknown native tool settlement was not rolled back: %+v", attempt)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatalf("read native Responses WS consume logs: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("unknown native tool wrote a consume log: %+v", logs)
	}
}

func TestNativeResponsesWSSearchDoneReachesSQLAcrossFactories(t *testing.T) {
	for _, search := range []struct {
		name    string
		service string
	}{
		{name: "preview", service: types.APIToolTypeWebSearchPreview},
		{name: "ga", service: types.APIToolTypeWebSearch},
	} {
		for _, factory := range toolUsageEvidenceNativeProviderFactories() {
			t.Run(search.name+"/"+factory.name, func(t *testing.T) {
				payloads := [][]byte{
					[]byte(`{"type":"response.created","response":{"id":"resp-i048","model":"native-actual","service_tier":"priority","status":"in_progress","tools":[{"type":"` + search.service + `","search_context_size":"medium"}]}}`),
					[]byte(`{"type":"response.output_item.done","response_id":"resp-i048","event_id":"evt-i048-search","output_index":0,"item":{"id":"search-i048","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
					[]byte(`{"type":"response.output_item.done","response_id":"resp-i048","event_id":"evt-i048-search-duplicate","output_index":0,"item":{"id":"search-i048","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
				}
				harness := newToolUsageEvidenceNativeRelayHarness(t, factory.factory, payloads, true)
				toolUsageEvidenceWaitNativeRelayDone(t, harness)
				key := types.BuildExtraBillingKey(search.service, "medium")
				toolUsageEvidenceAssertNativeSearchSettlement(t, harness.attempt, key, 5000)
				if harness.attempt.Usage.ResponseModel != "native-actual" || harness.attempt.Usage.ServiceTier != "priority" {
					t.Fatalf("native search attribution was lost: %+v", harness.attempt.Usage)
				}
				harness.actor.close("duplicate_close")
				toolUsageEvidenceAssertNativeSearchSettlement(t, harness.attempt, key, 5000)
			})
		}
	}
}

func TestNativeResponsesWSSearchTerminalUsageDoesNotDoubleCharge(t *testing.T) {
	for _, factory := range toolUsageEvidenceNativeProviderFactories() {
		t.Run(factory.name, func(t *testing.T) {
			payloads := [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-token","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"medium"}]}}`),
				[]byte(`{"type":"response.output_item.done","response_id":"resp-i048-token","event_id":"evt-i048-token-search","output_index":0,"item":{"id":"search-i048-token","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
				[]byte(`{"type":"response.completed","sequence_number":2,"response":{"id":"resp-i048-token","status":"completed","usage":{"input_tokens":30,"output_tokens":2,"total_tokens":32},"tools":[{"type":"web_search_preview","search_context_size":"medium"}],"output":[{"id":"search-i048-token","type":"web_search_call","status":"completed","action":{"type":"search"}}]}}`),
			}
			harness := newToolUsageEvidenceNativeRelayHarness(t, factory.factory, payloads, true)
			toolUsageEvidenceWaitNativeRelayDone(t, harness)
			key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
			toolUsageEvidenceAssertNativeSearchSettlement(t, harness.attempt, key, 5032)
			if harness.attempt.TerminalUsage == nil || harness.attempt.TerminalUsage.PromptTokens != 30 || harness.attempt.TerminalUsage.CompletionTokens != 2 || harness.attempt.TerminalUsage.TotalTokens != 32 {
				t.Fatalf("native terminal token snapshot was not preserved: %+v", harness.attempt.TerminalUsage)
			}
		})
	}
}

func TestNativeResponsesWSImageTerminalWithoutTokensUsesOneUnit(t *testing.T) {
	for _, factory := range toolUsageEvidenceNativeProviderFactories() {
		t.Run(factory.name, func(t *testing.T) {
			payloads := [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-image","status":"in_progress","tools":[{"type":"image_generation","model":"gpt-image-1-mini","quality":"medium","size":"1024x1024"}]}}`),
				[]byte(`{"type":"response.output_item.done","response_id":"resp-i048-image","event_id":"evt-i048-image","output_index":0,"item":{"id":"image-i048","type":"image_generation_call","status":"completed","quality":"medium","size":"1024x1024"}}`),
				[]byte(`{"type":"response.completed","sequence_number":2,"response":{"id":"resp-i048-image","status":"completed","tools":[{"type":"image_generation","model":"gpt-image-1-mini","quality":"medium","size":"1024x1024"}],"output":[{"id":"image-i048","type":"image_generation_call","status":"completed","quality":"medium","size":"1024x1024"}]}}`),
			}
			harness := newToolUsageEvidenceNativeRelayHarness(t, factory.factory, payloads, true)
			toolUsageEvidenceWaitNativeRelayDone(t, harness)
			key := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-1-mini|medium|1024x1024|0")
			toolUsageEvidenceAssertNativeSearchSettlement(t, harness.attempt, key, 5500)
		})
	}
}

func TestNativeResponsesWSUnknownToolAndDuplicateCloseStayUnbilled(t *testing.T) {
	for _, factory := range toolUsageEvidenceNativeProviderFactories() {
		t.Run(factory.name, func(t *testing.T) {
			payloads := [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-unknown","status":"in_progress","tools":[{"type":"future_hosted_tool"}]}}`),
				[]byte(`{"type":"response.output_item.done","response_id":"resp-i048-unknown","event_id":"evt-i048-unknown","output_index":0,"item":{"id":"unknown-i048","type":"future_hosted_call","status":"completed"}}`),
			}
			harness := newToolUsageEvidenceNativeRelayHarness(t, factory.factory, payloads, true)
			toolUsageEvidenceWaitNativeRelayDone(t, harness)
			toolUsageEvidenceAssertNativeNoCharge(t, harness.attempt)
			// close() is the actor's idempotent terminal path. A second close must
			// not reopen the attempt or create another SQL action.
			harness.actor.close("duplicate_close")
			toolUsageEvidenceAssertNativeNoCharge(t, harness.attempt)
		})
	}
}

func TestNativeResponsesWSDeclaredSearchWithoutDoneStaysUnbilled(t *testing.T) {
	for _, factory := range toolUsageEvidenceNativeProviderFactories() {
		t.Run(factory.name, func(t *testing.T) {
			payloads := [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-declared-only","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"medium"}]}}`),
			}
			harness := newToolUsageEvidenceNativeRelayHarness(t, factory.factory, payloads, true)
			toolUsageEvidenceWaitNativeRelayDone(t, harness)
			toolUsageEvidenceAssertNativeNoCharge(t, harness.attempt)
		})
	}
}

func TestNativeResponsesWSClientAbortKeepsAcceptedSearch(t *testing.T) {
	for _, factory := range toolUsageEvidenceNativeProviderFactories() {
		t.Run(factory.name, func(t *testing.T) {
			payloads := [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-abort","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"medium"}]}}`),
				[]byte(`{"type":"response.output_item.done","response_id":"resp-i048-abort","event_id":"evt-i048-abort","output_index":0,"item":{"id":"search-i048-abort","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
			}
			harness := newToolUsageEvidenceNativeRelayHarness(t, factory.factory, payloads, false)
			key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
			toolUsageEvidenceWaitNativeFrame(t, harness, payloads[1])
			harness.userClient.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Code: wsconn.CloseAbnormalClosure, Reason: "client_abort", Err: context.Canceled})
			toolUsageEvidenceWaitNativeRelayDone(t, harness)
			toolUsageEvidenceAssertNativeSearchSettlement(t, harness.attempt, key, 5000)
		})
	}
}

func TestNativeResponsesWSInjectKeepsSearchOwnerAndDeduplicates(t *testing.T) {
	for _, factory := range toolUsageEvidenceNativeProviderFactories() {
		t.Run(factory.name, func(t *testing.T) {
			payloads := [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-inject","model":"native-inject-model","service_tier":"priority","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"low"}]}}`),
				[]byte(`{"type":"response.output_item.done","response_id":"resp-i048-inject","event_id":"evt-i048-inject-search","output_index":0,"item":{"id":"search-i048-inject","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
				[]byte(`{"type":"response.output_item.done","response_id":"resp-i048-inject","event_id":"evt-i048-inject-duplicate","output_index":0,"item":{"id":"search-i048-inject","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
			}
			inject := []byte(`{"type":"response.inject","event_id":"evt-i048-inject-client","response_id":"resp-i048-inject","input":[{"type":"function_call_output","call_id":"call-i048-inject","output":"ok"}]}`)
			harness := newToolUsageEvidenceNativeRelayHarnessWithClientGate(t, factory.factory, payloads, true, []byte(`"type":"response.inject"`), true)
			toolUsageEvidenceWaitNativeFrame(t, harness, payloads[1])
			if err := harness.userClient.WriteMessage(wsconn.TextMessage, inject); err != nil {
				t.Fatalf("send native response.inject: %v", err)
			}
			toolUsageEvidenceWaitNativeRelayDone(t, harness)
			key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "low")
			toolUsageEvidenceAssertNativeSearchSettlement(t, harness.attempt, key, 5000)
			if harness.attempt.SeenProviderResponseID != "resp-i048-inject" || harness.attempt.Usage.ResponseModel != "native-inject-model" || harness.attempt.Usage.ServiceTier != "priority" {
				t.Fatalf("inject changed current response attribution: %+v", harness.attempt.Usage)
			}
		})
	}
}

func TestNativeResponsesWSRepeatedCreatedKeepsOwnerTracker(t *testing.T) {
	for _, factory := range toolUsageEvidenceNativeProviderFactories() {
		t.Run(factory.name, func(t *testing.T) {
			payloads := [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-created-same","model":"native-created-model","service_tier":"priority","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"low"}]}}`),
				[]byte(`{"type":"response.output_item.done","response_id":"resp-i048-created-same","event_id":"evt-i048-created-first","output_index":0,"item":{"id":"search-i048-created","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-created-same","model":"native-created-model","service_tier":"priority","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"low"}]}}`),
				[]byte(`{"type":"response.output_item.done","response_id":"resp-i048-created-same","event_id":"evt-i048-created-duplicate","output_index":0,"item":{"id":"search-i048-created","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
			}
			harness := newToolUsageEvidenceNativeRelayHarness(t, factory.factory, payloads, true)
			toolUsageEvidenceWaitNativeRelayDone(t, harness)
			key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "low")
			toolUsageEvidenceAssertNativeSearchSettlement(t, harness.attempt, key, 5000)
		})
	}
}

func TestNativeResponsesWSNewCreatedResetsItemTracker(t *testing.T) {
	for _, factory := range toolUsageEvidenceNativeProviderFactories() {
		t.Run(factory.name, func(t *testing.T) {
			payloads := [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-created-one","model":"native-created-model","service_tier":"priority","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"low"}]}}`),
				[]byte(`{"type":"response.output_item.done","response_id":"resp-i048-created-one","event_id":"evt-i048-created-one","output_index":0,"item":{"id":"search-i048-reused","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-created-two","model":"native-created-model","service_tier":"priority","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"low"}]}}`),
				[]byte(`{"type":"response.output_item.done","response_id":"resp-i048-created-two","event_id":"evt-i048-created-two","output_index":0,"item":{"id":"search-i048-reused","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
			}
			usage := toolUsageEvidenceCollectNativeProviderUsage(t, factory.factory, payloads)
			key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "low")
			toolUsageEvidenceAssertNativeProviderSearchUsage(t, usage, key, 2)
		})
	}
}

func TestNativeResponsesWSTwoClientTurnsReuseItemIDAndSettleSeparately(t *testing.T) {
	installResponsesWSTestAPILimiter(t, 100)
	factory := toolUsageEvidenceNativeProviderFactories()[0]
	payloads := [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp-i048-turn-one","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"medium"}]}}`),
		[]byte(`{"type":"response.output_item.done","response_id":"resp-i048-turn-one","event_id":"evt-i048-turn-one-search","output_index":0,"item":{"id":"search-i048-two-turn","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
		[]byte(`{"type":"response.completed","sequence_number":2,"response":{"id":"resp-i048-turn-one","status":"completed","output":[{"id":"search-i048-two-turn","type":"web_search_call","status":"completed","action":{"type":"search"}}]}}`),
		[]byte(`{"type":"response.created","response":{"id":"resp-i048-turn-two","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"medium"}]}}`),
		[]byte(`{"type":"response.output_item.done","response_id":"resp-i048-turn-two","event_id":"evt-i048-turn-two-search","output_index":0,"item":{"id":"search-i048-two-turn","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
		[]byte(`{"type":"response.completed","sequence_number":2,"response":{"id":"resp-i048-turn-two","status":"completed","output":[{"id":"search-i048-two-turn","type":"web_search_call","status":"completed","action":{"type":"search"}}]}}`),
	}
	harness := newToolUsageEvidenceNativeRelayHarnessWithClientGateAt(t, factory.factory, payloads, true, []byte(`"event_id":"create-i048-turn-two"`), 3, false)
	selected := readResponsesWSChannelFixture(t)
	selected.Other = responsesWSTestSelfHostedOther
	harness.actor.mutateSnapshot(func(snapshot *ResponsesWSRequestSnapshot) {
		snapshot.Set("responses_ws_selected_channel", &selected)
	})
	toolUsageEvidenceWaitNativeFrame(t, harness, payloads[2])
	secondCreate := []byte(`{"type":"response.create","event_id":"create-i048-turn-two","model":"gpt-5","store":false,"input":[]}`)
	if err := harness.userClient.WriteMessage(wsconn.TextMessage, secondCreate); err != nil {
		t.Fatalf("send second native response.create: %v", err)
	}
	toolUsageEvidenceWaitNativeFrame(t, harness, payloads[5])
	toolUsageEvidenceWaitNativeRelayDone(t, harness)
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Order("id asc").Find(&logs).Error; err != nil {
		t.Fatalf("read two-turn native Responses WS consume logs: %v", err)
	}
	if len(logs) != 2 || logs[0].Quota != 5000 || logs[1].Quota != 5000 {
		t.Fatalf("expected two independent 5000 SQL settlements for reused item ID, logs=%+v", logs)
	}
	if len(harness.actor.observation.works) != 0 {
		t.Fatalf("completed responses retained work observations: %d", len(harness.actor.observation.works))
	}
}

func TestNativeResponsesWSUnknownResponsePreservesWireWithoutChangingKnownOwner(t *testing.T) {
	factory := toolUsageEvidenceNativeProviderFactories()[0]
	payloads := [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp-i048-owner","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"medium"}]}}`),
		[]byte(`{"type":"response.created","response":{"id":"resp-i048-foreign","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"medium"}]}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp-i048-foreign","status":"completed","usage":{"input_tokens":99,"output_tokens":99,"total_tokens":198}}}`),
	}
	harness := newToolUsageEvidenceNativeRelayHarness(t, factory.factory, payloads, false)
	toolUsageEvidenceWaitNativeFrame(t, harness, payloads[2])
	responsesWSContinuationWaitDeliveredEventCompletion(t, harness.actor)
	if harness.actor.closing.closed.Load() {
		t.Fatal("unknown billing identity closed a valid provider connection")
	}
	if harness.attempt.SeenProviderResponseID != "resp-i048-owner" || harness.attempt.Usage.PromptTokens != 0 {
		t.Fatalf("unassociated response replaced or charged the known owner: %+v", harness.attempt)
	}
	harness.userClient.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Code: wsconn.CloseAbnormalClosure, Reason: "client_abort", Err: context.Canceled})
	toolUsageEvidenceWaitNativeRelayDone(t, harness)
	toolUsageEvidenceAssertNativeNoCharge(t, harness.attempt)
}
