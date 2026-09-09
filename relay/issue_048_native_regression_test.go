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

type issue048NativeProviderFactory func(baseURL string) providersBase.ResponsesWSProvider

func issue048NativeProviderFactories() []struct {
	name    string
	factory issue048NativeProviderFactory
} {
	return []struct {
		name    string
		factory issue048NativeProviderFactory
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

func issue048NativeProviderServer(t *testing.T, payloads [][]byte, closeAfter bool) (string, <-chan error, func()) {
	return issue048NativeProviderServerWithClientGate(t, payloads, closeAfter, nil)
}

func issue048NativeProviderServerWithClientGate(t *testing.T, payloads [][]byte, closeAfter bool, clientGate []byte) (string, <-chan error, func()) {
	return issue048NativeProviderServerWithClientGateAt(t, payloads, closeAfter, clientGate, 2)
}

func issue048NativeProviderServerWithClientGateAt(t *testing.T, payloads [][]byte, closeAfter bool, clientGate []byte, clientGateIndex int) (string, <-chan error, func()) {
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

type issue048NativeRelayHarness struct {
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

func newIssue048NativeRelayHarness(t *testing.T, providerFactory issue048NativeProviderFactory, payloads [][]byte, closeAfter bool) *issue048NativeRelayHarness {
	return newIssue048NativeRelayHarnessWithClientGate(t, providerFactory, payloads, closeAfter, nil, false)
}

func newIssue048NativeRelayHarnessWithClientGate(t *testing.T, providerFactory issue048NativeProviderFactory, payloads [][]byte, closeAfter bool, clientGate []byte, multiAgentEnabled bool) *issue048NativeRelayHarness {
	return newIssue048NativeRelayHarnessWithClientGateAt(t, providerFactory, payloads, closeAfter, clientGate, 2, multiAgentEnabled)
}

func newIssue048NativeRelayHarnessWithClientGateAt(t *testing.T, providerFactory issue048NativeProviderFactory, payloads [][]byte, closeAfter bool, clientGate []byte, clientGateIndex int, multiAgentEnabled bool) *issue048NativeRelayHarness {
	t.Helper()
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 100000, "attempt-"+t.Name())
	providerURL, providerDone, providerStop := issue048NativeProviderServerWithClientGateAt(t, payloads, closeAfter, clientGate, clientGateIndex)
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
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	harness := &issue048NativeRelayHarness{
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

func issue048WaitNativeFrame(t *testing.T, harness *issue048NativeRelayHarness, want []byte) {
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

func issue048WaitNativeRelayDone(t *testing.T, harness *issue048NativeRelayHarness) {
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

func issue048CollectNativeProviderUsage(t *testing.T, providerFactory issue048NativeProviderFactory, payloads [][]byte) *types.Usage {
	t.Helper()
	providerURL, _, providerStop := issue048NativeProviderServer(t, payloads, false)
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

func issue048AssertNativeProviderSearchUsage(t *testing.T, usage *types.Usage, key string, wantCount int) {
	t.Helper()
	if usage == nil {
		t.Fatal("native provider usage is nil")
	}
	billing := usage.ExtraBilling[key]
	if billing.CallCount != wantCount || !usage.HasProviderExtraBilling(key) {
		t.Fatalf("native provider search evidence mismatch: usage=%+v key=%q want_count=%d", usage, key, wantCount)
	}
}

func issue048AssertNativeSearchSettlement(t *testing.T, attempt *ResponsesWSTurnAttempt, key string, wantCharge int64) {
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

func issue048AssertNativeNoCharge(t *testing.T, attempt *ResponsesWSTurnAttempt) {
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

func TestIssue048NativeResponsesWSSearchDoneReachesSQLAcrossFactories(t *testing.T) {
	for _, search := range []struct {
		name    string
		service string
	}{
		{name: "preview", service: types.APIToolTypeWebSearchPreview},
		{name: "ga", service: types.APIToolTypeWebSearch},
	} {
		for _, factory := range issue048NativeProviderFactories() {
			t.Run(search.name+"/"+factory.name, func(t *testing.T) {
				payloads := [][]byte{
					[]byte(`{"type":"response.created","response":{"id":"resp-i048","model":"native-actual","service_tier":"priority","status":"in_progress","tools":[{"type":"` + search.service + `","search_context_size":"medium"}]}}`),
					[]byte(`{"type":"response.output_item.done","event_id":"evt-i048-search","output_index":0,"item":{"id":"search-i048","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
					[]byte(`{"type":"response.output_item.done","event_id":"evt-i048-search-duplicate","output_index":0,"item":{"id":"search-i048","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
				}
				harness := newIssue048NativeRelayHarness(t, factory.factory, payloads, true)
				issue048WaitNativeRelayDone(t, harness)
				key := types.BuildExtraBillingKey(search.service, "medium")
				issue048AssertNativeSearchSettlement(t, harness.attempt, key, 5000)
				if harness.attempt.Usage.ResponseModel != "native-actual" || harness.attempt.Usage.ServiceTier != "priority" {
					t.Fatalf("native search attribution was lost: %+v", harness.attempt.Usage)
				}
				harness.actor.close("duplicate_close")
				issue048AssertNativeSearchSettlement(t, harness.attempt, key, 5000)
			})
		}
	}
}

func TestIssue048NativeResponsesWSSearchTerminalUsageDoesNotDoubleCharge(t *testing.T) {
	for _, factory := range issue048NativeProviderFactories() {
		t.Run(factory.name, func(t *testing.T) {
			payloads := [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-token","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"medium"}]}}`),
				[]byte(`{"type":"response.output_item.done","event_id":"evt-i048-token-search","output_index":0,"item":{"id":"search-i048-token","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
				[]byte(`{"type":"response.completed","sequence_number":2,"response":{"id":"resp-i048-token","status":"completed","usage":{"input_tokens":30,"output_tokens":2,"total_tokens":32},"tools":[{"type":"web_search_preview","search_context_size":"medium"}],"output":[{"id":"search-i048-token","type":"web_search_call","status":"completed","action":{"type":"search"}}]}}`),
			}
			harness := newIssue048NativeRelayHarness(t, factory.factory, payloads, true)
			issue048WaitNativeRelayDone(t, harness)
			key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
			issue048AssertNativeSearchSettlement(t, harness.attempt, key, 5032)
			if harness.attempt.TerminalUsage == nil || harness.attempt.TerminalUsage.PromptTokens != 30 || harness.attempt.TerminalUsage.CompletionTokens != 2 || harness.attempt.TerminalUsage.TotalTokens != 32 {
				t.Fatalf("native terminal token snapshot was not preserved: %+v", harness.attempt.TerminalUsage)
			}
		})
	}
}

func TestIssue048NativeResponsesWSImageTerminalWithoutTokensUsesOneUnit(t *testing.T) {
	for _, factory := range issue048NativeProviderFactories() {
		t.Run(factory.name, func(t *testing.T) {
			payloads := [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-image","status":"in_progress","tools":[{"type":"image_generation","model":"gpt-image-1-mini","quality":"medium","size":"1024x1024"}]}}`),
				[]byte(`{"type":"response.output_item.done","event_id":"evt-i048-image","output_index":0,"item":{"id":"image-i048","type":"image_generation_call","status":"completed","quality":"medium","size":"1024x1024"}}`),
				[]byte(`{"type":"response.completed","sequence_number":2,"response":{"id":"resp-i048-image","status":"completed","tools":[{"type":"image_generation","model":"gpt-image-1-mini","quality":"medium","size":"1024x1024"}],"output":[{"id":"image-i048","type":"image_generation_call","status":"completed","quality":"medium","size":"1024x1024"}]}}`),
			}
			harness := newIssue048NativeRelayHarness(t, factory.factory, payloads, true)
			issue048WaitNativeRelayDone(t, harness)
			key := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-1-mini|medium|1024x1024|0")
			issue048AssertNativeSearchSettlement(t, harness.attempt, key, 5500)
		})
	}
}

func TestIssue048NativeResponsesWSUnknownToolAndDuplicateCloseStayUnbilled(t *testing.T) {
	for _, factory := range issue048NativeProviderFactories() {
		t.Run(factory.name, func(t *testing.T) {
			payloads := [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-unknown","status":"in_progress","tools":[{"type":"future_hosted_tool"}]}}`),
				[]byte(`{"type":"response.output_item.done","event_id":"evt-i048-unknown","output_index":0,"item":{"id":"unknown-i048","type":"future_hosted_call","status":"completed"}}`),
			}
			harness := newIssue048NativeRelayHarness(t, factory.factory, payloads, true)
			issue048WaitNativeRelayDone(t, harness)
			issue048AssertNativeNoCharge(t, harness.attempt)
			// close() is the actor's idempotent terminal path. A second close must
			// not reopen the attempt or create another SQL action.
			harness.actor.close("duplicate_close")
			issue048AssertNativeNoCharge(t, harness.attempt)
		})
	}
}

func TestIssue048NativeResponsesWSDeclaredSearchWithoutDoneStaysUnbilled(t *testing.T) {
	for _, factory := range issue048NativeProviderFactories() {
		t.Run(factory.name, func(t *testing.T) {
			payloads := [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-declared-only","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"medium"}]}}`),
			}
			harness := newIssue048NativeRelayHarness(t, factory.factory, payloads, true)
			issue048WaitNativeRelayDone(t, harness)
			issue048AssertNativeNoCharge(t, harness.attempt)
		})
	}
}

func TestIssue048NativeResponsesWSClientAbortKeepsAcceptedSearch(t *testing.T) {
	for _, factory := range issue048NativeProviderFactories() {
		t.Run(factory.name, func(t *testing.T) {
			payloads := [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-abort","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"medium"}]}}`),
				[]byte(`{"type":"response.output_item.done","event_id":"evt-i048-abort","output_index":0,"item":{"id":"search-i048-abort","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
			}
			harness := newIssue048NativeRelayHarness(t, factory.factory, payloads, false)
			key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
			issue048WaitNativeFrame(t, harness, payloads[1])
			harness.userClient.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Code: wsconn.CloseAbnormalClosure, Reason: "client_abort", Err: context.Canceled})
			issue048WaitNativeRelayDone(t, harness)
			issue048AssertNativeSearchSettlement(t, harness.attempt, key, 5000)
		})
	}
}

func TestIssue048NativeResponsesWSInjectKeepsSearchOwnerAndDeduplicates(t *testing.T) {
	for _, factory := range issue048NativeProviderFactories() {
		t.Run(factory.name, func(t *testing.T) {
			payloads := [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-inject","model":"native-inject-model","service_tier":"priority","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"low"}]}}`),
				[]byte(`{"type":"response.output_item.done","event_id":"evt-i048-inject-search","output_index":0,"item":{"id":"search-i048-inject","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
				[]byte(`{"type":"response.output_item.done","event_id":"evt-i048-inject-duplicate","output_index":0,"item":{"id":"search-i048-inject","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
			}
			inject := []byte(`{"type":"response.inject","event_id":"evt-i048-inject-client","response_id":"resp-i048-inject","input":[{"type":"function_call_output","call_id":"call-i048-inject","output":"ok"}]}`)
			harness := newIssue048NativeRelayHarnessWithClientGate(t, factory.factory, payloads, true, []byte(`"type":"response.inject"`), true)
			issue048WaitNativeFrame(t, harness, payloads[1])
			if err := harness.userClient.WriteMessage(wsconn.TextMessage, inject); err != nil {
				t.Fatalf("send native response.inject: %v", err)
			}
			issue048WaitNativeRelayDone(t, harness)
			key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "low")
			issue048AssertNativeSearchSettlement(t, harness.attempt, key, 5000)
			if harness.attempt.SeenProviderResponseID != "resp-i048-inject" || harness.attempt.Usage.ResponseModel != "native-inject-model" || harness.attempt.Usage.ServiceTier != "priority" {
				t.Fatalf("inject changed current response attribution: %+v", harness.attempt.Usage)
			}
		})
	}
}

func TestIssue048NativeResponsesWSRepeatedCreatedKeepsOwnerTracker(t *testing.T) {
	for _, factory := range issue048NativeProviderFactories() {
		t.Run(factory.name, func(t *testing.T) {
			payloads := [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-created-same","model":"native-created-model","service_tier":"priority","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"low"}]}}`),
				[]byte(`{"type":"response.output_item.done","event_id":"evt-i048-created-first","output_index":0,"item":{"id":"search-i048-created","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-created-same","model":"native-created-model","service_tier":"priority","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"low"}]}}`),
				[]byte(`{"type":"response.output_item.done","event_id":"evt-i048-created-duplicate","output_index":0,"item":{"id":"search-i048-created","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
			}
			harness := newIssue048NativeRelayHarness(t, factory.factory, payloads, true)
			issue048WaitNativeRelayDone(t, harness)
			key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "low")
			issue048AssertNativeSearchSettlement(t, harness.attempt, key, 5000)
		})
	}
}

func TestIssue048NativeResponsesWSNewCreatedResetsItemTracker(t *testing.T) {
	for _, factory := range issue048NativeProviderFactories() {
		t.Run(factory.name, func(t *testing.T) {
			payloads := [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-created-one","model":"native-created-model","service_tier":"priority","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"low"}]}}`),
				[]byte(`{"type":"response.output_item.done","event_id":"evt-i048-created-one","output_index":0,"item":{"id":"search-i048-reused","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
				[]byte(`{"type":"response.created","response":{"id":"resp-i048-created-two","model":"native-created-model","service_tier":"priority","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"low"}]}}`),
				[]byte(`{"type":"response.output_item.done","event_id":"evt-i048-created-two","output_index":0,"item":{"id":"search-i048-reused","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
			}
			usage := issue048CollectNativeProviderUsage(t, factory.factory, payloads)
			key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "low")
			issue048AssertNativeProviderSearchUsage(t, usage, key, 2)
		})
	}
}

func TestIssue048NativeResponsesWSTwoClientTurnsReuseItemIDAndSettleSeparately(t *testing.T) {
	installResponsesWSTestAPILimiter(t, 100)
	factory := issue048NativeProviderFactories()[0]
	payloads := [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp-i048-turn-one","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"medium"}]}}`),
		[]byte(`{"type":"response.output_item.done","event_id":"evt-i048-turn-one-search","output_index":0,"item":{"id":"search-i048-two-turn","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
		[]byte(`{"type":"response.completed","sequence_number":2,"response":{"id":"resp-i048-turn-one","status":"completed","output":[{"id":"search-i048-two-turn","type":"web_search_call","status":"completed","action":{"type":"search"}}]}}`),
		[]byte(`{"type":"response.created","response":{"id":"resp-i048-turn-two","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"medium"}]}}`),
		[]byte(`{"type":"response.output_item.done","event_id":"evt-i048-turn-two-search","output_index":0,"item":{"id":"search-i048-two-turn","type":"web_search_call","status":"completed","action":{"type":"search"}}}`),
		[]byte(`{"type":"response.completed","sequence_number":2,"response":{"id":"resp-i048-turn-two","status":"completed","output":[{"id":"search-i048-two-turn","type":"web_search_call","status":"completed","action":{"type":"search"}}]}}`),
	}
	harness := newIssue048NativeRelayHarnessWithClientGateAt(t, factory.factory, payloads, true, []byte(`"event_id":"create-i048-turn-two"`), 3, false)
	selected := readResponsesWSChannelFixture(t)
	selected.Other = responsesWSTestSelfHostedOther
	harness.actor.mutateSnapshot(func(snapshot *ResponsesWSRequestSnapshot) {
		snapshot.Set("responses_ws_selected_channel", &selected)
	})
	issue048WaitNativeFrame(t, harness, payloads[2])
	secondCreate := []byte(`{"type":"response.create","event_id":"create-i048-turn-two","model":"gpt-5","store":false,"input":[]}`)
	if err := harness.userClient.WriteMessage(wsconn.TextMessage, secondCreate); err != nil {
		t.Fatalf("send second native response.create: %v", err)
	}
	issue048WaitNativeFrame(t, harness, payloads[5])
	issue048WaitNativeRelayDone(t, harness)
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Order("id asc").Find(&logs).Error; err != nil {
		t.Fatalf("read two-turn native Responses WS consume logs: %v", err)
	}
	if len(logs) != 2 || logs[0].Quota != 5000 || logs[1].Quota != 5000 {
		t.Fatalf("expected two independent 5000 SQL settlements for reused item ID, logs=%+v", logs)
	}
	if harness.actor.turns.history.lastFinal == nil || harness.actor.turns.history.lastFinal.ID != "resp-i048-turn-two" {
		t.Fatalf("second native response did not become the final turn: %+v", harness.actor.turns.history.lastFinal)
	}
}

func TestIssue048NativeResponsesWSForeignResponseIDIsRejectedByActiveAttempt(t *testing.T) {
	factory := issue048NativeProviderFactories()[0]
	payloads := [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp-i048-owner","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"medium"}]}}`),
		[]byte(`{"type":"response.created","response":{"id":"resp-i048-foreign","status":"in_progress","tools":[{"type":"web_search_preview","search_context_size":"medium"}]}}`),
	}
	harness := newIssue048NativeRelayHarness(t, factory.factory, payloads, true)
	issue048WaitNativeRelayDone(t, harness)
	if !harness.actor.closing.closed.Load() {
		t.Fatal("foreign provider response ID did not close the active Responses WS turn")
	}
	if harness.attempt.SeenProviderResponseID != "resp-i048-owner" {
		t.Fatalf("foreign response ID replaced the active owner: %q", harness.attempt.SeenProviderResponseID)
	}
	issue048AssertNativeNoCharge(t, harness.attempt)
}
