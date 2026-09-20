package relay

import (
	"fmt"
	"testing"
	"time"
)

func sendRejectionTestCreate(t *testing.T, a *ResponsesWSSessionActor, session *responsesWSCaptureSendSession, lane string) *ResponsesWSTurnAttempt {
	t.Helper()
	suffix := ""
	if lane != "" {
		suffix = fmt.Sprintf(`,"stream_id":%q`, lane)
	}
	raw := `{"type":"response.create","model":"gpt-5","store":false,"future":9007199254740993` + suffix + `}`
	a.upstream.recvArmed = true // 本夹具直接提交真实上游回执。
	a.handleClientFrame(responsesWSTestClientTextFrame([]byte(raw)))
	select {
	case request := <-session.requests:
		if string(request.Frame.Payload()) != raw {
			t.Fatalf("client wire changed: %s", request.Frame.Payload())
		}
		work := a.observation.byAttempt(request.AttemptID)
		if work == nil {
			t.Fatal("work was not admitted")
		}
		a.handleEvent(readResponsesWSEvent(t, a))
		return work.attempt
	case <-time.After(time.Second):
		t.Fatal("create was not forwarded")
	}
	return nil
}

func TestResponsesWSRequestRejectionAllowsNextSerialWork(t *testing.T) {
	for _, lane := range []string{"", "named"} {
		t.Run("lane="+lane, func(t *testing.T) {
			a, session, conn := newSteeringTestActor(t, 1000)
			parent := a.observation.byResponse("resp_parent")
			if !a.finishObservedWork(parent) {
				t.Fatal("fixture parent settlement")
			}
			other := sendRejectionTestCreate(t, a, session, "other")
			failed := sendRejectionTestCreate(t, a, session, lane)
			suffix := ""
			if lane != "" {
				suffix = fmt.Sprintf(`,"stream_id":%q`, lane)
			}
			rejection := `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"bad_input"}` + suffix + `}`
			observedProviderFrame(t, a, conn, rejection)
			if !failed.RolledBack || other.RolledBack || a.observation.unsafeLane(lane) {
				t.Fatal("request rejection poisoned its lane or another work")
			}
			next := sendRejectionTestCreate(t, a, session, lane)
			// 已明确收尾的旧响应回执不占用下一条 create 的候选。
			observedProviderFrame(t, a, conn, `{"type":"response.created","response":{"id":"resp_parent"}`+suffix+`}`)
			observedProviderFrame(t, a, conn, `{"type":"response.completed","response":{"id":"resp_parent","usage":{"input_tokens":999,"output_tokens":1,"total_tokens":1000}}`+suffix+`}`)
			if next.RolledBack || next.SeenProviderResponseID != "" || next.Usage.TotalTokens != 0 {
				t.Fatal("late old response consumed the new candidate")
			}
			observedProviderFrame(t, a, conn, `{"type":"response.created","response":{"id":"resp_next"}`+suffix+`}`)
			observedProviderFrame(t, a, conn, `{"type":"response.completed","response":{"id":"resp_next","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`+suffix+`}`)
			observedProviderFrame(t, a, conn, `{"type":"response.created","stream_id":"other","response":{"id":"resp_other"}}`)
			observedProviderFrame(t, a, conn, `{"type":"response.completed","stream_id":"other","response":{"id":"resp_other","usage":{"input_tokens":6,"output_tokens":2,"total_tokens":8}}}`)
			if !next.QuotaFinalized || next.Usage.TotalTokens != 5 || !other.QuotaFinalized {
				t.Fatal("fresh serial work lost its independent billing")
			}
			user, token := readResponsesWSQuotaFixture(t)
			if user.Quota != 987 || token.RemainQuota != 987 {
				t.Fatalf("wrong settlement: user=%d token=%d", user.Quota, token.RemainQuota)
			}
		})
	}
}

func TestResponsesWSRequestRejectionWithMultipleCandidatesStaysAmbiguous(t *testing.T) {
	a, conn := newObservedTestActor(t)
	first := addObservedTestWork(t, a, "first", "lane", "")
	second := addObservedTestWork(t, a, "second", "lane", "")
	observedProviderFrame(t, a, conn, `{"type":"error","status":400,"stream_id":"lane","error":{"type":"invalid_request_error","code":"bad_input"}}`)
	if !first.RolledBack || !second.RolledBack || !a.observation.unsafeLane("lane") {
		t.Fatal("multi-candidate error guessed a request")
	}
	next := addObservedTestWork(t, a, "new", "lane", "")
	observedProviderFrame(t, a, conn, `{"type":"response.created","stream_id":"lane","response":{"id":"possibly_old"}}`)
	if !next.RolledBack || next.SeenProviderResponseID != "" {
		t.Fatal("old work was assigned to a new candidate")
	}
}

func TestResponsesWSRequestRejectionDoesNotGuessAuxiliaryCommand(t *testing.T) {
	for _, purpose := range []ResponsesWSSendPurpose{ResponsesWSSendPurposeResponseInject, ResponsesWSSendPurposeResponseSteer} {
		t.Run(string(purpose), func(t *testing.T) {
			a, session, conn := newSteeringTestActor(t, 1000)
			parent := a.observation.byResponse("resp_parent")
			a.holdSteeringParent(parent.attempt)
			a.finishObservedWork(parent)
			candidate := sendRejectionTestCreate(t, a, session, "")
			raw := `{"type":"response.inject","response_id":"resp_parent","input":"hello"}`
			if purpose == ResponsesWSSendPurposeResponseSteer {
				raw = `{"type":"response.steer","previous_response_id":"resp_parent","input":"hello"}`
			}
			a.handleClientFrame(responsesWSTestClientTextFrame([]byte(raw)))
			select {
			case request := <-session.requests:
				if string(request.Frame.Payload()) != raw {
					t.Fatal("control wire changed")
				}
			case <-time.After(time.Second):
				t.Fatal("control not queued")
			}
			a.handleEvent(readResponsesWSEvent(t, a))
			if purpose == ResponsesWSSendPurposeResponseSteer {
				// 候选容量已释放也不能抹掉已发控制命令的来源歧义。
				successor := observedSteeringCandidate(a, "resp_parent")
				if successor == nil || !a.finishObservedWork(a.observation.byAttempt(successor.AttemptID)) {
					t.Fatal("steer observation cleanup")
				}
			}
			observedProviderFrame(t, a, conn, `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"bad_input"}}`)
			if !candidate.RolledBack || !a.observation.unsafeLane("") {
				t.Fatal("control error was mistaken for a proved create rejection")
			}
		})
	}
}

func TestResponsesWSIdleErrorDoesNotPoisonFutureLane(t *testing.T) {
	a, conn := newObservedTestActor(t)
	observedProviderFrame(t, a, conn, `{"type":"error","stream_id":"lane","error":{"code":"diagnostic"}}`)
	if a.observation.unsafeLane("lane") {
		t.Fatal("idle error poisoned a future request")
	}
	next := addObservedTestWork(t, a, "new", "lane", "")
	observedProviderFrame(t, a, conn, `{"type":"response.created","stream_id":"lane","response":{"id":"new"}}`)
	observedProviderFrame(t, a, conn, `{"type":"response.completed","stream_id":"lane","response":{"id":"new","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
	if !next.QuotaFinalized {
		t.Fatal("new serial work was not billed")
	}
}

func TestResponsesWSRetiredIdentityCacheIsBounded(t *testing.T) {
	var observations responsesWSObservations
	for i := 0; i < responsesWSObservationLimit+1; i++ {
		id := fmt.Sprint(i)
		observations.seen.add(id)
		observations.rememberRetiredResponse(id)
	}
	if observations.retiredResponse("0") || !observations.retiredResponse("64") || !observations.seen.contains("0") {
		t.Fatal("bounded exact history or conservative fallback failed")
	}
	if len(observations.retired) != responsesWSObservationLimit {
		t.Fatal("unbounded retired response table")
	}
}

func TestResponsesWSRequestRejectionIncludesAuxiliaryWireLane(t *testing.T) {
	a, session, conn := newSteeringTestActor(t, 1000)
	parent := a.observation.byResponse("resp_parent")
	a.holdSteeringParent(parent.attempt)
	a.finishObservedWork(parent)
	candidate := sendRejectionTestCreate(t, a, session, "wire-lane")
	raw := `{"type":"response.inject","response_id":"resp_parent","stream_id":"wire-lane","input":"hello"}`
	a.handleClientFrame(responsesWSTestClientTextFrame([]byte(raw)))
	select {
	case request := <-session.requests:
		if string(request.Frame.Payload()) != raw {
			t.Fatal("control wire changed")
		}
	case <-time.After(time.Second):
		t.Fatal("control not queued")
	}
	a.handleEvent(readResponsesWSEvent(t, a))
	observedProviderFrame(t, a, conn, `{"type":"error","status":400,"stream_id":"wire-lane","error":{"type":"invalid_request_error","code":"bad_input"}}`)
	if !candidate.RolledBack || !a.observation.unsafeLane("wire-lane") {
		t.Fatal("wire-lane control rejection was misidentified as create rejection")
	}
}
