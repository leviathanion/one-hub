package relay

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/model"
)

type issue039InjectFailedEvent struct {
	Type           string            `json:"type"`
	SequenceNumber int64             `json:"sequence_number"`
	ResponseID     string            `json:"response_id"`
	Input          []json.RawMessage `json:"input"`
	Error          struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func issue039StartManagedReadPump(t *testing.T, conn *wsconn.ManagedConn, handle func([]byte)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		wsconn.Pump{
			Conn: conn,
			Handle: func(_ context.Context, _ wsconn.MessageType, payload []byte) {
				if handle != nil {
					handle(append([]byte(nil), payload...))
				}
			},
		}.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Errorf("timed out waiting for test websocket read pump cleanup")
		}
	})
}

func issue039NextProviderCreate(t *testing.T, frames <-chan []byte, previousResponseID string) []byte {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case payload := <-frames:
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal(payload, &envelope); err != nil {
				continue
			}
			var eventType, previous string
			_ = json.Unmarshal(envelope["type"], &eventType)
			_ = json.Unmarshal(envelope["previous_response_id"], &previous)
			if eventType == "response.create" && previous == previousResponseID {
				return payload
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for response.create with previous_response_id=%q", previousResponseID)
			return nil
		}
	}
}

func issue039ReadInjectFailed(t *testing.T, frame issue004RelayWireFrame) issue039InjectFailedEvent {
	t.Helper()
	if frame.messageType != wsconn.TextMessage {
		t.Fatalf("expected text response.inject.failed frame, got message type %d", frame.messageType)
	}
	var failed issue039InjectFailedEvent
	if err := json.Unmarshal(frame.payload, &failed); err != nil {
		t.Fatalf("decode response.inject.failed: %v; payload=%s", err, frame.payload)
	}
	return failed
}

func issue039AssertSecondSettlement(t *testing.T, first *ResponsesWSTurnAttempt, firstCharge int64) {
	t.Helper()
	if first == nil || first.AppliedSettlement == nil || first.AppliedSettlement.AppliedFinalQuota != firstCharge {
		t.Fatalf("first response settlement changed after recovery: first=%+v charge=%d", first, firstCharge)
	}
	var logs []model.Log
	if err := model.DB.Order("id asc").Find(&logs).Error; err != nil {
		t.Fatalf("read Responses WS consume logs: %v", err)
	}
	if len(logs) != 2 || logs[0].Quota <= 0 || logs[1].Quota <= 0 {
		t.Fatalf("expected one positive SQL settlement per response turn, logs=%+v", logs)
	}
	total := logs[0].Quota + logs[1].Quota
	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 1000-total || user.UsedQuota != total || token.RemainQuota != 1000-total || token.UsedQuota != total {
		t.Fatalf("expected independent two-turn settlement, total=%d user=%+v token=%+v", total, user, token)
	}
}

func TestIssue039ResponsesWSCompletedInjectRecoversThroughNextTurn(t *testing.T) {
	harness := newIssue004RelayHarness(t, "attempt-issue-039-first", true)
	originalApproximate := config.ApproximateTokenEnabled
	config.ApproximateTokenEnabled = true
	t.Cleanup(func() { config.ApproximateTokenEnabled = originalApproximate })
	installResponsesWSTestAPILimiter(t, 100)
	// The shared I004 harness seeds a pending inject for its terminal-ack test.
	// This scenario starts with no prior inject so the completed response can
	// enter the completed-response recovery path directly.
	harness.actor.turns.inject.Reset()

	providerFrames := make(chan []byte, 8)
	var providerReady sync.Once
	providerSeed := make(chan struct{})
	issue039StartManagedReadPump(t, harness.providerServer, func(payload []byte) {
		select {
		case providerFrames <- payload:
		default:
		}
		providerReady.Do(func() { close(providerSeed) })
	})
	select {
	case <-providerSeed:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the seeded provider request")
	}

	issue039StartManagedReadPump(t, harness.userServer, func(payload []byte) {
		harness.actor.onClientFrame(context.Background(), wsconn.TextMessage, payload)
	})
	harness.actor.mutateSnapshot(func(snapshot *ResponsesWSRequestSnapshot) {
		snapshot.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, Models: "gpt-5"})
	})
	harness.actor.Start()
	harness.pump.ArmProviderRecvPump(harness.generation, 17, harness.native)

	firstCompleted := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp-039-first","status":"completed","output":[{"type":"function_call","id":"fc-039","call_id":"call-039","name":"lookup","arguments":"{}","status":"completed"}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`)
	if err := harness.providerServer.WriteMessage(wsconn.TextMessage, firstCompleted); err != nil {
		t.Fatalf("write first completed provider event: %v", err)
	}
	firstFrame := issue004NextRelayFrame(t, harness)
	if string(firstFrame.payload) != string(firstCompleted) {
		t.Fatalf("first completed frame changed before recovery: got=%s want=%s", firstFrame.payload, firstCompleted)
	}
	issue004WaitRelayActorEvents(t, harness.actor)
	if harness.attempt.AppliedSettlement == nil || !harness.attempt.QuotaFinalized || harness.attempt.RolledBack {
		t.Fatalf("first response did not settle before inject recovery: %+v", harness.attempt)
	}
	firstCharge := harness.attempt.AppliedSettlement.AppliedFinalQuota

	injected := []byte(`{"type":"response.inject","event_id":"inject-039","response_id":"resp-039-first","input":[{"type":"function_call_output","call_id":"call-039","output":"tool result"}]}`)
	if err := harness.userClient.WriteMessage(wsconn.TextMessage, injected); err != nil {
		t.Fatalf("write completed-response inject: %v", err)
	}
	failed := issue039ReadInjectFailed(t, issue004NextRelayFrame(t, harness))
	if failed.Type != "response.inject.failed" || failed.Error.Code != "response_already_completed" ||
		failed.ResponseID != "resp-039-first" || failed.SequenceNumber != 2 || len(failed.Input) != 1 {
		t.Fatalf("completed inject did not produce recoverable protocol event: %+v", failed)
	}
	var failedInput map[string]any
	if err := json.Unmarshal(failed.Input[0], &failedInput); err != nil || failedInput["type"] != "function_call_output" || failedInput["call_id"] != "call-039" {
		t.Fatalf("recovery event lost function_call_output association: input=%s err=%v", failed.Input[0], err)
	}
	if harness.actor.closing.closed.Load() || harness.actor.turns.active.attempt != nil || harness.actor.turns.pending.attempt != nil {
		t.Fatalf("completed inject recovery must keep session reusable without reopening old attempt: closed=%v active=%+v pending=%+v", harness.actor.closing.closed.Load(), harness.actor.turns.active.attempt, harness.actor.turns.pending.attempt)
	}
	if harness.attempt.AppliedSettlement.AppliedFinalQuota != firstCharge {
		t.Fatalf("inject recovery changed first response settlement: before=%d after=%d", firstCharge, harness.attempt.AppliedSettlement.AppliedFinalQuota)
	}

	nextRequest, err := json.Marshal(map[string]any{
		"type":                 "response.create",
		"event_id":             "create-039-next",
		"model":                "gpt-5",
		"store":                false,
		"multi_agent":          map[string]any{"enabled": true},
		"previous_response_id": failed.ResponseID,
		"input":                failed.Input,
	})
	if err != nil {
		t.Fatalf("marshal next response.create: %v", err)
	}
	if err := harness.userClient.WriteMessage(wsconn.TextMessage, nextRequest); err != nil {
		t.Fatalf("write recovered response.create: %v", err)
	}
	sentNext := issue039NextProviderCreate(t, providerFrames, failed.ResponseID)
	var sentNextObject map[string]json.RawMessage
	if err := json.Unmarshal(sentNext, &sentNextObject); err != nil {
		t.Fatalf("decode recovered provider request: %v", err)
	}
	var sentInput []json.RawMessage
	if err := json.Unmarshal(sentNextObject["input"], &sentInput); err != nil || len(sentInput) != 1 || string(sentInput[0]) != string(failed.Input[0]) {
		t.Fatalf("provider next turn did not receive returned recovery input: input=%s err=%v", sentNextObject["input"], err)
	}

	secondCompleted := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp-039-second","status":"completed","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`)
	if err := harness.providerServer.WriteMessage(wsconn.TextMessage, secondCompleted); err != nil {
		t.Fatalf("write second completed provider event: %v", err)
	}
	secondFrame := issue004NextRelayFrame(t, harness)
	if string(secondFrame.payload) != string(secondCompleted) {
		t.Fatalf("second response did not complete through the real relay path: got=%s want=%s", secondFrame.payload, secondCompleted)
	}
	issue004WaitRelayActorEvents(t, harness.actor)
	if harness.actor.closing.closed.Load() || harness.actor.turns.history.lastFinal == nil || harness.actor.turns.history.lastFinal.ID != "resp-039-second" {
		t.Fatalf("second response did not leave a reusable session: closed=%v final=%+v", harness.actor.closing.closed.Load(), harness.actor.turns.history.lastFinal)
	}
	issue039AssertSecondSettlement(t, harness.attempt, firstCharge)
}

func issue039CompletedRecoveryActor(t *testing.T) (*ResponsesWSSessionActor, *responsesWSFakeUserConn, *ResponsesWSTurnAttempt) {
	t.Helper()
	ctx, attempt := setupPreconsumedResponsesWSActorAttempt(t, 1000, "attempt-issue-039-invalid")
	attempt.MultiAgentEnabled = true
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	session := &responsesWSCaptureSendSession{requests: make(chan responsesws.SendRequest, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	generation := actor.AttachUpstreamSession(session, 17)
	attempt.Session = actor.upstream.session
	actor.turns.active.attempt = attempt
	actor.turns.active.affinity = CommitResponsesTurnAffinity(&ResponsesTurnAffinity{}, 17)
	actor.turns.active.channelID = 17
	actor.state = responsesWSStateInFlight
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID:                 attempt.AttemptID,
		UpstreamSessionGeneration: generation,
		ChannelID:                 17,
		Kind:                      ProviderDownstreamFrame,
		Frame:                     responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp-039-owner","status":"completed","output":[{"type":"function_call","call_id":"call-039-owner","name":"lookup","arguments":"{}","status":"completed"}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`)),
		DetailOrigin:              responsesws.RecvDetailOriginProviderFrame,
	})
	issue004WaitRelayActorEvents(t, actor)
	return actor, conn, attempt
}

func TestIssue039CompletedInjectRejectsUnknownResponseAndCall(t *testing.T) {
	actor, conn, _ := issue039CompletedRecoveryActor(t)
	defer actor.finish()
	session := actor.upstream.session.(*responsesWSCaptureSendSession)
	otherOwner, err := model.NewResponseOwner("resp-039-other-owner", 2, 2, 17, time.Now())
	if err != nil {
		t.Fatalf("create cross-user response owner fixture: %v", err)
	}
	if err := model.CreateResponseOwner(context.Background(), otherOwner); err != nil {
		t.Fatalf("persist cross-user response owner fixture: %v", err)
	}
	for _, test := range []struct {
		name    string
		payload string
		code    string
	}{
		{
			name:    "unknown response owner",
			payload: `{"type":"response.inject","response_id":"resp-039-other-owner","input":[{"type":"function_call_output","call_id":"call-039-owner","output":"tool result"}]}`,
			code:    "response_not_found",
		},
		{
			name:    "wrong function call id",
			payload: `{"type":"response.inject","response_id":"resp-039-owner","input":[{"type":"function_call_output","call_id":"call-039-other","output":"tool result"}]}`,
			code:    "invalid_response_inject",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn.lastWrite.Store("")
			actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(test.payload)))
			got, _ := conn.lastWrite.Load().(string)
			if !strings.Contains(got, `"code":"`+test.code+`"`) {
				t.Fatalf("expected completed inject rejection %q, got %q", test.code, got)
			}
			select {
			case request := <-session.requests:
				t.Fatalf("rejected completed inject reached upstream: %+v", request)
			default:
			}
			if actor.closing.closed.Load() || actor.state != responsesWSStateIdle {
				t.Fatalf("rejected completed inject must keep session open, closed=%v state=%v", actor.closing.closed.Load(), actor.state)
			}
		})
	}
}

func TestIssue039RevokedPrincipalRejectsRecoveredNextTurnBeforeProvider(t *testing.T) {
	harness := newIssue004RelayHarness(t, "attempt-issue-039-revoked", true)
	originalApproximate := config.ApproximateTokenEnabled
	config.ApproximateTokenEnabled = true
	t.Cleanup(func() { config.ApproximateTokenEnabled = originalApproximate })
	installResponsesWSTestAPILimiter(t, 100)
	harness.actor.turns.inject.Reset()
	providerFrames := make(chan []byte, 4)
	issue039StartManagedReadPump(t, harness.providerServer, func(payload []byte) {
		select {
		case providerFrames <- payload:
		default:
		}
	})
	issue039StartManagedReadPump(t, harness.userServer, func(payload []byte) {
		harness.actor.onClientFrame(context.Background(), wsconn.TextMessage, payload)
	})
	harness.actor.mutateSnapshot(func(snapshot *ResponsesWSRequestSnapshot) {
		snapshot.Set("responses_ws_selected_channel", &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, Models: "gpt-5"})
	})
	harness.actor.Start()
	harness.pump.ArmProviderRecvPump(harness.generation, 17, harness.native)

	completed := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp-039-revoked","status":"completed","output":[{"type":"function_call","call_id":"call-039-revoked","name":"lookup","arguments":"{}","status":"completed"}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`)
	if err := harness.providerServer.WriteMessage(wsconn.TextMessage, completed); err != nil {
		t.Fatalf("write completed provider event: %v", err)
	}
	if got := issue004NextRelayFrame(t, harness); string(got.payload) != string(completed) {
		t.Fatalf("first completed response changed: got=%s want=%s", got.payload, completed)
	}
	issue004WaitRelayActorEvents(t, harness.actor)
	injected := []byte(`{"type":"response.inject","response_id":"resp-039-revoked","input":[{"type":"function_call_output","call_id":"call-039-revoked","output":"tool result"}]}`)
	if err := harness.userClient.WriteMessage(wsconn.TextMessage, injected); err != nil {
		t.Fatalf("write completed-response inject: %v", err)
	}
	failed := issue039ReadInjectFailed(t, issue004NextRelayFrame(t, harness))
	if failed.Error.Code != "response_already_completed" {
		t.Fatalf("expected completed recovery event before revocation, got %+v", failed)
	}

	// This test uses the same SQL principal snapshot as the real long-lived
	// websocket path. Revoking the token after the first turn must be observed
	// by the next response.create before any second provider frame is sent.
	harness.actor.mutateSnapshot(func(snapshot *ResponsesWSRequestSnapshot) {
		snapshot.Set("long_lived_principal_authenticated", true)
	})
	if err := model.DB.Model(&model.Token{}).Where("id = ?", 1).Update("status", config.TokenStatusDisabled).Error; err != nil {
		t.Fatalf("revoke test token: %v", err)
	}
	nextRequest, err := json.Marshal(map[string]any{
		"type":                 "response.create",
		"event_id":             "create-039-revoked",
		"model":                "gpt-5",
		"store":                false,
		"multi_agent":          map[string]any{"enabled": true},
		"previous_response_id": failed.ResponseID,
		"input":                failed.Input,
	})
	if err != nil {
		t.Fatalf("marshal revoked next response.create: %v", err)
	}
	if err := harness.userClient.WriteMessage(wsconn.TextMessage, nextRequest); err != nil {
		t.Fatalf("write revoked next response.create: %v", err)
	}
	issue004WaitRelayActorEvents(t, harness.actor)
	frame := issue004NextRelayFrame(t, harness)
	if !strings.Contains(string(frame.payload), `"code":"invalid_api_key"`) {
		t.Fatalf("revoked principal did not fail prework with invalid_api_key: %s", frame.payload)
	}
	if harness.actor.turns.pending.attempt != nil || harness.actor.turns.active.attempt != nil {
		t.Fatalf("revoked next turn created provider work before principal revalidation: pending=%+v active=%+v", harness.actor.turns.pending.attempt, harness.actor.turns.active.attempt)
	}
	select {
	case payload := <-providerFrames:
		var object map[string]json.RawMessage
		if json.Unmarshal(payload, &object) == nil {
			var previous string
			_ = json.Unmarshal(object["previous_response_id"], &previous)
			if previous == failed.ResponseID {
				t.Fatalf("revoked next turn reached provider: %s", payload)
			}
		}
	default:
	}
}
