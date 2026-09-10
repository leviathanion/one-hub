package relay

import (
	"errors"
	"strings"
	"testing"
	"time"

	"one-api/common/responsesws"
)

func pendingSteeringBatchForTest(t *testing.T) (*ResponsesWSSessionActor, *responsesWSCaptureSendSession, *responsesWSFakeUserConn) {
	t.Helper()
	a, session, conn := newSteeringTestActor(t, 1000)
	sendTestSteer(t, a, session, "resp_parent")
	sendTestSteer(t, a, session, "resp_parent")
	steeringProviderFrame(a, `{"type":"response.steer.accepted","sequence_number":1,"steer":{"id":"s1","previous_response_id":"resp_parent"}}`)
	steeringProviderFrame(a, `{"type":"response.steer.accepted","sequence_number":2,"steer":{"id":"s2","previous_response_id":"resp_parent"}}`)
	steeringProviderFrame(a, `{"type":"response.completed","sequence_number":3,"response":{"id":"resp_parent","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`)
	steeringProviderFrame(a, `{"type":"response.steer.pending","sequence_number":4,"steer":{"id":"s1","previous_response_id":"resp_parent"},"reason":"waiting_for_required_input","required_input":[{"type":"function_call_output","call_id":"call_1"}]}`)
	return a, session, conn
}

func startExplicitSteeringContinuationForTest(t *testing.T, a *ResponsesWSSessionActor, session *responsesWSCaptureSendSession) *ResponsesWSTurnAttempt {
	t.Helper()
	a.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.create","model":"gpt-5","store":false,"previous_response_id":"resp_parent","input":[{"type":"function_call_output","call_id":"call_1","output":"saved result"}]}`)))
	next := a.turns.pending.attempt
	if next == nil {
		t.Fatal("explicit continuation was not prepared")
	}
	select {
	case req := <-session.requests:
		if req.AttemptID != next.AttemptID {
			t.Fatal("explicit continuation used wrong transport identity")
		}
	case <-time.After(time.Second):
		t.Fatal("explicit continuation was not sent")
	}
	return next
}

func TestResponsesWSSteeringLateControlPreservesWireWithoutChangingNewAttempt(t *testing.T) {
	for _, phase := range []string{"pending", "active"} {
		for _, eventType := range []string{"pending", "failed"} {
			t.Run(phase+"/"+eventType, func(t *testing.T) {
				a, session, conn := pendingSteeringBatchForTest(t)
				next := startExplicitSteeringContinuationForTest(t, a, session)
				if phase == "active" {
					a.handleSendResult(ResponsesWSEventSendResult{AttemptID: next.AttemptID, SelectedChannelID: 17, UpstreamSessionGeneration: a.upstream.sessionGeneration, Purpose: ResponsesWSSendPurposeResponseCreate, TransportResult: responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAttempted}})
				}
				payload := `{"type":"response.steer.` + eventType + `","sequence_number":5,"steer":{"id":"s2","previous_response_id":"resp_parent","input":"user correction"},"error":{"code":"successor_creation_failed","message":"failed"},"future":{"kept":true}}`
				steeringProviderFrame(a, payload)
				if got, _ := conn.lastWrite.Load().(string); got != payload {
					t.Fatalf("late receipt/input lost: %s", got)
				}
				if a.closing.closed.Load() || next.DownstreamCommitted || next.SeenProviderResponseID != "" || next.Usage.PromptTokens != 0 || next.QuotaFinalized || next.RolledBack {
					t.Fatal("old receipt changed the new attempt's lifecycle or settlement")
				}
			})
		}
	}
}

func TestResponsesWSSteeringLatePolicyStopClosesNewTurn(t *testing.T) {
	for _, oldGeneration := range []bool{false, true} {
		t.Run(map[bool]string{false: "same_connection", true: "old_connection"}[oldGeneration], func(t *testing.T) {
			a, session, conn := pendingSteeringBatchForTest(t)
			next := startExplicitSteeringContinuationForTest(t, a, session)
			payload := `{"type":"error","error":{"code":"misalignment_policy_violation","message":"must stop"}}`
			generation := a.upstream.sessionGeneration
			if oldGeneration {
				generation = "retired-generation"
			}
			a.handleProviderDownstream(ResponsesWSEventProviderDownstream{AttemptID: "transport-parent", UpstreamSessionGeneration: generation, ChannelID: 17, Kind: ProviderDownstreamFrame, Frame: responsesWSTestProviderTextFrame([]byte(payload)), DetailOrigin: responsesws.RecvDetailOriginProviderFrame})
			got, _ := conn.lastWrite.Load().(string)
			if oldGeneration {
				if a.closing.closed.Load() || strings.Contains(got, "misalignment_policy_violation") {
					t.Fatal("retired connection affected current work")
				}
			} else if !a.closing.closed.Load() || got != payload || !next.RolledBack {
				t.Fatalf("policy stop not delivered/settled: closed=%v rolled_back=%v payload=%s", a.closing.closed.Load(), next.RolledBack, got)
			}
		})
	}
}

func TestResponsesWSSteeringSendResultUsesBatchIdentity(t *testing.T) {
	a, session, _ := newSteeringTestActor(t, 1000)
	sendTestSteer(t, a, session, "resp_parent")
	event, ok := readResponsesWSEvent(t, a).(ResponsesWSEventSendResult)
	if !ok || event.AttemptID != a.steering.next.AttemptID || event.ResponseID != "resp_parent" || event.Purpose != ResponsesWSSendPurposeResponseSteer {
		t.Fatalf("steer result did not identify its batch: %+v", event)
	}
	a.handleSendResult(event)
	if a.closing.closed.Load() {
		t.Fatal("successful send closed session")
	}
	oldBatch := event.AttemptID
	steeringProviderFrame(a, `{"type":"response.steer.failed","sequence_number":1,"steer":{"previous_response_id":"resp_parent"},"error":{"code":"invalid_input","message":"rejected"}}`)
	sendTestSteer(t, a, session, "resp_parent")
	next := a.steering.next
	if next.AttemptID == oldBatch {
		t.Fatal("steering reused a batch ID")
	}
	// 同一连接、同一目标 response 的下一批也不能接收旧批次失败。
	event.TransportResult = responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAmbiguous, Err: errors.New("late write result")}
	a.handleSendResult(event)
	a.handleTransportContractViolation(ResponsesWSEventTransportContractViolation{Completion: event.Completion, AttemptID: oldBatch, ResponseID: "resp_parent", UpstreamSessionGeneration: a.upstream.sessionGeneration, SelectedChannelID: 17, Purpose: ResponsesWSSendPurposeResponseSteer, Err: responsesws.ErrInvalidResponsesWSTransportSendResult})
	if a.closing.closed.Load() || a.steering.next != next {
		t.Fatal("stale result affected new steering batch")
	}
}

func TestResponsesWSSteeringRequestErrorClosesAndReleasesReserve(t *testing.T) {
	for _, phase := range []string{"before_terminal", "after_terminal"} {
		t.Run(phase, func(t *testing.T) {
			a, session, conn := newSteeringTestActor(t, 1000)
			parent := a.turns.active.attempt
			sendTestSteer(t, a, session, "resp_parent")
			next := a.steering.next
			if phase == "after_terminal" {
				steeringProviderFrame(a, `{"type":"response.steer.accepted","sequence_number":1,"steer":{"id":"s1","previous_response_id":"resp_parent"}}`)
				steeringProviderFrame(a, `{"type":"response.completed","sequence_number":2,"response":{"id":"resp_parent","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`)
			}
			a.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.create","model":"gpt-5","store":false,"input":[]}`)))
			payload := `{"type":"error","error":{"code":"invalid_request_error","message":"upstream request failed"}}`
			steeringProviderFrame(a, payload)
			if got, _ := conn.lastWrite.Load().(string); got != payload {
				t.Fatalf("request error changed: %s", got)
			}
			if !a.closing.closed.Load() || a.turns.active.attempt != nil || a.steering.next != nil || !next.RolledBack || parent.AppliedSettlement == nil {
				t.Fatal("failed request kept its turn or reservation")
			}
			if phase == "after_terminal" && (!parent.QuotaFinalized || parent.AppliedSettlement.AppliedFinalQuota <= 0) {
				t.Fatal("parent terminal settlement was lost")
			}
			user, token := readResponsesWSQuotaFixture(t)
			want := 1000 - int(parent.AppliedSettlement.AppliedFinalQuota)
			if user.Quota != want || token.RemainQuota != want {
				t.Fatalf("wrong final balances: want=%d user=%d token=%d", want, user.Quota, token.RemainQuota)
			}
			select {
			case req := <-session.requests:
				t.Fatalf("queued request was sent after error: %+v", req)
			default:
			}
		})
	}
}
