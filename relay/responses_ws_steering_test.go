package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	actor.turns.active = responsesWSActiveTurn{attempt: parent, channelID: 17}
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
		if string(req.Frame.Payload()) != payload || req.AttemptID != "transport-parent" {
			t.Fatalf("steer altered: %+v", req)
		}
	case <-time.After(time.Second):
		t.Fatalf("steer not forwarded, state=%+v", actor.steering)
	}
}

func TestResponsesWSSteeringAutomaticSuccessorsSettleIndependently(t *testing.T) {
	actor, session, _ := newSteeringTestActor(t, 1000)
	parent := actor.turns.active.attempt
	sendTestSteer(t, actor, session, "resp_parent")
	next := actor.steering.next
	if next == nil || !next.QuotaPreconsumed || next.AttemptID == parent.AttemptID {
		t.Fatal("successor must be admitted and reserved before steering")
	}
	steeringProviderFrame(actor, `{"type":"response.steer.accepted","sequence_number":1,"steer":{"id":"steer_1","previous_response_id":"resp_parent"}}`)
	steeringProviderFrame(actor, `{"type":"response.incomplete","sequence_number":2,"response":{"id":"resp_parent","status":"incomplete","incomplete_details":{"reason":"steered"},"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`)
	if !parent.QuotaFinalized || actor.turns.active.attempt != nil || actor.steering.next != next {
		t.Fatal("parent must settle and release while the successor reservation retains its barrier")
	}
	steeringProviderFrame(actor, `{"type":"response.created","sequence_number":0,"response":{"id":"resp_next","previous_response_id":"resp_parent","status":"in_progress"}}`)
	if actor.closing.closed.Load() || actor.turns.active.attempt != next || next.SeenProviderResponseID != "resp_next" {
		t.Fatalf("successor not adopted: %+v", actor.turns.active)
	}
	// 自动续接还能继续接受 steering，transport 不换 ID，也不发送伪造 create。
	sendTestSteer(t, actor, session, "resp_next")
	last := actor.steering.next
	steeringProviderFrame(actor, `{"type":"response.steer.accepted","sequence_number":1,"steer":{"id":"steer_2","previous_response_id":"resp_next"}}`)
	steeringProviderFrame(actor, `{"type":"response.completed","sequence_number":2,"response":{"id":"resp_next","status":"completed","usage":{"input_tokens":8,"output_tokens":3,"total_tokens":11}}}`)
	steeringProviderFrame(actor, `{"type":"response.created","sequence_number":0,"response":{"id":"resp_last","previous_response_id":"resp_next","status":"in_progress"}}`)
	steeringProviderFrame(actor, `{"type":"response.completed","sequence_number":1,"response":{"id":"resp_last","status":"completed","usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}}}`)
	if actor.closing.closed.Load() || actor.state != responsesWSStateIdle || !next.QuotaFinalized || !last.QuotaFinalized {
		t.Fatal("chain did not finish independently")
	}
	if parent.Usage.PromptTokens != 5 || next.Usage.PromptTokens != 8 || last.Usage.PromptTokens != 9 {
		t.Fatal("usage crossed response boundaries")
	}
	select {
	case req := <-session.requests:
		t.Fatalf("unexpected automatic replay: %+v", req)
	default:
	}
}

func TestResponsesWSSteeringPendingAndFailedReleaseUnusedReserve(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed", true: "pending"}[pending], func(t *testing.T) {
			actor, session, conn := newSteeringTestActor(t, 1000)
			sendTestSteer(t, actor, session, "resp_parent")
			next := actor.steering.next
			steeringProviderFrame(actor, `{"type":"response.steer.accepted","sequence_number":1,"steer":{"id":"s1","previous_response_id":"resp_parent"}}`)
			steeringProviderFrame(actor, `{"type":"response.completed","sequence_number":2,"response":{"id":"resp_parent","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7},"output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}]}}`)
			kind := "failed"
			if pending {
				kind = "pending"
			}
			payload := `{"type":"response.steer.` + kind + `","sequence_number":3,"steer":{"id":"s1","previous_response_id":"resp_parent"},"reason":"waiting_for_required_input","required_input":[{"type":"function_call_output","call_id":"call_1"}],"future":true}`
			steeringProviderFrame(actor, payload)
			if actor.closing.closed.Load() || actor.state != responsesWSStateIdle || !next.RolledBack || actor.steering.next != nil {
				t.Fatalf("unused reserve not released: %+v", actor.steering)
			}
			if got, _ := conn.lastWrite.Load().(string); got != payload {
				t.Fatalf("ack changed: %s", got)
			}
			if pending {
				actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.create","model":"gpt-5","store":false,"previous_response_id":"resp_parent","input":[{"type":"function_call_output","call_id":"call_1","output":"saved result"}]}`)))
				select {
				case req := <-session.requests:
					if strings.Contains(string(req.Frame.Payload()), "smaller scope") || !strings.Contains(string(req.Frame.Payload()), "saved result") {
						t.Fatalf("explicit continuation mutated: %s", req.Frame.Payload())
					}
				case <-time.After(time.Second):
					t.Fatal("explicit tool continuation blocked")
				}
			}
		})
	}
}

func TestResponsesWSSteeringRejectsQuotaAndForeignOwnerBeforeSend(t *testing.T) {
	for _, quota := range []int{100, 1000} {
		t.Run(map[int]string{100: "quota", 1000: "foreign_owner"}[quota], func(t *testing.T) {
			actor, session, _ := newSteeringTestActor(t, 1000)
			if quota == 100 {
				if err := model.DB.Model(&model.User{}).Where("id = 1").Update("quota", 0).Error; err != nil {
					t.Fatal(err)
				}
			}
			target := "resp_parent"
			if quota == 1000 {
				target = "resp_other_user"
			}
			actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.steer","previous_response_id":"` + target + `","input":"hi"}`)))
			select {
			case req := <-session.requests:
				t.Fatalf("unauthorized work sent: %+v", req)
			default:
			}
			if actor.steering.next != nil {
				t.Fatal("rejected steering leaked reservation")
			}
		})
	}
}

func TestResponsesWSSteeringAmbiguousSendNeverReplays(t *testing.T) {
	actor, session, _ := newSteeringTestActor(t, 1000)
	sendTestSteer(t, actor, session, "resp_parent")
	actor.handleSendResult(ResponsesWSEventSendResult{Completion: &responsesWSSendCompletion{}, AttemptID: actor.steering.next.AttemptID, ResponseID: "resp_parent", UpstreamSessionGeneration: actor.upstream.sessionGeneration, SelectedChannelID: 17, Purpose: ResponsesWSSendPurposeResponseSteer, TransportResult: responsesws.ResponsesWSTransportSendResult{Status: responsesws.ResponsesWSTransportSendAmbiguous, Err: errors.New("write outcome unknown")}})
	if !actor.closing.closed.Load() {
		t.Fatal("ambiguous steering must close")
	}
	select {
	case req := <-session.requests:
		t.Fatalf("steering replayed: %+v", req)
	default:
	}
}

func TestResponsesWSSteeringCoalescesBoundedSubmissionsAndKeepsFailedWire(t *testing.T) {
	actor, session, conn := newSteeringTestActor(t, 1000)
	sendTestSteer(t, actor, session, "resp_parent")
	next := actor.steering.next
	for i := 1; i < responsesWSInjectMaxPending; i++ {
		sendTestSteer(t, actor, session, "resp_parent")
	}
	if actor.steering.next != next {
		t.Fatal("one successor was reserved more than once")
	}
	actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.steer","previous_response_id":"resp_parent","input":"over limit"}`)))
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "responses_ws_steer_queue_full") {
		t.Fatalf("missing capacity rejection: %s", got)
	}
	select {
	case req := <-session.requests:
		t.Fatalf("over-limit steer sent: %+v", req)
	default:
	}
	for i := 0; i < responsesWSInjectMaxPending; i++ {
		payload := fmt.Sprintf(`{"type":"response.steer.failed","sequence_number":%d,"steer":{"previous_response_id":"resp_parent","input":"original input"},"error":{"code":"steering_not_supported","message":"unsupported mode"}}`, i+1)
		steeringProviderFrame(actor, payload)
		if got, _ := conn.lastWrite.Load().(string); got != payload {
			t.Fatalf("upstream failure changed: %s", got)
		}
	}
	if !next.RolledBack || actor.steering.next != nil || actor.closing.closed.Load() {
		t.Fatal("rejected batch retained reservation or closed current response")
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

func TestResponsesWSMisalignmentStopsQueuedWork(t *testing.T) {
	actor, session, conn := newSteeringTestActor(t, 1000)
	actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.create","model":"gpt-5","store":false,"input":"must not run"}`)))
	payload := `{"type":"error","error":{"type":"invalid_request_error","code":"misalignment_policy_violation","message":"review this workflow"}}`
	steeringProviderFrame(actor, payload)
	if !actor.closing.closed.Load() {
		t.Fatal("workflow stop must close session")
	}
	got, _ := conn.lastWrite.Load().(string)
	var wire map[string]any
	if json.Unmarshal([]byte(got), &wire) != nil || !strings.Contains(got, "misalignment_policy_violation") {
		t.Fatalf("policy error lost: %s", got)
	}
	select {
	case req := <-session.requests:
		t.Fatalf("queued work sent after stop: %+v", req)
	default:
	}
}

func TestResponsesWSSteeringPolicyStopAfterParentTerminalPreservesError(t *testing.T) {
	for _, control := range []bool{false, true} {
		t.Run(fmt.Sprint(control), func(t *testing.T) {
			actor, session, conn := newSteeringTestActor(t, 1000)
			sendTestSteer(t, actor, session, "resp_parent")
			next := actor.steering.next
			steeringProviderFrame(actor, `{"type":"response.steer.accepted","sequence_number":1,"steer":{"id":"s1","previous_response_id":"resp_parent"}}`)
			steeringProviderFrame(actor, `{"type":"response.completed","sequence_number":2,"response":{"id":"resp_parent","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`)
			payload := `{"type":"error","error":{"type":"invalid_request_error","code":"misalignment_policy_violation","message":"review required"}}`
			if control {
				payload = `{"type":"response.steer.failed","sequence_number":3,"steer":{"id":"s1","previous_response_id":"resp_parent"},"error":{"code":"misalignment_policy_violation","message":"review required"}}`
			}
			steeringProviderFrame(actor, payload)
			if got, _ := conn.lastWrite.Load().(string); got != payload {
				t.Fatalf("stop error lost after parent terminal: %s", got)
			}
			if !actor.closing.closed.Load() || !next.RolledBack {
				t.Fatal("policy stop did not close and release unused reserve")
			}
		})
	}
}
