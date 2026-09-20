package relay

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unsafe"

	"one-api/common/responsesws"
	"one-api/model"
	"one-api/types"
)

func addObservedTestWork(t *testing.T, actor *ResponsesWSSessionActor, id, lane, parent string) *ResponsesWSTurnAttempt {
	t.Helper()
	attempt := preparePreconsumedResponsesWSTestAttempt(t, actor.Context())
	attempt.AttemptID = id
	attempt.RequireStoredOwner = false
	raw := fmt.Sprintf(`{"type":"response.create","model":"gpt-5","store":false,"stream_id":%q}`, lane)
	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	attempt.RequestFrame = frame
	if err := actor.observation.add(attempt, lane, parent); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { actor.finishAllObservedWorks() })
	return attempt
}
func newObservedTestActor(t *testing.T) (*ResponsesWSSessionActor, *responsesWSFakeUserConn) {
	t.Helper()
	ctx := setupResponsesWSQuotaFixture(t, 100000)
	configureResponsesWSTokenPricingFloor(t, 100)
	a := NewResponsesWSSessionActor(ctx)
	conn := &responsesWSFakeUserConn{}
	a.SetPump(NewResponsesWSIOPump(conn, a))
	a.upstream.channelID = 17
	a.upstream.sessionGeneration = "observation-session"
	return a, conn
}
func observedProviderFrame(t *testing.T, a *ResponsesWSSessionActor, conn *responsesWSFakeUserConn, raw string) {
	t.Helper()
	a.handleProviderDownstream(ResponsesWSEventProviderDownstream{UpstreamSessionGeneration: a.upstream.sessionGeneration, ChannelID: 17, Frame: responsesWSTestProviderTextFrame([]byte(raw)), DetailOrigin: responsesws.RecvDetailOriginProviderFrame})
	if got, _ := conn.lastWrite.Load().(string); got != raw {
		t.Fatalf("raw delivery changed: %s != %s", got, raw)
	}
	if a.closing.closed.Load() {
		t.Fatal("observation unexpectedly closed connection")
	}
}
func TestResponsesWSParallelLanesInterleavedTerminals(t *testing.T) {
	a, conn := newObservedTestActor(t)
	first := addObservedTestWork(t, a, "work-first", "first", "")
	second := addObservedTestWork(t, a, "work-second", "second", "")
	fallback := addObservedTestWork(t, a, "work-default", "", "")
	observedProviderFrame(t, a, conn, `{"type":"response.created","stream_id":"second","response":{"id":"resp_second"}}`)
	observedProviderFrame(t, a, conn, `{"type":"response.created","response":{"id":"resp_default"}}`)
	observedProviderFrame(t, a, conn, `{"type":"response.created","stream_id":"first","response":{"id":"resp_first"}}`)
	for _, item := range []struct {
		id, lane string
		tokens   int
	}{{"resp_second", "second", 23}, {"resp_first", "first", 11}, {"resp_default", "", 7}} {
		observedProviderFrame(t, a, conn, fmt.Sprintf(`{"type":"response.completed","stream_id":%q,"response":{"id":%q,"status":"completed","usage":{"input_tokens":%d,"output_tokens":2,"total_tokens":%d}}}`, item.lane, item.id, item.tokens, item.tokens+2))
	}
	if first.Usage.PromptTokens != 11 || second.Usage.PromptTokens != 23 || fallback.Usage.PromptTokens != 7 {
		t.Fatal("usage crossed lanes")
	}
	if !first.QuotaFinalized || !second.QuotaFinalized || !fallback.QuotaFinalized || len(a.observation.works) != 0 {
		t.Fatal("work was not finalized independently")
	}
	before, _ := readResponsesWSQuotaFixture(t)
	observedProviderFrame(t, a, conn, `{"type":"response.completed","stream_id":"first","response":{"id":"resp_first","usage":{"input_tokens":999,"output_tokens":999,"total_tokens":1998}}}`)
	after, _ := readResponsesWSQuotaFixture(t)
	if before.Quota != after.Quota {
		t.Fatal("late duplicate charged again")
	}
}
func TestResponsesWSParallelAmbiguousSuccessorReleasesOnlyUnboundLane(t *testing.T) {
	a, conn := newObservedTestActor(t)
	active := addObservedTestWork(t, a, "active", "same", "")
	observedProviderFrame(t, a, conn, `{"type":"response.created","stream_id":"same","response":{"id":"resp_A"}}`)
	queued := addObservedTestWork(t, a, "queued", "same", "")
	successor := addObservedTestWork(t, a, "successor", "same", "resp_A")
	independent := addObservedTestWork(t, a, "independent", "other", "")
	observedProviderFrame(t, a, conn, `{"type":"response.created","stream_id":"same","response":{"id":"resp_S","previous_response_id":"resp_A"}}`)
	if !queued.RolledBack || !successor.RolledBack || active.RolledBack || independent.RolledBack {
		t.Fatal("ambiguity abandoned an independent work or retained the ambiguous reservations")
	}
	if len(a.observation.works) != 2 {
		t.Fatalf("abandonment did not release slots: %d", len(a.observation.works))
	}
	newer := addObservedTestWork(t, a, "newer", "same", "")
	observedProviderFrame(t, a, conn, `{"type":"response.created","stream_id":"same","response":{"id":"resp_B"}}`)
	if !newer.RolledBack || newer.SeenProviderResponseID != "" {
		t.Fatal("late created was shifted onto the new work")
	}
	observedProviderFrame(t, a, conn, `{"type":"response.completed","stream_id":"same","response":{"id":"resp_A","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`)
	observedProviderFrame(t, a, conn, `{"type":"response.created","stream_id":"other","response":{"id":"resp_other"}}`)
	observedProviderFrame(t, a, conn, `{"type":"response.completed","stream_id":"other","response":{"id":"resp_other","usage":{"input_tokens":8,"output_tokens":2,"total_tokens":10}}}`)
	if !active.QuotaFinalized || !independent.QuotaFinalized {
		t.Fatal("independent valid evidence lost")
	}
}
func TestResponsesWSParallelLaneErrorDoesNotConsumeAnotherLane(t *testing.T) {
	a, conn := newObservedTestActor(t)
	lost := addObservedTestWork(t, a, "lost", "bad", "")
	other := addObservedTestWork(t, a, "other", "good", "")
	observedProviderFrame(t, a, conn, `{"type":"error","stream_id":"bad","error":{"code":"future_rejection"},"extra":9007199254740993}`)
	if !lost.RolledBack || other.RolledBack || len(a.observation.works) != 1 {
		t.Fatal("lane-only error guessed the wrong candidate")
	}
	observedProviderFrame(t, a, conn, `{"type":"response.created","stream_id":"good","response":{"id":"resp_good"}}`)
	observedProviderFrame(t, a, conn, `{"type":"future_event","stream_id":"bad","value":[null,true]}`)
	if other.SeenProviderResponseID != "resp_good" {
		t.Fatal("good lane lost association")
	}
}
func TestResponsesWSParallelCapacityAndBoundedHistory(t *testing.T) {
	var observations responsesWSObservations
	for i := 0; i < responsesWSObservationLimit; i++ {
		if err := observations.add(&ResponsesWSTurnAttempt{AttemptID: fmt.Sprint(i)}, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := observations.add(&ResponsesWSTurnAttempt{AttemptID: "overflow"}, "", ""); err == nil {
		t.Fatal("actual work capacity not enforced")
	}
	observations.remove("0")
	if err := observations.add(&ResponsesWSTurnAttempt{AttemptID: "new"}, "", ""); err != nil {
		t.Fatal("released capacity unusable")
	}
	before := unsafe.Sizeof(observations.seen)
	for i := 0; i < 100000; i++ {
		observations.seen.add(fmt.Sprint(i))
		observations.unsafe.add(fmt.Sprint(i))
	}
	if unsafe.Sizeof(observations.seen) != before {
		t.Fatal("history grew")
	}
	if !observations.seen.contains("0") {
		t.Fatal("history forgot an old ID and could misbill it")
	}
}
func TestResponsesWSParallelOwnerTombstoneRemainsAuthorized(t *testing.T) {
	a, _ := newObservedTestActor(t)
	if err := model.DB.AutoMigrate(&model.ResponseOwner{}); err != nil {
		t.Fatal(err)
	}
	if err := persistStoredResponseOwner(a.Context(), "resp_deleted", 17); err != nil {
		t.Fatal(err)
	}
	if err := model.TombstoneResponseOwner(a.logContext(), "resp_deleted", 1); err != nil {
		t.Fatal(err)
	}
	if err := a.authorizeInjectTarget("resp_deleted", 17); err != nil {
		t.Fatalf("local tombstone simulated upstream unavailability: %v", err)
	}
	if err := a.authorizeInjectTarget("resp_deleted", 18); err == nil {
		t.Fatal("cross-channel target authorized")
	}
}
func TestResponsesWSParallelCreateFIFOWithoutClientQueue(t *testing.T) {
	a, _ := newObservedTestActor(t)
	first := addObservedTestWork(t, a, "first", "", "")
	second := addObservedTestWork(t, a, "second", "", "")
	parse := func(raw string) map[string]json.RawMessage {
		var object map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &object)
		return object
	}
	if got := a.bindObservedResponse(parse(`{"response":{"id":"a"}}`), "a"); got == nil || got.attempt != first {
		t.Fatal("first create not associated")
	}
	if got := a.bindObservedResponse(parse(`{"response":{"id":"b"}}`), "b"); got == nil || got.attempt != second {
		t.Fatal("second create not associated")
	}
}

func TestResponsesWSParallelUnidentifiedCreatedCannotShiftFIFO(t *testing.T) {
	for _, name := range []string{"missing_id", "seen_filter_match"} {
		t.Run(name, func(t *testing.T) {
			a, conn := newObservedTestActor(t)
			first := addObservedTestWork(t, a, "first", "lane", "")
			second := addObservedTestWork(t, a, "second", "lane", "")
			raw := `{"type":"response.created","stream_id":"lane","response":{}}`
			if name == "seen_filter_match" {
				a.observation.seen.add("collision")
				raw = `{"type":"response.created","stream_id":"lane","response":{"id":"collision"}}`
			}
			observedProviderFrame(t, a, conn, raw)
			observedProviderFrame(t, a, conn, `{"type":"response.created","stream_id":"lane","response":{"id":"next"}}`)
			if !first.RolledBack || !second.RolledBack || first.SeenProviderResponseID != "" || second.SeenProviderResponseID != "" {
				t.Fatal("unidentified created shifted FIFO billing")
			}
		})
	}
}

func TestResponsesWSParallelMissingCreatedTerminalCannotShiftFIFO(t *testing.T) {
	a, conn := newObservedTestActor(t)
	first := addObservedTestWork(t, a, "first", "lane", "")
	second := addObservedTestWork(t, a, "second", "lane", "")
	observedProviderFrame(t, a, conn, `{"type":"response.completed","stream_id":"lane","response":{"id":"unbound","usage":{"input_tokens":99,"output_tokens":1,"total_tokens":100}}}`)
	observedProviderFrame(t, a, conn, `{"type":"response.created","stream_id":"lane","response":{"id":"next"}}`)
	if !first.RolledBack || !second.RolledBack || first.SeenProviderResponseID != "" || second.SeenProviderResponseID != "" {
		t.Fatal("missing created shifted FIFO")
	}
}
func TestResponsesWSParallelErrorDiagnosticsHaveBoundedStorage(t *testing.T) {
	a, _ := newObservedTestActor(t)
	attempt := addObservedTestWork(t, a, "errors", "", "")
	attempt.SeenProviderResponseID = "response-errors"
	for i := 0; i < 10000; i++ {
		a.markProviderAPIErrorSeen(&types.OpenAIErrorWithStatusCode{OpenAIError: types.OpenAIError{Message: fmt.Sprintf("diagnostic %d", i)}}, "provider_frame", "response-errors")
	}
	if len(attempt.providerAPIErrorKeys) > responsesWSObservationLimit {
		t.Fatal("diagnostic deduplication grew without a bound")
	}
}

func TestResponsesWSParallelConflictingAccountingIDDoesNotReplaceResourceOwner(t *testing.T) {
	a, conn := newObservedTestActor(t)
	attempt := addObservedTestWork(t, a, "request", "", "")
	raw := `{"type":"response.created","response_id":"ambiguous-accounting-id","response":{"id":"actual-resource-id"}}`
	observedProviderFrame(t, a, conn, raw)
	if !attempt.RolledBack {
		t.Fatal("conflicting identity retained billing association")
	}
	owner, err := model.GetResponseOwner(a.logContext(), "actual-resource-id", 1)
	if err != nil || owner.ChannelID != 17 {
		t.Fatalf("actual resource lacked ownership before delivery: %+v %v", owner, err)
	}
	if _, err := model.GetResponseOwner(a.logContext(), "ambiguous-accounting-id", 1); err == nil {
		t.Fatal("unproved alias was granted resource ownership")
	}
}

func TestResponsesWSSteeringColdParentCannotBypassTokenModelRestriction(t *testing.T) {
	actor, session, conn := newSteeringTestActor(t, 1000)
	if err := persistStoredResponseOwner(actor.Context(), "resp_cold", 17); err != nil {
		t.Fatal(err)
	}
	ctx := actor.Context()
	ctx.Set("token_setting", &model.TokenSetting{Limits: model.LimitsConfig{LimitModelSetting: model.LimitModelSetting{Enabled: true, Models: []string{"gpt-5"}}}})
	actor.RefreshContext(ctx)
	actor.handleClientFrame(responsesWSTestClientTextFrame([]byte(`{"type":"response.steer","previous_response_id":"resp_cold","input":"continue"}`)))
	if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "responses_ws_parent_model_unknown") {
		t.Fatalf("model authorization was guessed from first frame: %s", got)
	}
	if len(actor.observation.works) != 1 {
		t.Fatal("unauthorized successor was reserved")
	}
	select {
	case <-session.requests:
		t.Fatal("model-restricted token sent unproved parent work")
	default:
	}
}

func TestResponsesWSOpaqueTargetAliasesDoNotAuthorize(t *testing.T) {
	a, session, conn := newSteeringTestActor(t, 1000)
	for _, target := range []string{" resp_parent ", "RESP_PARENT"} {
		for _, operation := range []string{"response.inject", "response.steer"} {
			field := "response_id"
			if operation == "response.steer" {
				field = "previous_response_id"
			}
			raw := fmt.Sprintf(`{"type":%q,%q:%q,"input":"hello"}`, operation, field, target)
			a.handleClientFrame(responsesWSTestClientTextFrame([]byte(raw)))
			if got, _ := conn.lastWrite.Load().(string); !strings.Contains(got, "response_not_found") {
				t.Fatalf("opaque target alias authorized: %s", got)
			}
			select {
			case request := <-session.requests:
				t.Fatalf("unauthorized alias reached upstream: %+v", request)
			default:
			}
		}
		if _, found := a.connectionLocalTurnAffinity(a.Context(), &types.OpenAIResponsesRequest{PreviousResponseID: target}); found {
			t.Fatalf("continuation alias inherited proof: %q", target)
		}
	}
}

func TestResponsesWSOpaqueResponseIdentityRoundTrip(t *testing.T) {
	a, conn := newObservedTestActor(t)
	attempt := addObservedTestWork(t, a, "opaque-work", "", "")
	exact := " Resp Raw "
	observedProviderFrame(t, a, conn, `{"type":"response.created","event_id":" event raw ","response":{"id":" Resp Raw "}}`)
	if attempt.SeenProviderResponseID != exact || attempt.ProviderAcceptedID != exact {
		t.Fatalf("provider identity normalized: %+v", attempt)
	}
	if a.authorizeInjectTarget(exact, 17) != nil {
		t.Fatal("actual raw ID lost its connection proof")
	}
	for _, alias := range []string{"Resp Raw", " resp raw "} {
		if a.authorizeInjectTarget(alias, 17) == nil {
			t.Fatalf("raw proof authorized alias %q", alias)
		}
	}
	observedProviderFrame(t, a, conn, `{"type":"response.completed","response":{"id":" resp raw ","usage":{"input_tokens":99,"output_tokens":1,"total_tokens":100}}}`)
	if attempt.QuotaFinalized || attempt.Usage.TotalTokens != 0 {
		t.Fatal("case variant consumed another response's usage")
	}
	observedProviderFrame(t, a, conn, `{"type":"response.completed","response":{"id":" Resp Raw ","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`)
	if !attempt.QuotaFinalized || attempt.Usage.TotalTokens != 5 {
		t.Fatalf("exact raw ID did not settle its work: %+v", attempt.Usage)
	}
}
