package relay

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
	"one-api/model"
	"one-api/types"
)

type issue004RelayNativeAdapter struct{}

func (issue004RelayNativeAdapter) PrepareClientFrame(_ context.Context, frame responsesws.Frame) (responsesws.Frame, error) {
	return frame, nil
}

func (issue004RelayNativeAdapter) HandleProviderFrame(_ context.Context, frame responsesws.Frame) responsesws.ProviderFrameResult {
	result := responsesws.ProviderFrameResult{
		EmitFrame: &frame,
		Origin:    responsesws.RecvDetailOriginProviderFrame,
	}
	if classified := responsesws.ClassifyResponsesWSEvent(frame.Payload()); classified.Kind != responsesws.ResponsesNonTerminal {
		result.Usage = &types.UsageEvent{
			InputTokens:           4,
			OutputTokens:          2,
			TotalTokens:           6,
			Source:                types.UsageSourceResponsesResponse,
			BillingBasis:          types.UsageBillingBasisTokens,
			ProviderTokenEvidence: true,
		}
	}
	return result
}

func (issue004RelayNativeAdapter) MapProviderClose(_ context.Context, info responsesws.ProviderCloseInfo) responsesws.ProviderCloseResult {
	return responsesws.ProviderCloseResult{
		ProviderClose: &responsesws.ProviderClose{Code: info.Code, Reason: info.Reason, Err: info.Err},
		Origin:        responsesws.RecvDetailOriginNativeProviderClose,
	}
}

type issue004RelayWireFrame struct {
	messageType wsconn.MessageType
	payload     []byte
}

type issue004RelayHarness struct {
	actor          *ResponsesWSSessionActor
	attempt        *ResponsesWSTurnAttempt
	native         *responsesws.NativeSession
	generation     string
	providerServer *wsconn.ManagedConn
	providerClient *wsconn.ManagedConn
	userServer     *wsconn.ManagedConn
	userClient     *wsconn.ManagedConn
	pump           *ResponsesWSIOPump
	nativeEnqueued chan responsesws.UpstreamEvent
	frames         chan issue004RelayWireFrame
	closeInfos     chan wsconn.CloseInfo
	userPumpDone   chan struct{}
}

func newIssue004RelayHarness(t *testing.T, attemptID string, multiAgent bool) *issue004RelayHarness {
	t.Helper()
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, attemptID)
	attempt.MultiAgentEnabled = multiAgent

	userClient, userServer := wstest.Pair(t)
	providerClient, providerServer := wstest.Pair(t)
	adapter := &issue004RelayNativeAdapter{}
	nativeEnqueued := make(chan responsesws.UpstreamEvent, 4)
	native := responsesws.NewNativeSession(providerClient, adapter, responsesws.NativeSessionOptions{
		RecvQueueSize: 2,
		EventEnqueued: func(event responsesws.UpstreamEvent) {
			if event.Frame == nil && event.ProviderClose == nil {
				return
			}
			select {
			case nativeEnqueued <- event:
			default:
			}
		},
	})
	initial := responsesws.NewTextFrame([]byte(`{"type":"response.create","model":"gpt-5","input":[]}`))
	result := native.SendClientWithResult(context.Background(), responsesws.SendRequest{AttemptID: attempt.AttemptID, Frame: initial})
	if result.Status != responsesws.ResponsesWSTransportSendAttempted || result.Err != nil {
		t.Fatalf("seed response.create did not reach native provider transport: %+v", result)
	}
	ctxCanceled, cancel := context.WithCancel(context.Background())
	cancel()
	// 取消的这次 Recv 只启动 NativeSession 的真实 read pump，不消费队列；
	// 后续由入队观察点确认两个 provider 事件都已积压后，才启动 relay 消费者。
	if _, err := native.Recv(ctxCanceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled Recv to start the native read pump without consuming events, got %v", err)
	}

	actor := NewResponsesWSSessionActor(ctx)
	pump := NewResponsesWSManagedPump(userServer, actor)
	actor.SetPump(pump)
	actor.SetClientConn(userServer)
	generation := actor.AttachUpstreamSession(native, 17)
	attempt.Session = native
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight

	harness := &issue004RelayHarness{
		actor:          actor,
		attempt:        attempt,
		native:         native,
		generation:     generation,
		providerServer: providerServer,
		providerClient: providerClient,
		userServer:     userServer,
		userClient:     userClient,
		pump:           pump,
		nativeEnqueued: nativeEnqueued,
		frames:         make(chan issue004RelayWireFrame, 8),
		closeInfos:     make(chan wsconn.CloseInfo, 1),
		userPumpDone:   make(chan struct{}),
	}
	go func() {
		defer close(harness.userPumpDone)
		wsconn.Pump{
			Conn: userClient,
			Handle: func(_ context.Context, messageType wsconn.MessageType, payload []byte) {
				harness.frames <- issue004RelayWireFrame{messageType: messageType, payload: append([]byte(nil), payload...)}
			},
			OnClose: func(info wsconn.CloseInfo) {
				select {
				case harness.closeInfos <- info:
				default:
				}
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
			t.Errorf("timed out waiting for Responses WS actor cleanup")
		}
		actor.waitStartedGoroutines()
		pump.Close()
		providerServer.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
		providerClient.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
		userServer.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
		userClient.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_cleanup"})
		select {
		case <-harness.userPumpDone:
		case <-time.After(time.Second):
			t.Errorf("timed out waiting for downstream client pump cleanup")
		}
	})
	return harness
}

func TestIssue004RelayNativeCompletedThenCloseDeliversAndSettlesOnce(t *testing.T) {
	harness := newIssue004RelayHarness(t, "attempt-issue-004-close", false)
	completed := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp-issue-004-close","status":"completed","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`)
	if err := harness.providerServer.WriteMessage(wsconn.TextMessage, completed); err != nil {
		t.Fatalf("write completed provider event: %v", err)
	}
	harness.providerServer.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindGracefulShutdown, Code: wsconn.CloseNormalClosure, Reason: "provider_done"})
	enqueued := issue004WaitNativeEvents(t, harness, 2)
	if enqueued[0].Frame == nil || string(enqueued[0].Frame.Payload()) != string(completed) ||
		enqueued[1].ProviderClose == nil || enqueued[1].ProviderClose.Code != int(wsconn.CloseNormalClosure) {
		t.Fatalf("native read pump did not enqueue completed then provider close: %+v", enqueued)
	}
	harness.actor.Start()
	harness.pump.ArmProviderRecvPump(harness.generation, 17, harness.native)

	frame := issue004NextRelayFrame(t, harness)
	if frame.messageType != wsconn.TextMessage || string(frame.payload) != string(completed) {
		t.Fatalf("provider close crossed completed frame at client boundary: %+v", frame)
	}
	select {
	case <-harness.actor.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for provider close to finish actor")
	}
	if !harness.attempt.QuotaFinalized || harness.attempt.RolledBack || harness.attempt.AppliedSettlement == nil {
		t.Fatalf("completed provider event did not settle active attempt: %+v", harness.attempt)
	}
	issue004AssertRelaySettlement(t, harness.attempt)
	select {
	case info := <-harness.closeInfos:
		if info.Code != wsconn.CloseNormalClosure {
			t.Fatalf("downstream close code=%d, want %d", info.Code, wsconn.CloseNormalClosure)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for downstream close after completed frame")
	}
}

func TestIssue004RelayNativeCompletedThenInjectAckKeepsSequenceAndSettlesOnce(t *testing.T) {
	harness := newIssue004RelayHarness(t, "attempt-issue-004-ack", true)
	completed := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp-issue-004-ack","status":"completed","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`)
	ack := []byte(`{"type":"response.inject.created","sequence_number":2,"response_id":"resp-issue-004-ack"}`)
	if err := harness.providerServer.WriteMessage(wsconn.TextMessage, completed); err != nil {
		t.Fatalf("write completed provider event: %v", err)
	}
	if err := harness.providerServer.WriteMessage(wsconn.TextMessage, ack); err != nil {
		t.Fatalf("write inject acknowledgement provider event: %v", err)
	}
	enqueued := issue004WaitNativeEvents(t, harness, 2)
	if enqueued[0].Frame == nil || string(enqueued[0].Frame.Payload()) != string(completed) ||
		enqueued[1].Frame == nil || string(enqueued[1].Frame.Payload()) != string(ack) {
		t.Fatalf("native read pump did not enqueue completed then inject acknowledgement: %+v", enqueued)
	}
	harness.actor.Start()
	harness.pump.ArmProviderRecvPump(harness.generation, 17, harness.native)

	first := issue004NextRelayFrame(t, harness)
	second := issue004NextRelayFrame(t, harness)
	if string(first.payload) != string(completed) || string(second.payload) != string(ack) {
		t.Fatalf("completed/inject acknowledgement order changed at client boundary: first=%q second=%q", first.payload, second.payload)
	}
	issue004WaitRelayActorEvents(t, harness.actor)
	if harness.actor.closing.closed.Load() || harness.actor.turns.active.attempt != nil {
		t.Fatalf("inject acknowledgement after terminal incorrectly closed or remained pending: closed=%v pending=%d", harness.actor.closing.closed.Load(), harness.actor.state)
	}
	if harness.attempt.QuotaFinalized == false || harness.attempt.RolledBack || harness.attempt.AppliedSettlement == nil {
		t.Fatalf("completed provider event did not settle exactly once before acknowledgement: %+v", harness.attempt)
	}
	issue004AssertRelaySettlement(t, harness.attempt)
}

func issue004WaitNativeEvents(t *testing.T, harness *issue004RelayHarness, want int) []responsesws.UpstreamEvent {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	events := make([]responsesws.UpstreamEvent, 0, want)
	for len(events) < want {
		select {
		case event := <-harness.nativeEnqueued:
			events = append(events, event)
		case <-deadline.C:
			t.Fatalf("timed out waiting for native read pump events: got=%d want=%d", len(events), want)
		}
	}
	return events
}

func issue004WaitRelayActorEvents(t *testing.T, actor *ResponsesWSSessionActor) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for actor.eventBytes.Load() != 0 {
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for relay actor to consume queued provider events")
		default:
			runtime.Gosched()
		}
	}
}

func issue004NextRelayFrame(t *testing.T, harness *issue004RelayHarness) issue004RelayWireFrame {
	t.Helper()
	select {
	case frame := <-harness.frames:
		return frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for downstream Responses WS frame")
		return issue004RelayWireFrame{}
	}
}

func issue004AssertRelaySettlement(t *testing.T, attempt *ResponsesWSTurnAttempt) {
	t.Helper()
	if attempt == nil || attempt.AppliedSettlement == nil || attempt.AppliedSettlement.AppliedFinalQuota <= 0 {
		t.Fatalf("expected positive applied settlement, attempt=%+v", attempt)
	}
	charge := int(attempt.AppliedSettlement.AppliedFinalQuota)
	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 1000-charge || user.UsedQuota != charge || token.RemainQuota != 1000-charge || token.UsedQuota != charge {
		t.Fatalf("settlement changed quota more than once or by wrong amount: charge=%d user=%+v token=%+v", charge, user, token)
	}
	var logs []model.Log
	if err := model.DB.Find(&logs).Error; err != nil {
		t.Fatalf("read Responses WS consume logs: %v", err)
	}
	if len(logs) != 1 || logs[0].Quota != charge {
		t.Fatalf("expected one consume log for one terminal settlement: logs=%+v charge=%d", logs, charge)
	}
}
