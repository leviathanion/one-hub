package relay

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/responsesws"
	"one-api/model"
	"one-api/types"
)

func newSteeringTestActor(t *testing.T, quota int) (*ResponsesWSSessionActor, *responsesWSCaptureSendSession, *responsesWSFakeUserConn) {
	t.Helper()
	ctx, parent := setupPreconsumedResponsesWSActorAttempt(t, quota, "transport-parent")
	installResponsesWSTestAPILimiter(t, 100)
	channel := &model.Channel{Id: 17, Type: config.ChannelTypeOpenAI, Models: "gpt-5", Status: config.ChannelStatusEnabled, PreCost: config.PreContNotAll}
	ctx.Set("responses_ws_selected_channel", channel)
	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","store":false,"reasoning":{"effort":"low"},"input":"hello","future":{"keep":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	parent.RequestFrame = frame
	parent.RequireStoredOwner = false
	parent.SeenProviderResponseID = "resp_parent"
	parent.SelectedChannelID = 17
	parent.StoredOwnerPersisted = true
	session := &responsesWSCaptureSendSession{requests: make(chan responsesws.SendRequest, 16)}
	conn := &responsesWSFakeUserConn{reads: make(chan responsesWSReadResult, 1)}
	actor := NewResponsesWSSessionActor(ctx)
	actor.SetPump(NewResponsesWSIOPump(conn, actor))
	actor.AttachUpstreamSession(session, 17)
	actor.turns.opening.firstFrame = frame
	if err := actor.observation.add(parent, "", ""); err != nil {
		t.Fatal(err)
	}
	actor.state = responsesWSStateInFlight
	actor.rememberConnectionLocalEphemeralResponseID("resp_parent")
	t.Cleanup(func() { actor.close("test_cleanup"); actor.waitStartedGoroutines() })
	return actor, session, conn
}

func steeringProviderFrame(actor *ResponsesWSSessionActor, payload string) {
	actor.handleProviderDownstream(ResponsesWSEventProviderDownstream{
		AttemptID: "transport-parent", UpstreamSessionGeneration: actor.upstream.sessionGeneration,
		ChannelID: 17, Kind: ProviderDownstreamFrame, Frame: responsesWSTestProviderTextFrame([]byte(payload)),
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
	})
}

func sendTestSteer(t *testing.T, actor *ResponsesWSSessionActor, session *responsesWSCaptureSendSession, target string) {
	t.Helper()
	payload := `{"type":"response.steer","previous_response_id":"` + target + `","input":[{"role":"user","content":[{"type":"input_text","text":"smaller scope","future":9007199254740993}]}],"future_envelope":true}`
	actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(payload)))
	select {
	case req := <-session.requests:
		if string(req.Frame.Payload()) != payload || req.AttemptID == "transport-parent" || observedSteeringCandidate(actor, target) == nil || req.AttemptID != observedSteeringCandidate(actor, target).AttemptID {
			t.Fatalf("steer altered: %+v", req)
		}
	case <-time.After(time.Second):
		t.Fatalf("steer not forwarded, work count=%d", len(actor.observation.works))
	}
}

func observedSteeringCandidate(a *ResponsesWSSessionActor, parent string) *ResponsesWSTurnAttempt {
	for _, work := range a.observation.works {
		if work.successorParent == parent && work.attempt.SeenProviderResponseID == "" {
			return work.attempt
		}
	}
	return nil
}

func observedParentForTest(t *testing.T, a *ResponsesWSSessionActor) *ResponsesWSTurnAttempt {
	t.Helper()
	work := a.observation.byResponse("resp_parent")
	if work == nil {
		t.Fatal("parent observation missing")
	}
	return work.attempt
}

func TestResponsesWSSteeringAutomaticSuccessorsSettleIndependently(t *testing.T) {
	actor, session, _ := newSteeringTestActor(t, 1000)
	parent := observedParentForTest(t, actor)
	sendTestSteer(t, actor, session, "resp_parent")
	next := observedSteeringCandidate(actor, "resp_parent")
	if next == nil || !next.QuotaPreconsumed || next.AttemptID == parent.AttemptID {
		t.Fatal("successor must be admitted and reserved before steering")
	}
	steeringProviderFrame(actor, `{"type":"response.steer.accepted","sequence_number":1,"steer":{"id":"steer_1","previous_response_id":"resp_parent"}}`)
	steeringProviderFrame(actor, `{"type":"response.incomplete","sequence_number":2,"response":{"id":"resp_parent","status":"incomplete","incomplete_details":{"reason":"steered"},"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`)
	if !parent.QuotaFinalized || actor.observation.byResponse("resp_parent") != nil || observedSteeringCandidate(actor, "resp_parent") != next {
		t.Fatal("parent settlement must not consume successor reservation")
	}
	steeringProviderFrame(actor, `{"type":"response.created","sequence_number":0,"response":{"id":"resp_next","previous_response_id":"resp_parent","status":"in_progress"}}`)
	if actor.closing.closed.Load() || actor.observation.byResponse("resp_next") == nil || next.SeenProviderResponseID != "resp_next" {
		t.Fatal("successor not associated with its own reservation")
	}
	sendTestSteer(t, actor, session, "resp_next")
	last := observedSteeringCandidate(actor, "resp_next")
	steeringProviderFrame(actor, `{"type":"response.completed","sequence_number":2,"response":{"id":"resp_next","status":"completed","usage":{"input_tokens":8,"output_tokens":3,"total_tokens":11}}}`)
	steeringProviderFrame(actor, `{"type":"response.created","sequence_number":0,"response":{"id":"resp_last","previous_response_id":"resp_next","status":"in_progress"}}`)
	steeringProviderFrame(actor, `{"type":"response.completed","sequence_number":1,"response":{"id":"resp_last","status":"completed","usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}}}`)
	if actor.closing.closed.Load() || len(actor.observation.works) != 0 || !next.QuotaFinalized || !last.QuotaFinalized {
		t.Fatal("chain did not finish independently")
	}
	if parent.Usage.PromptTokens != 5 || next.Usage.PromptTokens != 8 || last.Usage.PromptTokens != 9 {
		t.Fatal("usage crossed response boundaries")
	}
	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 969 || token.RemainQuota != 969 {
		t.Fatalf("incorrect independent settlements: user=%d token=%d", user.Quota, token.RemainQuota)
	}
	select {
	case req := <-session.requests:
		t.Fatalf("unexpected automatic replay: %+v", req)
	default:
	}
}

func TestResponsesWSSteeringRejectsQuotaAndForeignOwnerBeforeSend(t *testing.T) {
	for _, reason := range []string{"quota", "foreign_owner"} {
		t.Run(reason, func(t *testing.T) {
			actor, session, _ := newSteeringTestActor(t, 1000)
			if reason == "quota" {
				if err := model.DB.Model(&model.User{}).Where("id = 1").Update("quota", 0).Error; err != nil {
					t.Fatal(err)
				}
			}
			target := "resp_parent"
			if reason == "foreign_owner" {
				target = "resp_other_user"
			}
			actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.steer","previous_response_id":"` + target + `","input":"hi"}`)))
			select {
			case req := <-session.requests:
				t.Fatalf("unauthorized work sent: %+v", req)
			default:
			}
			if observedSteeringCandidate(actor, target) != nil || len(actor.observation.works) != 1 {
				t.Fatal("rejected steering leaked reservation")
			}
		})
	}
}

func TestResponsesWSSteeringAmbiguousSendNeverReplays(t *testing.T) {
	actor, session, _ := newSteeringTestActor(t, 1000)
	sendTestSteer(t, actor, session, "resp_parent")
	next := observedSteeringCandidate(actor, "resp_parent")
	actor.handleSendResult(ResponsesWSEventSendResult{Completion: &responsesWSSendCompletion{}, AttemptID: next.AttemptID, ResponseID: "resp_parent", UpstreamSessionGeneration: actor.upstream.sessionGeneration, SelectedChannelID: 17, Purpose: ResponsesWSSendPurposeResponseSteer, TransportResult: responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAmbiguous, Err: errors.New("write outcome unknown")}})
	if !actor.closing.closed.Load() || !next.RolledBack {
		t.Fatal("uncertain transport must stop and close local reservation")
	}
	select {
	case req := <-session.requests:
		t.Fatalf("steering replayed: %+v", req)
	default:
	}
}

func TestResponsesWSSteeringCoalescesSubmissionsWithoutBusinessBarrier(t *testing.T) {
	actor, session, conn := newSteeringTestActor(t, 1000)
	sendTestSteer(t, actor, session, "resp_parent")
	next := observedSteeringCandidate(actor, "resp_parent")
	for i := 0; i < 5; i++ {
		sendTestSteer(t, actor, session, "resp_parent")
	}
	if observedSteeringCandidate(actor, "resp_parent") != next || len(actor.observation.works) != 2 {
		t.Fatal("same successor was reserved more than once")
	}
	payload := `{"type":"response.steer.pending","steer":{"id":"s1","previous_response_id":"resp_parent"},"reason":"waiting_for_required_input","future":true}`
	steeringProviderFrame(actor, payload)
	if got, _ := conn.lastWrite.Load().(string); got != payload {
		t.Fatalf("upstream control changed: %s", got)
	}
	// 客户端无需等父终态或 pending 回执才发送工具结果。
	create := `{"type":"response.create","model":"gpt-5","store":false,"previous_response_id":"resp_parent","input":[{"type":"function_call_output","call_id":"call_1","output":"saved result"}]}`
	actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(create)))
	select {
	case req := <-session.requests:
		if string(req.Frame.Payload()) != create {
			t.Fatalf("explicit continuation changed: %s", req.Frame.Payload())
		}
	case <-time.After(time.Second):
		t.Fatal("explicit continuation blocked by successor observation")
	}
	actor.close("client_closed")
	if !next.RolledBack || len(actor.observation.works) != 0 {
		t.Fatal("unused successor reservation survived connection cleanup")
	}
}

func TestMisalignmentPolicyViolationNeverRetries(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		err := &types.OpenAIErrorWithStatusCode{StatusCode: status, OpenAIError: types.OpenAIError{Type: "invalid_request_error", Code: "misalignment_policy_violation"}}
		if shouldRetry(newRelayTestContext(nil), err, config.ChannelTypeOpenAI) {
			t.Fatalf("policy stop retried with status %d", status)
		}
	}
}

func TestAstraCrossProtocolRejectsUnrepresentableFeatures(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-6-astra","input":"hi","tools":[{"type":"function","name":"lookup","async":true}]}`,
		`{"model":"gpt-6-astra","input":[{"type":"configuration_update","reasoning":{"effort":"high"}},{"role":"user","content":"hi"}]}`,
	} {
		var request types.OpenAIResponsesRequest
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &request); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(body), &fields); err != nil {
			t.Fatal(err)
		}
		if err := validateResponsesToChatRepresentability(&request, fields); err == nil {
			t.Fatalf("lossy conversion accepted: %s", body)
		}
	}
}

func TestResponsesWSSteeringSuccessorDoesNotWaitForParentTerminal(t *testing.T) {
	a, session, conn := newSteeringTestActor(t, 1000)
	parent := observedParentForTest(t, a)
	sendTestSteer(t, a, session, "resp_parent")
	child := observedSteeringCandidate(a, "resp_parent")
	created := `{"type":"response.created","response":{"id":"resp_early_child","previous_response_id":"resp_parent","future":true}}`
	steeringProviderFrame(a, created)
	if got, _ := conn.lastWrite.Load().(string); got != created {
		t.Fatalf("created was held behind parent terminal: %s", got)
	}
	if child.SeenProviderResponseID != "resp_early_child" || parent.QuotaFinalized || a.closing.closed.Load() {
		t.Fatal("successor association imposed parent business state")
	}
	steeringProviderFrame(a, `{"type":"response.completed","sequence_number":10,"response":{"id":"resp_early_child","usage":{"input_tokens":8,"output_tokens":3,"total_tokens":11}}}`)
	steeringProviderFrame(a, `{"type":"response.completed","sequence_number":1,"response":{"id":"resp_parent","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`)
	if !child.QuotaFinalized || !parent.QuotaFinalized || child.Usage.PromptTokens != 8 || parent.Usage.PromptTokens != 5 {
		t.Fatal("out-of-order distinct response terminals crossed billing owners")
	}
	user, token := readResponsesWSQuotaFixture(t)
	if user.Quota != 982 || token.RemainQuota != 982 {
		t.Fatalf("incorrect parent/successor settlements: user=%d token=%d", user.Quota, token.RemainQuota)
	}
}
