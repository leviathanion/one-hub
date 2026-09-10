package relay

import (
	"errors"
	"fmt"
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
				next = a.steering.next
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
	parent := a.turns.active.attempt
	for _, payload := range []string{
		`{"type":"response.steer.failed","sequence_number":9999,"steer":{"id":"unknown","previous_response_id":"evicted-parent"},"error":{"code":"future_error","message":"keep me"},"future":9007199254740993}`,
		`{"type":"response.steer.pending","sequence_number":{"future":true},"steer":{"future_union":[1,2]},"reason":"future_reason","required_input":{"future":true}}`,
		`{"type":"response.steer.accepted","steer":null,"future":true}`,
	} {
		steeringProviderFrame(a, payload)
		if got, _ := conn.lastWrite.Load().(string); got != payload {
			t.Fatalf("receipt changed: %s", got)
		}
		if a.closing.closed.Load() || parent.DownstreamCommitted || a.turns.active.hasLastProviderSequence || parent.Usage.PromptTokens != 0 {
			t.Fatal("receipt changed active response")
		}
	}
}

func TestResponsesWSSteeringInitialFailuresAndReplay(t *testing.T) {
	a, session, _ := newSteeringTestActor(t, 1000)
	sendTestSteer(t, a, session, "resp_parent")
	sendTestSteer(t, a, session, "resp_parent")
	old := a.steering.next
	steeringProviderFrame(a, `{"type":"response.steer.accepted","sequence_number":1,"steer":{"id":"s1","previous_response_id":"resp_parent"}}`)
	steeringProviderFrame(a, `{"type":"response.steer.failed","sequence_number":2,"steer":{"id":"s1","previous_response_id":"resp_parent"}}`)
	steeringProviderFrame(a, `{"type":"response.steer.failed","sequence_number":3,"steer":{"id":"unknown","previous_response_id":"resp_parent"}}`)
	if a.steering.awaiting != 1 || a.steering.next != old || old.RolledBack {
		t.Fatal("accepted failure or unknown ID consumed another initial obligation")
	}
	initialFailure := `{"type":"response.steer.failed","sequence_number":4,"steer":{"previous_response_id":"resp_parent"},"error":{"code":"invalid_input"}}`
	steeringProviderFrame(a, initialFailure)
	if !old.RolledBack || a.steering.next != nil || len(a.steering.accepted) != 0 {
		t.Fatal("no-ID rejection did not cancel and release observation")
	}
	sendTestSteer(t, a, session, "resp_parent")
	next := a.steering.next
	steeringProviderFrame(a, initialFailure)
	if a.closing.closed.Load() || a.steering.next != next || a.steering.awaiting != 1 || next.RolledBack {
		t.Fatal("replayed old fact canceled new candidate")
	}
	steeringProviderFrame(a, `{"type":"response.steer.failed","sequence_number":5,"steer":{"previous_response_id":"resp_parent"}}`)
	if !next.RolledBack {
		t.Fatal("fresh rejection did not cancel")
	}
}

func TestResponsesWSSteeringPendingKeepsInitialReplyBarrier(t *testing.T) {
	a, session, _ := newSteeringTestActor(t, 1000)
	sendTestSteer(t, a, session, "resp_parent")
	sendTestSteer(t, a, session, "resp_parent")
	next := a.steering.next
	steeringProviderFrame(a, `{"type":"response.steer.accepted","sequence_number":1,"steer":{"id":"s1","previous_response_id":"resp_parent"}}`)
	steeringProviderFrame(a, `{"type":"response.completed","sequence_number":2,"response":{"id":"resp_parent","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
	steeringProviderFrame(a, `{"type":"response.steer.pending","sequence_number":3,"steer":{"id":"s1","previous_response_id":"resp_parent"},"reason":"waiting_for_required_input"}`)
	if next.RolledBack || a.steering.awaiting != 1 || a.turns.active.attempt != nil {
		t.Fatal("pending released unacknowledged work or retained settled parent")
	}
	steeringProviderFrame(a, `{"type":"response.steer.failed","sequence_number":4,"steer":{"previous_response_id":"resp_parent"}}`)
	if !next.RolledBack || a.steering.next != nil || !a.hasHeldSteeringParent(a.Context(), "resp_parent") {
		t.Fatal("pending did not retain only resource proof")
	}
}

func TestResponsesWSSteeringUnknownPendingDoesNotRefund(t *testing.T) {
	a, session, conn := newSteeringTestActor(t, 1000)
	sendTestSteer(t, a, session, "resp_parent")
	next := a.steering.next
	steeringProviderFrame(a, `{"type":"response.steer.accepted","sequence_number":1,"steer":{"id":"s1","previous_response_id":"resp_parent"}}`)
	steeringProviderFrame(a, `{"type":"response.completed","sequence_number":2,"response":{"id":"resp_parent","status":"completed"}}`)
	payload := `{"type":"response.steer.pending","sequence_number":3,"steer":{"id":"s1","previous_response_id":"resp_parent"},"reason":"future_reason"}`
	steeringProviderFrame(a, payload)
	if got, _ := conn.lastWrite.Load().(string); got != payload {
		t.Fatal("unknown pending lost")
	}
	if next.RolledBack || a.steering.next != next || a.watchdogAttempt() != next {
		t.Fatal("unknown reason canceled or lost watchdog target")
	}
}

func TestResponsesWSSteeringChildBindsWithoutCompleteAcceptedMirror(t *testing.T) {
	a, session, conn := newSteeringTestActor(t, 1000)
	sendTestSteer(t, a, session, "resp_parent")
	next := a.steering.next
	completion := readResponsesWSEvent(t, a).(ResponsesWSEventSendResult)
	steeringProviderFrame(a, `{"type":"response.completed","sequence_number":1,"response":{"id":"resp_parent","status":"completed"}}`)
	steeringProviderFrame(a, `{"type":"response.created","sequence_number":0,"response":{"id":"resp_child","previous_response_id":"resp_parent","status":"in_progress"}}`)
	if a.closing.closed.Load() || a.turns.active.attempt != next || a.steering.next != nil || !a.hasHeldSteeringParent(a.Context(), "resp_parent") {
		t.Fatal("unique claimed reservation failed to bind or lost incomplete parent's proof")
	}
	payload := `{"type":"response.steer.accepted","sequence_number":999,"steer":{"id":"late","previous_response_id":"resp_parent"}}`
	steeringProviderFrame(a, payload)
	if got, _ := conn.lastWrite.Load().(string); got != payload {
		t.Fatal("late accepted lost")
	}
	if a.turns.active.lastProviderSequence != 0 {
		t.Fatal("late receipt polluted child sequence")
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
	old := a.steering.next
	event.TransportResult = responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendNotAttempted, Err: errors.New("not sent")}
	a.handleSendResult(event)
	if !old.RolledBack || a.steering.next != nil {
		t.Fatal("unsent obligation leaked")
	}
	sendTestSteer(t, a, session, "resp_parent")
	next := a.steering.next
	a.handleSendResult(event)
	if a.closing.closed.Load() || a.steering.next != next || a.steering.awaiting != 1 {
		t.Fatal("duplicate completion touched new candidate")
	}
}

func completeExplicitTestResponse(t *testing.T, a *ResponsesWSSessionActor, session *responsesWSCaptureSendSession, id string) {
	t.Helper()
	next := a.turns.pending.attempt
	if next == nil {
		t.Fatal("explicit candidate missing")
	}
	select {
	case <-session.requests:
	case <-time.After(time.Second):
		t.Fatal("explicit create was not sent")
	}
	a.handleSendResult(ResponsesWSEventSendResult{AttemptID: next.AttemptID, SelectedChannelID: 17, UpstreamSessionGeneration: a.upstream.sessionGeneration, Purpose: ResponsesWSSendPurposeResponseCreate, TransportResult: responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAttempted}})
	for _, payload := range []string{
		fmt.Sprintf(`{"type":"response.created","sequence_number":0,"response":{"id":%q,"status":"in_progress"}}`, id),
		fmt.Sprintf(`{"type":"response.completed","sequence_number":1,"response":{"id":%q,"status":"completed","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`, id),
	} {
		a.handleProviderDownstream(ResponsesWSEventProviderDownstream{AttemptID: next.AttemptID, UpstreamSessionGeneration: a.upstream.sessionGeneration, ChannelID: 17, Kind: ProviderDownstreamFrame, Frame: responsesWSTestProviderTextFrame([]byte(payload)), DetailOrigin: responsesws.RecvDetailOriginProviderFrame})
	}
	if a.closing.closed.Load() {
		t.Fatal("explicit response unexpectedly closed")
	}
}

func TestResponsesWSSteeringParentProofOutlivesHistoryAndCache(t *testing.T) {
	a, session, conn := pendingSteeringBatchForTest(t)
	for i := 0; i <= responsesWSRecentResponseIDLimit; i++ {
		a.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.create","model":"gpt-5","store":false,"input":[]}`)))
		completeExplicitTestResponse(t, a, session, fmt.Sprintf("resp_other_%d", i))
	}
	clearResponsesEphemeralProof(a.Context(), "resp_parent", 17)
	if _, ok := lookupResponsesEphemeralProof(a.Context(), "resp_parent"); ok {
		t.Fatal("cache was not cleared")
	}
	payload := `{"type":"response.steer.failed","sequence_number":5,"steer":{"id":"s2","previous_response_id":"resp_parent","input":"original"},"error":{"code":"successor_creation_failed"}}`
	steeringProviderFrame(a, payload)
	if got, _ := conn.lastWrite.Load().(string); got != payload || a.closing.closed.Load() {
		t.Fatal("evicted parent's late receipt lost")
	}
	a.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.create","model":"gpt-5","store":false,"previous_response_id":"resp_parent","input":"continue"}`)))
	if !a.hasHeldSteeringParent(a.Context(), "resp_parent") {
		t.Fatal("sending explicit create prematurely consumed proof")
	}
	completeExplicitTestResponse(t, a, session, "resp_continued")
	if a.hasHeldSteeringParent(a.Context(), "resp_parent") {
		t.Fatal("matching created did not release proof")
	}
}

func TestResponsesWSStoredSteeringChildOwnerFailurePreservesCutUsage(t *testing.T) {
	a, session, conn := newSteeringTestActor(t, 1000)
	ctx := a.Context()
	ctx.Set("channel_type", config.ChannelTypeOpenAI)
	a.RefreshContext(ctx)
	sendTestSteer(t, a, session, "resp_parent")
	next := a.steering.next
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
	if a.steering.awaiting != 5 {
		t.Fatal("large submissions lost")
	}
}

func TestResponsesWSSteeringFIFOIsNotReorderedForMatchingParent(t *testing.T) {
	a, session, _ := newSteeringTestActor(t, 1000)
	sendTestSteer(t, a, session, "resp_parent")
	for _, payload := range []string{
		`{"type":"response.create","model":"gpt-5","store":false,"input":"unrelated first"}`,
		`{"type":"response.create","model":"gpt-5","store":false,"previous_response_id":"resp_parent","input":"matching second"}`,
	} {
		a.handleClientFrame(responsesWSTestClientTextFrame([]byte(payload)))
	}
	steeringProviderFrame(a, `{"type":"response.steer.accepted","sequence_number":1,"steer":{"id":"s1","previous_response_id":"resp_parent"}}`)
	steeringProviderFrame(a, `{"type":"response.completed","sequence_number":2,"response":{"id":"resp_parent","status":"completed"}}`)
	if a.turns.pending.attempt != nil {
		t.Fatal("FIFO crossed unresolved successor")
	}
	steeringProviderFrame(a, `{"type":"response.steer.pending","sequence_number":3,"steer":{"id":"s1","previous_response_id":"resp_parent"},"reason":"waiting_for_required_input"}`)
	if a.turns.pending.attempt == nil || a.turns.pending.attempt.AttemptedPreviousResponseID != "" {
		t.Fatal("matching continuation overtook FIFO head")
	}
	completeExplicitTestResponse(t, a, session, "resp_unrelated")
	if a.turns.pending.attempt == nil || a.turns.pending.attempt.AttemptedPreviousResponseID != "resp_parent" {
		t.Fatal("FIFO did not advance to matching continuation")
	}
	completeExplicitTestResponse(t, a, session, "resp_matching")
}

func TestResponsesWSSteeringHeldProofCapacityRejectsBeforeWork(t *testing.T) {
	for _, byBytes := range []bool{false, true} {
		t.Run(fmt.Sprint(byBytes), func(t *testing.T) {
			a, session, _ := newSteeringTestActor(t, 1000)
			a.heldSteeringParents = make(map[string]responsesWSParentProof)
			if byBytes {
				id := strings.Repeat("x", responsesWSHeldParentMaxBytes)
				a.heldSteeringParents[id] = responsesWSParentProof{}
				a.heldSteeringParentBytes = len(id)
			} else {
				for i := 0; i < responsesWSHeldParentLimit; i++ {
					id := fmt.Sprint(i)
					a.heldSteeringParents[id] = responsesWSParentProof{}
					a.heldSteeringParentBytes += len(id)
				}
			}
			a.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.steer","previous_response_id":"resp_parent","input":"hi"}`)))
			if a.steering.next != nil {
				t.Fatal("capacity rejection admitted a reservation")
			}
			select {
			case req := <-session.requests:
				t.Fatalf("capacity rejection sent: %+v", req)
			default:
			}
		})
	}
}

func TestResponsesWSSteeringKnownResponseUsageDoesNotDependOnLastSendID(t *testing.T) {
	a, _, _ := newSteeringTestActor(t, 1000)
	parent := a.turns.active.attempt
	a.handleProviderDownstream(ResponsesWSEventProviderDownstream{AttemptID: "diagnostic-last-send", UpstreamSessionGeneration: a.upstream.sessionGeneration, ChannelID: 17, Kind: ProviderDownstreamFrame, DetailOrigin: responsesws.RecvDetailOriginProviderFrame, Frame: responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_parent","status":"completed","usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`))})
	if !parent.QuotaFinalized || parent.Usage.PromptTokens != 7 || a.closing.closed.Load() {
		t.Fatal("transport diagnostic ID displaced bound response owner")
	}
}

func TestResponsesWSSteeringWatchdogFollowsCandidateThenChild(t *testing.T) {
	setResponsesWSTestViperInt(t, "responses_ws.active_turn_timeout_ms", 30000)
	a, session, _ := newSteeringTestActor(t, 1000)
	a.armActiveTurnWatchdog()
	parentID, parentGen := a.turns.active.attempt.AttemptID, a.watchdog.activeTurnTimerGen
	sendTestSteer(t, a, session, "resp_parent")
	next := a.steering.next
	steeringProviderFrame(a, `{"type":"response.completed","sequence_number":1,"response":{"id":"resp_parent","status":"completed"}}`)
	candidateGen := a.watchdog.activeTurnTimerGen
	a.handleTimeout(ResponsesWSEventTimeout{Reason: responsesWSActiveTurnTimeoutReason, AttemptID: parentID, TimeoutGeneration: parentGen, UpstreamSessionGeneration: a.upstream.sessionGeneration, ChannelID: 17})
	if a.closing.closed.Load() || a.watchdogAttempt() != next || candidateGen == parentGen {
		t.Fatal("parent timeout affected successor")
	}
	steeringProviderFrame(a, `{"type":"response.created","sequence_number":0,"response":{"id":"resp_child","previous_response_id":"resp_parent","status":"in_progress"}}`)
	childGen := a.watchdog.activeTurnTimerGen
	a.handleTimeout(ResponsesWSEventTimeout{Reason: responsesWSActiveTurnTimeoutReason, AttemptID: next.AttemptID, TimeoutGeneration: candidateGen, UpstreamSessionGeneration: a.upstream.sessionGeneration, ChannelID: 17})
	if a.closing.closed.Load() || childGen == candidateGen {
		t.Fatal("candidate timeout affected bound child")
	}
	steeringProviderFrame(a, `{"type":"response.steer.failed","sequence_number":99,"steer":{"previous_response_id":"resp_parent"}}`)
	if a.watchdog.activeTurnTimerGen != childGen {
		t.Fatal("late parent receipt refreshed child timeout")
	}
	a.handleTimeout(ResponsesWSEventTimeout{Reason: responsesWSActiveTurnTimeoutReason, AttemptID: next.AttemptID, TimeoutGeneration: childGen, UpstreamSessionGeneration: a.upstream.sessionGeneration, ChannelID: 17})
	if !a.closing.closed.Load() {
		t.Fatal("current child timeout was ignored")
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
	parent := a.turns.active.attempt
	steeringProviderFrame(a, `{"type":"response.steer.failed","sequence_number":999,"steer":{"id":"unknown","previous_response_id":"resp_parent"}}`)
	steeringProviderFrame(a, `{"type":"response.steer.pending","sequence_number":1000,"steer":{"id":"unknown","previous_response_id":"resp_parent"},"reason":"future"}`)
	steeringProviderFrame(a, `{"type":"response.steer.accepted","sequence_number":1,"steer":{"id":"s1","previous_response_id":"resp_parent"}}`)
	steeringProviderFrame(a, `{"type":"response.completed","sequence_number":2,"response":{"id":"resp_parent","status":"completed","usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`)
	if a.closing.closed.Load() || !parent.QuotaFinalized || parent.Usage.PromptTokens != 7 || a.steering.awaiting != 0 {
		t.Fatal("diagnostic receipt corrupted parent evidence")
	}
}

func TestResponsesWSWorkflowStopKeepsBoundUsageDespiteDiagnosticSendID(t *testing.T) {
	a, _, conn := newSteeringTestActor(t, 1000)
	parent := a.turns.active.attempt
	payload := `{"type":"response.failed","sequence_number":1,"response":{"id":"resp_parent","status":"failed","error":{"code":"misalignment_policy_violation","message":"stop"},"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`
	a.handleProviderDownstream(ResponsesWSEventProviderDownstream{AttemptID: "diagnostic-last-send", UpstreamSessionGeneration: a.upstream.sessionGeneration, ChannelID: 17, Kind: ProviderDownstreamFrame, DetailOrigin: responsesws.RecvDetailOriginProviderFrame, Frame: responsesWSTestProviderTextFrame([]byte(payload))})
	if !a.closing.closed.Load() || !parent.QuotaFinalized || parent.Usage.PromptTokens != 7 {
		t.Fatal("workflow stop lost its legitimate usage")
	}
	if got, _ := conn.lastWrite.Load().(string); got != payload {
		t.Fatalf("workflow error changed: %s", got)
	}
}

func TestResponsesWSAttachedIdentityCannotOverrideWireResponse(t *testing.T) {
	a, _, _ := newSteeringTestActor(t, 1000)
	parent := a.turns.active.attempt
	a.handleProviderDownstream(ResponsesWSEventProviderDownstream{AttemptID: "diagnostic-last-send", ResponseID: "resp_parent", UpstreamSessionGeneration: a.upstream.sessionGeneration, ChannelID: 17, Kind: ProviderDownstreamFrame, DetailOrigin: responsesws.RecvDetailOriginProviderFrame, Frame: responsesWSTestProviderTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_other","status":"completed","usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`))})
	if !a.closing.closed.Load() || parent.Usage.PromptTokens != 0 || !parent.RolledBack {
		t.Fatal("conflicting raw response identity was charged to current owner")
	}
}

func TestResponsesWSSteeringNullSequenceIsNotRefundEvidence(t *testing.T) {
	a, session, conn := newSteeringTestActor(t, 1000)
	sendTestSteer(t, a, session, "resp_parent")
	next := a.steering.next
	payload := `{"type":"response.steer.failed","sequence_number":null,"steer":{"previous_response_id":"resp_parent"}}`
	steeringProviderFrame(a, payload)
	if got, _ := conn.lastWrite.Load().(string); got != payload {
		t.Fatal("unknown sequence shape did not pass through")
	}
	if a.closing.closed.Load() || a.steering.next != next || a.steering.awaiting != 1 || next.RolledBack {
		t.Fatal("null sequence was interpreted as a fresh failure")
	}
}
