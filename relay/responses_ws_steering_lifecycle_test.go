package relay

import (
	"errors"
	"testing"
	"time"

	"one-api/common/responsesws"
)

func sendExplicitSteeringContinuationForTest(t *testing.T, a *ResponsesWSSessionActor, session *responsesWSCaptureSendSession, lane string) *ResponsesWSTurnAttempt {
	t.Helper()
	a.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.create","stream_id":"` + lane + `","model":"gpt-5","store":false,"previous_response_id":"resp_parent","input":[{"type":"function_call_output","call_id":"call_1","output":"saved result"}]}`)))
	select {
	case req := <-session.requests:
		work := a.observation.byAttempt(req.AttemptID)
		if work == nil {
			t.Fatal("explicit continuation was sent without admission")
		}
		return work.attempt
	case <-time.After(time.Second):
		t.Fatal("explicit continuation was blocked")
	}
	return nil
}

func TestResponsesWSSteeringLateControlPreservesWireWithoutChangingOtherLane(t *testing.T) {
	for _, kind := range []string{"pending", "failed", "accepted"} {
		t.Run(kind, func(t *testing.T) {
			a, session, conn := newSteeringTestActor(t, 1000)
			sendTestSteer(t, a, session, "resp_parent")
			next := sendExplicitSteeringContinuationForTest(t, a, session, "independent")
			steeringProviderFrame(a, `{"type":"response.created","stream_id":"independent","response":{"id":"resp_independent"}}`)
			payload := `{"type":"response.steer.` + kind + `","sequence_number":5,"steer":{"id":"old","previous_response_id":"resp_parent","input":"user correction"},"error":{"code":"successor_creation_failed","message":"failed"},"future":{"kept":true}}`
			steeringProviderFrame(a, payload)
			if got, _ := conn.lastWrite.Load().(string); got != payload {
				t.Fatalf("late receipt/input lost: %s", got)
			}
			if a.closing.closed.Load() || next.SeenProviderResponseID != "resp_independent" || next.Usage.PromptTokens != 0 || next.QuotaFinalized || next.RolledBack {
				t.Fatal("old receipt changed independent work")
			}
		})
	}
}

func TestResponsesWSSteeringSendResultUsesCandidateIdentityOnce(t *testing.T) {
	a, session, _ := newSteeringTestActor(t, 1000)
	sendTestSteer(t, a, session, "resp_parent")
	next := observedSteeringCandidate(a, "resp_parent")
	event, ok := readResponsesWSEvent(t, a).(ResponsesWSEventSendResult)
	if !ok || event.AttemptID != next.AttemptID || event.ResponseID != "resp_parent" || event.Purpose != ResponsesWSSendPurposeResponseSteer || event.Completion == nil {
		t.Fatalf("steer result did not identify candidate: %+v", event)
	}
	a.handleSendResult(event)
	if a.closing.closed.Load() {
		t.Fatal("successful send closed session")
	}
	// 一个提交完成记录只能消费一次，重复的诊断结果不能撤回成功事实。
	event.TransportResult = responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAmbiguous, Err: errors.New("duplicate completion")}
	a.handleSendResult(event)
	if a.closing.closed.Load() || observedSteeringCandidate(a, "resp_parent") != next {
		t.Fatal("duplicate completion affected candidate")
	}
}

func TestResponsesWSSteeringGenericErrorReleasesUnboundObservationWithoutClosing(t *testing.T) {
	a, session, conn := newSteeringTestActor(t, 1000)
	parent := observedParentForTest(t, a)
	sendTestSteer(t, a, session, "resp_parent")
	successor := observedSteeringCandidate(a, "resp_parent")
	queued := sendExplicitSteeringContinuationForTest(t, a, session, "")
	payload := `{"type":"error","error":{"code":"invalid_request_error","message":"upstream request failed"}}`
	steeringProviderFrame(a, payload)
	if got, _ := conn.lastWrite.Load().(string); got != payload {
		t.Fatalf("request error changed: %s", got)
	}
	if a.closing.closed.Load() || !successor.RolledBack || !queued.RolledBack || len(a.observation.works) != 1 || parent.RolledBack {
		t.Fatal("ambiguity must release only unbound candidates and keep identified parent")
	}
	// 迟到自动后继仍交付，但不能占用已撤销候选，更不能串到下一笔预扣。
	late := `{"type":"response.created","response":{"id":"resp_late","previous_response_id":"resp_parent"}}`
	steeringProviderFrame(a, late)
	if got, _ := conn.lastWrite.Load().(string); got != late {
		t.Fatalf("late created was suppressed: %s", got)
	}
	steeringProviderFrame(a, `{"type":"response.completed","response":{"id":"resp_late","usage":{"input_tokens":100,"output_tokens":100,"total_tokens":200}}}`)
	steeringProviderFrame(a, `{"type":"response.completed","response":{"id":"resp_parent","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`)
	a.close("client_closed")
	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 993 || token.RemainQuota != 993 || !parent.QuotaFinalized {
		t.Fatalf("unattributed evidence charged or parent lost: user=%d token=%d", user.Quota, token.RemainQuota)
	}
}

func TestResponsesWSSteeringRetiredConnectionCannotAffectCurrentWork(t *testing.T) {
	a, session, conn := newSteeringTestActor(t, 1000)
	sendTestSteer(t, a, session, "resp_parent")
	next := observedSteeringCandidate(a, "resp_parent")
	payload := `{"type":"error","error":{"code":"invalid_request_error","message":"old connection"}}`
	a.handleProviderDownstream(ResponsesWSEventProviderDownstream{AttemptID: next.AttemptID, UpstreamSessionGeneration: "retired-generation", ChannelID: 17, Kind: ProviderDownstreamFrame, Frame: responsesWSTestProviderTextFrame([]byte(payload)), DetailOrigin: responsesws.RecvDetailOriginProviderFrame})
	if got, _ := conn.lastWrite.Load().(string); got == payload || a.closing.closed.Load() || next.RolledBack {
		t.Fatal("retired connection affected current observation")
	}
}

func TestResponsesWSSteeringCoalescedUnsentCommandCannotShiftSuccessorToNewCreate(t *testing.T) {
	a, session, conn := newSteeringTestActor(t, 1000)
	sendTestSteer(t, a, session, "resp_parent")
	first := readResponsesWSEvent(t, a).(ResponsesWSEventSendResult)
	a.handleSendResult(first)
	sendTestSteer(t, a, session, "resp_parent")
	second := readResponsesWSEvent(t, a).(ResponsesWSEventSendResult)
	second.TransportResult = responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendNotAttempted, Err: errors.New("second command did not reach transport")}
	a.handleSendResult(second)
	// 首条 steer 仍可能产生后继；撤掉观察后不能重新使用该 lane 的普通 FIFO 猜身份。
	next := sendExplicitSteeringContinuationForTest(t, a, session, "")
	payload := `{"type":"response.created","response":{"id":"resp_delayed_successor","previous_response_id":"resp_parent"}}`
	steeringProviderFrame(a, payload)
	if got, _ := conn.lastWrite.Load().(string); got != payload {
		t.Fatalf("successor raw frame lost: %s", got)
	}
	if a.closing.closed.Load() || next.SeenProviderResponseID != "" || !next.RolledBack {
		t.Fatal("coalesced delivery uncertainty assigned successor to later create")
	}
}

func TestResponsesWSSteeringAbandonedObservationStillConsumesRealTransportFailure(t *testing.T) {
	a, session, _ := newSteeringTestActor(t, 1000)
	sendTestSteer(t, a, session, "resp_parent")
	completion := readResponsesWSEvent(t, a).(ResponsesWSEventSendResult)
	steeringProviderFrame(a, `{"type":"error","error":{"code":"unknown","message":"uncorrelated"}}`)
	if a.observation.byAttempt(completion.AttemptID) != nil {
		t.Fatal("ambiguous candidate did not release capacity")
	}
	completion.TransportResult = responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAmbiguous, Err: errors.New("real uncertain write")}
	a.handleSendResult(completion)
	if !a.closing.closed.Load() {
		t.Fatal("abandoned billing observation hid a real transport failure")
	}
}
