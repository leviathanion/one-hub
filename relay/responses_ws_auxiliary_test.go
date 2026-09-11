package relay

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"one-api/common/responsesws"
)

func TestResponsesWSAuxiliaryFailureStopsWorkerBeforeActorConsumesResult(t *testing.T) {
	for _, purpose := range []ResponsesWSSendPurpose{ResponsesWSSendPurposeResponseInject, ResponsesWSSendPurposeResponseSteer} {
		for _, status := range []responsesws.ResponsesWSTransportSendStatus{responsesws.ResponsesWSTransportSendAmbiguous, "invalid"} {
			t.Run(string(purpose)+"/"+string(status), func(t *testing.T) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest("GET", "/v1/responses", nil)
				actor := NewResponsesWSSessionActor(c)
				defer actor.close("test_cleanup")
				session := &responsesWSSendResultTestSession{result: responsesws.ResponsesWSTransportSendResult{Status: status, Err: errors.New("write result unknown")}}
				frame := responsesws.NewTextFrame([]byte(`{"type":"response.inject","response_id":"resp_a","input":[]}`))
				actor.workers.sendBytes.Add(int64(frame.PayloadLen()))
				actor.handleSendCommand(responsesWSSendCommand{AttemptID: "inject-a", Purpose: purpose, Session: session, Frame: frame, Receipt: &responsesWSSendCompletion{}})
				// 不消费 mailbox：worker 已经看到失败，后续排队命令必须在 I/O 前停止。
				result := actor.sendResultForCommand(context.Background(), responsesWSSendCommand{AttemptID: "create-b", Session: session, Frame: responsesws.NewTextFrame([]byte(`{"type":"response.create","model":"gpt-5"}`))})
				if result.Status != responsesws.ResponsesWSTransportSendNotAttempted || atomic.LoadInt32(&session.resultCalls) != 1 {
					t.Fatalf("later work reached upstream before actor consumed fatal result: result=%+v calls=%d", result, atomic.LoadInt32(&session.resultCalls))
				}
			})
		}
	}
}

func TestResponsesWSInjectCompletionSurvivesParentSettlement(t *testing.T) {
	for _, status := range []responsesws.ResponsesWSTransportSendStatus{responsesws.ResponsesWSTransportSendAttempted, responsesws.ResponsesWSTransportSendNotAttempted, responsesws.ResponsesWSTransportSendAmbiguous, "invalid"} {
		t.Run(string(status), func(t *testing.T) {
			actor, session, _ := newSteeringTestActor(t, 1000)
			parent := actor.turns.active.attempt
			actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.inject","response_id":"resp_parent","input":{"future":true}}`)))
			select {
			case <-session.requests:
			case <-time.After(time.Second):
				t.Fatal("inject was not sent")
			}
			event, ok := readResponsesWSEvent(t, actor).(ResponsesWSEventSendResult)
			if !ok || event.Purpose != ResponsesWSSendPurposeResponseInject || event.Completion == nil {
				t.Fatalf("inject completion missing: %+v", event)
			}
			// 原命令的完成结果晚于父结算与下一次 create。
			steeringProviderFrame(actor, `{"type":"response.completed","response":{"id":"resp_parent","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`)
			actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.create","model":"gpt-5","store":false,"input":"next"}`)))
			select {
			case <-session.requests:
			case <-time.After(time.Second):
				t.Fatal("inject completion blocked the next create")
			}
			next := actor.turns.pending.attempt
			if !parent.QuotaFinalized || next == nil {
				t.Fatal("parent was retained or next create was not admitted")
			}
			event.TransportResult = responsesws.ResponsesWSTransportSendResult{Status: status}
			if status != responsesws.ResponsesWSTransportSendAttempted {
				event.TransportResult.Err = errors.New("late inject send failure")
			}
			actor.handleSendResult(event)
			fatal := status == responsesws.ResponsesWSTransportSendAmbiguous || status == "invalid"
			if actor.closing.closed.Load() != fatal || !event.Completion.consumed {
				t.Fatalf("late completion lost its connection scope: closed=%t event=%+v", actor.closing.closed.Load(), event)
			}
			if !fatal {
				event.TransportResult = responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAmbiguous, Err: errors.New("duplicate")}
				actor.handleSendResult(event)
				if actor.closing.closed.Load() || actor.turns.pending.attempt != next || next.QuotaFinalized || next.RolledBack {
					t.Fatal("consumed old completion affected new work")
				}
			}
		})
	}
}
