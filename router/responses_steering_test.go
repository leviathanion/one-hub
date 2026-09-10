package router

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"one-api/common/config"
	ratelimit "one-api/common/limit"
	"one-api/common/wsconn"
	"one-api/model"
)

func TestResponsesSteeringRoutePreservesOwnershipAndPerResponseBilling(t *testing.T) {
	oldEncoders := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() { config.DisableTokenEncoders = oldEncoders })
	fixture := newLongLivedRouteFixtureWithHTTP(t, false, true, func(http.ResponseWriter, *http.Request) { t.Error("unexpected HTTP fallback") })
	model.GlobalUserGroupRatio.Lock()
	model.GlobalUserGroupRatio.APILimiter["realtime-route"] = ratelimit.NewMemoryLimiter(100, 100, time.Minute, false)
	model.GlobalUserGroupRatio.Unlock()
	client, err := fixture.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	frames := realtimeRouteClientFrames(t, client)
	if err := client.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.create","model":"gpt-realtime","input":[],"store":true}`)); err != nil {
		t.Fatal(err)
	}
	if got := realtimeRouteReceive(t, fixture.frames); !strings.Contains(string(got), "response.create") {
		t.Fatalf("create missing: %s", got)
	}
	upstream := <-fixture.upstream
	created := `{"type":"response.created","sequence_number":0,"response":{"id":"resp_route_parent","model":"gpt-realtime","status":"in_progress"}}`
	if err := upstream.WriteMessage(wsconn.TextMessage, []byte(created)); err != nil {
		t.Fatal(err)
	}
	if got := realtimeRouteReceive(t, frames); string(got) != created {
		t.Fatalf("created changed: %s", got)
	}
	steer := `{"type":"response.steer","previous_response_id":"resp_route_parent","input":"keep scope small"}`
	if err := client.WriteMessage(wsconn.TextMessage, []byte(steer)); err != nil {
		t.Fatal(err)
	}
	if got := realtimeRouteReceive(t, fixture.frames); string(got) != steer {
		t.Fatalf("steering changed: %s", got)
	}
	events := []string{
		`{"type":"response.steer.accepted","sequence_number":1,"steer":{"id":"steer_route","previous_response_id":"resp_route_parent"}}`,
		`{"type":"response.incomplete","sequence_number":2,"response":{"id":"resp_route_parent","model":"gpt-realtime","status":"incomplete","incomplete_details":{"reason":"steered"},"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		`{"type":"response.created","sequence_number":0,"response":{"id":"resp_route_next","previous_response_id":"resp_route_parent","model":"gpt-realtime","status":"in_progress"}}`,
		`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_route_next","model":"gpt-realtime","status":"completed","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}`,
	}
	for _, event := range events {
		if err := upstream.WriteMessage(wsconn.TextMessage, []byte(event)); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range events {
		if got := realtimeRouteReceive(t, frames); string(got) != event {
			t.Fatalf("event lost/changed: want=%s got=%s", event, got)
		}
	}
	fixture.awaitQuota(t, 10)
	for _, id := range []string{"resp_route_parent", "resp_route_next"} {
		owner, err := model.GetResponseOwner(t.Context(), id, fixture.userID)
		if err != nil || owner.UserID != fixture.userID || owner.ChannelID == 0 {
			t.Fatalf("owner missing for %s: %+v %v", id, owner, err)
		}
	}
	select {
	case got := <-fixture.frames:
		t.Fatalf("unexpected synthesized create: %s", got)
	default:
	}
}
