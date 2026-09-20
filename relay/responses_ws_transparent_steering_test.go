package relay

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"one-api/common/config"
	ratelimit "one-api/common/limit"
	"one-api/common/responsesws"
	"one-api/model"
)

type steeringCountingLimiter struct {
	ratelimit.RateLimiter
	calls int
}

func (l *steeringCountingLimiter) Allow(key string) bool { l.calls++; return l.RateLimiter.Allow(key) }

func TestResponsesWSCloseCutDoesNotAdmitQueuedWork(t *testing.T) {
	a, session, _ := newSteeringTestActor(t, 1000)
	reserves := 0
	name := "steering_close_reserve"
	if err := model.DB.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table != "users" {
			return
		}
		if fields, ok := tx.Statement.Dest.(map[string]interface{}); ok {
			if expr, ok := fields["quota"].(clause.Expr); ok && expr.SQL == "quota - ?" {
				reserves++
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { model.DB.Callback().Update().Remove(name) })
	model.GlobalUserGroupRatio.Lock()
	limiter := &steeringCountingLimiter{RateLimiter: model.GlobalUserGroupRatio.APILimiter["default"]}
	model.GlobalUserGroupRatio.APILimiter["default"] = limiter
	model.GlobalUserGroupRatio.Unlock()
	for _, payload := range []string{`{"type":"response.steer","previous_response_id":"resp_parent","input":"hi"}`, `{"type":"response.create","model":"gpt-5","store":false,"input":"hi"}`} {
		if !a.tryPostEvent(responsesWSTestClientTextFrame([]byte(payload))) {
			t.Fatal("enqueue failed")
		}
	}
	a.close("test_close")
	if reserves != 0 || limiter.calls != 0 {
		t.Fatalf("close started work: reserves=%d rpm=%d", reserves, limiter.calls)
	}
	select {
	case req := <-session.requests:
		t.Fatalf("sent after close: %+v", req)
	default:
	}
}

func TestResponsesWSSteeringCloseDuringTryDoesNotClaimOrSend(t *testing.T) {
	a, session, _ := newSteeringTestActor(t, 1000)
	var next *ResponsesWSTurnAttempt
	name := "steering_close_after_try"
	if err := model.DB.Callback().Update().After("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table != "users" {
			return
		}
		if fields, ok := tx.Statement.Dest.(map[string]interface{}); ok {
			if expr, ok := fields["quota"].(clause.Expr); ok && expr.SQL == "quota - ?" {
				next = observedSteeringCandidate(a, "resp_parent")
				a.markClientClosed(nil)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { model.DB.Callback().Update().Remove(name) })
	a.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.steer","previous_response_id":"resp_parent","input":"hi"}`)))
	if next == nil || next.Billing.SubmissionClaimed() || !next.RolledBack {
		t.Fatalf("Try was not cleaned without Claim: %+v", next)
	}
	select {
	case req := <-session.requests:
		t.Fatalf("sent after close: %+v", req)
	default:
	}
}

func TestResponsesWSSteeringUnknownControlIsTransparent(t *testing.T) {
	a, _, conn := newSteeringTestActor(t, 1000)
	parent := observedParentForTest(t, a)
	for _, payload := range []string{
		`{"type":"response.steer.failed","sequence_number":9999,"steer":{"id":"unknown","previous_response_id":"evicted-parent"},"error":{"code":"future_error","message":"keep me"},"future":9007199254740993}`,
		`{"type":"response.steer.pending","sequence_number":{"future":true},"steer":{"future_union":[1,2]},"reason":"future_reason","required_input":{"future":true}}`,
		`{"type":"response.steer.accepted","steer":null,"future":true}`,
	} {
		steeringProviderFrame(a, payload)
		if got, _ := conn.lastWrite.Load().(string); got != payload {
			t.Fatalf("receipt changed: %s", got)
		}
		if a.closing.closed.Load() || parent.DownstreamCommitted || parent.Usage.PromptTokens != 0 {
			t.Fatal("receipt changed active response")
		}
	}
}

func TestResponsesWSSteeringChildBindsWithoutCompleteAcceptedMirror(t *testing.T) {
	a, session, conn := newSteeringTestActor(t, 1000)
	sendTestSteer(t, a, session, "resp_parent")
	next := observedSteeringCandidate(a, "resp_parent")
	completion := readResponsesWSEvent(t, a).(ResponsesWSEventSendResult)
	steeringProviderFrame(a, `{"type":"response.completed","sequence_number":1,"response":{"id":"resp_parent","status":"completed"}}`)
	steeringProviderFrame(a, `{"type":"response.created","sequence_number":0,"response":{"id":"resp_child","previous_response_id":"resp_parent","status":"in_progress"}}`)
	if a.closing.closed.Load() || a.observation.byResponse("resp_child") == nil || next.SeenProviderResponseID != "resp_child" {
		t.Fatal("unique claimed reservation failed to bind or lost incomplete parent's proof")
	}
	payload := `{"type":"response.steer.accepted","sequence_number":999,"steer":{"id":"late","previous_response_id":"resp_parent"}}`
	steeringProviderFrame(a, payload)
	if got, _ := conn.lastWrite.Load().(string); got != payload {
		t.Fatal("late accepted lost")
	}
	if next.Usage.PromptTokens != 0 || next.RolledBack || next.QuotaFinalized {
		t.Fatal("late receipt polluted child evidence")
	}
	// 原 command 首次报告的真实歧义仍作用于连接，即使候选已绑定。
	completion.TransportResult = responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAmbiguous, Err: errors.New("uncertain write")}
	a.handleSendResult(completion)
	if !a.closing.closed.Load() {
		t.Fatal("unconsumed late ambiguous result was ignored")
	}
}

func TestResponsesWSSteeringNotAttemptedConsumesOnlyOriginalCommandOnce(t *testing.T) {
	a, session, _ := newSteeringTestActor(t, 1000)
	sendTestSteer(t, a, session, "resp_parent")
	event := readResponsesWSEvent(t, a).(ResponsesWSEventSendResult)
	old := observedSteeringCandidate(a, "resp_parent")
	event.TransportResult = responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendNotAttempted, Err: errors.New("not sent")}
	a.handleSendResult(event)
	if !old.RolledBack || observedSteeringCandidate(a, "resp_parent") != nil {
		t.Fatal("unsent obligation leaked")
	}
	sendTestSteer(t, a, session, "resp_parent")
	next := observedSteeringCandidate(a, "resp_parent")
	a.handleSendResult(event)
	if a.closing.closed.Load() || observedSteeringCandidate(a, "resp_parent") != next {
		t.Fatal("duplicate completion touched new candidate")
	}
}

func TestResponsesWSStoredSteeringChildOwnerFailurePreservesCutUsage(t *testing.T) {
	a, session, conn := newSteeringTestActor(t, 1000)
	ctx := a.Context()
	ctx.Set("channel_type", config.ChannelTypeOpenAI)
	a.RefreshContext(ctx)
	sendTestSteer(t, a, session, "resp_parent")
	next := observedSteeringCandidate(a, "resp_parent")
	next.RequireStoredOwner = true
	steeringProviderFrame(a, `{"type":"response.completed","sequence_number":1,"response":{"id":"resp_parent","status":"completed"}}`)
	writes := 0
	name := "steering_owner_failure"
	if err := model.DB.Callback().Create().Before("gorm:create").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "response_owners" {
			writes++
			tx.AddError(errors.New("owner unavailable"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { model.DB.Callback().Create().Remove(name) })
	terminal := ResponsesWSEventProviderDownstream{AttemptID: "transport-parent", UpstreamSessionGeneration: a.upstream.sessionGeneration, ChannelID: 17, Kind: ProviderDownstreamFrame, DetailOrigin: responsesws.RecvDetailOriginProviderFrame, Frame: responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_child","status":"completed","usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`))}
	if !a.tryPostEvent(terminal) {
		t.Fatal("enqueue terminal failed")
	}
	steeringProviderFrame(a, `{"type":"response.created","sequence_number":0,"response":{"id":"resp_child","previous_response_id":"resp_parent","status":"in_progress"}}`)
	if !a.closing.closed.Load() || writes != 1 || !next.QuotaFinalized || next.Usage.PromptTokens != 7 || next.Usage.CompletionTokens != 3 {
		t.Fatalf("owner barrier lost cut evidence: writes=%d attempt=%+v", writes, next)
	}
	if got, _ := conn.lastWrite.Load().(string); strings.Contains(got, "resp_child") || !strings.Contains(got, "responses_owner_persist_failed") {
		t.Fatalf("owner failure exposed resource: %s", got)
	}
}

func TestResponsesWSSteeringDoesNotCountHistoricalPayloadBytes(t *testing.T) {
	a, session, _ := newSteeringTestActor(t, 1000)
	payload := `{"type":"response.steer","previous_response_id":"resp_parent","input":"` + strings.Repeat("x", (1<<20)+1) + `"}`
	for i := 0; i < 5; i++ {
		a.handleClientFrame(responsesWSTestClientTextFrame([]byte(payload)))
		select {
		case req := <-session.requests:
			if string(req.Frame.Payload()) != payload {
				t.Fatal("large frame mutated")
			}
		case <-time.After(time.Second):
			t.Fatalf("historical payload budget rejected frame %d", i)
		}
	}
	if len(a.observation.works) != 2 {
		t.Fatal("same parent submissions created duplicate reservations")
	}
}

func TestResponsesWSSteeringKnownResponseUsageDoesNotDependOnLastSendID(t *testing.T) {
	a, _, _ := newSteeringTestActor(t, 1000)
	parent := observedParentForTest(t, a)
	a.handleProviderDownstream(ResponsesWSEventProviderDownstream{AttemptID: "diagnostic-last-send", UpstreamSessionGeneration: a.upstream.sessionGeneration, ChannelID: 17, Kind: ProviderDownstreamFrame, DetailOrigin: responsesws.RecvDetailOriginProviderFrame, Frame: responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_parent","status":"completed","usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`))})
	if !parent.QuotaFinalized || parent.Usage.PromptTokens != 7 || a.closing.closed.Load() {
		t.Fatal("transport diagnostic ID displaced bound response owner")
	}
}

func TestResponsesWSOpenHandoffIsDecidedBeforeCloseAndCleanup(t *testing.T) {
	a := NewResponsesWSSessionActor(nil)
	session := &responsesWSTestSession{}
	lease := &responsesWSTestLease{}
	result := &responsesWSOpenResult{Session: session, ActiveLease: lease, Channel: &model.Channel{Id: 17}}
	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","store":false,"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	opening := a.ReserveFirstTurnOpening(frame)
	adopted := make(chan bool, 1)
	a.markClientClosed(nil)
	a.handleFirstTurnOpenResult(ResponsesWSEventFirstTurnOpenResult{OpeningID: opening, OpenResult: result, Adopted: adopted})
	select {
	case <-a.done:
	default:
		t.Fatal("actor did not close")
	}
	// 即使 worker 同时看到 done，也必须读取已经确定的交接，不能再次清理。
	select {
	case ok := <-adopted:
		if !ok {
			cleanupResponsesWSOpenResult(result, "worker_refused")
		}
	default:
		t.Fatal("done published before handoff decision")
	}
	if session.abortCount != 1 || lease.releases != 1 {
		t.Fatalf("resource cleanup repeated: abort=%d lease=%d", session.abortCount, lease.releases)
	}
}

func TestResponsesWSSteeringUnknownReceiptCannotAdvanceParentSequence(t *testing.T) {
	a, session, _ := newSteeringTestActor(t, 1000)
	sendTestSteer(t, a, session, "resp_parent")
	parent := observedParentForTest(t, a)
	steeringProviderFrame(a, `{"type":"response.steer.failed","sequence_number":999,"steer":{"id":"unknown","previous_response_id":"resp_parent"}}`)
	steeringProviderFrame(a, `{"type":"response.steer.pending","sequence_number":1000,"steer":{"id":"unknown","previous_response_id":"resp_parent"},"reason":"future"}`)
	steeringProviderFrame(a, `{"type":"response.steer.accepted","sequence_number":1,"steer":{"id":"s1","previous_response_id":"resp_parent"}}`)
	steeringProviderFrame(a, `{"type":"response.completed","sequence_number":2,"response":{"id":"resp_parent","status":"completed","usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`)
	if a.closing.closed.Load() || !parent.QuotaFinalized || parent.Usage.PromptTokens != 7 {
		t.Fatal("diagnostic receipt corrupted parent evidence")
	}
}

func TestResponsesWSAttachedIdentityCannotOverrideWireResponse(t *testing.T) {
	a, _, _ := newSteeringTestActor(t, 1000)
	parent := observedParentForTest(t, a)
	a.handleProviderDownstream(ResponsesWSEventProviderDownstream{AttemptID: "diagnostic-last-send", ResponseID: "resp_parent", UpstreamSessionGeneration: a.upstream.sessionGeneration, ChannelID: 17, Kind: ProviderDownstreamFrame, DetailOrigin: responsesws.RecvDetailOriginProviderFrame, Frame: responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_other","status":"completed","usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`))})
	if a.closing.closed.Load() || parent.Usage.PromptTokens != 0 || parent.QuotaFinalized {
		t.Fatal("conflicting raw response identity was charged to current owner")
	}
}

func TestResponsesWSSteeringUnknownReceiptIdentityIsNotRefundEvidence(t *testing.T) {
	a, session, conn := newSteeringTestActor(t, 1000)
	sendTestSteer(t, a, session, "resp_parent")
	next := observedSteeringCandidate(a, "resp_parent")
	payload := `{"type":"response.steer.failed","sequence_number":null,"steer":{"id":"unknown","previous_response_id":"resp_parent"}}`
	steeringProviderFrame(a, payload)
	if got, _ := conn.lastWrite.Load().(string); got != payload {
		t.Fatal("unknown sequence shape did not pass through")
	}
	if a.closing.closed.Load() || observedSteeringCandidate(a, "resp_parent") != next || next.RolledBack {
		t.Fatal("null sequence was interpreted as a fresh failure")
	}
}

func TestResponsesWSSteeringConfirmedRejectionReleasesOnlyUnusedReservation(t *testing.T) {
	a, session, conn := newSteeringTestActor(t, 1000)
	parent := observedParentForTest(t, a)
	sendTestSteer(t, a, session, "resp_parent")
	next := observedSteeringCandidate(a, "resp_parent")
	steeringProviderFrame(a, `{"type":"response.steer.accepted","steer":{"id":"s1","previous_response_id":"resp_parent"}}`)
	payload := `{"type":"response.steer.failed","steer":{"id":"s1","previous_response_id":"resp_parent"},"error":{"code":"invalid_input","message":"rejected"},"future":true}`
	steeringProviderFrame(a, payload)
	if got, _ := conn.lastWrite.Load().(string); got != payload {
		t.Fatalf("rejection was changed: %s", got)
	}
	if a.closing.closed.Load() || !next.RolledBack || a.observation.byAttempt(next.AttemptID) != nil || parent.RolledBack {
		t.Fatal("rejection retained reservation or canceled identified parent")
	}
	steeringProviderFrame(a, payload)
	steeringProviderFrame(a, `{"type":"response.completed","response":{"id":"resp_parent","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`)
	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 993 || token.RemainQuota != 993 {
		t.Fatalf("rejection was settled more than once: user=%d token=%d", user.Quota, token.RemainQuota)
	}
}

func TestResponsesWSWorkflowErrorPreservesBoundUsageAndWire(t *testing.T) {
	a, _, conn := newSteeringTestActor(t, 1000)
	parent := observedParentForTest(t, a)
	payload := `{"type":"response.failed","response":{"id":"resp_parent","status":"failed","error":{"code":"misalignment_policy_violation","message":"stop"},"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`
	a.handleProviderDownstream(ResponsesWSEventProviderDownstream{AttemptID: "diagnostic-last-send", UpstreamSessionGeneration: a.upstream.sessionGeneration, ChannelID: 17, Kind: ProviderDownstreamFrame, DetailOrigin: responsesws.RecvDetailOriginProviderFrame, Frame: responsesWSTestProviderTextFrame([]byte(payload))})
	if a.closing.closed.Load() || !parent.QuotaFinalized || parent.Usage.PromptTokens != 7 {
		t.Fatal("upstream business error altered connection or lost legitimate usage")
	}
	if got, _ := conn.lastWrite.Load().(string); got != payload {
		t.Fatalf("workflow error changed: %s", got)
	}
}
