package router

import (
	"strings"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/wsconn"
	"one-api/model"
)

func TestRealtimeRouteAutomaticSettlementStopsExhaustedPrincipal(t *testing.T) {
	for _, test := range []struct {
		name                  string
		userQuota, tokenQuota int
		unlimited             bool
	}{
		{name: "user", userQuota: 5, tokenQuota: 100},
		{name: "token", userQuota: 100, tokenQuota: 5},
		{name: "unlimited_token_still_limits_user", userQuota: 5, unlimited: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRealtimeRouteFixture(t, false)
			if err := fixture.db.Model(&model.User{}).Where("id = ?", fixture.userID).Update("quota", test.userQuota).Error; err != nil {
				t.Fatal(err)
			}
			if err := fixture.db.Model(&model.Token{}).Where("id = ?", fixture.userID).Updates(map[string]any{"remain_quota": test.tokenQuota, "unlimited_quota": test.unlimited}).Error; err != nil {
				t.Fatal(err)
			}
			client, err := fixture.dial(t)
			if err != nil {
				t.Fatal(err)
			}
			frames := realtimeRouteClientFrames(t, client)
			realtimeRouteReceive(t, frames)
			upstream := <-fixture.upstream
			t.Cleanup(func() { upstream.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_done"}) })
			if err := client.WriteMessage(wsconn.TextMessage, []byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`)); err != nil {
				t.Fatal(err)
			}
			realtimeRouteReceive(t, fixture.frames)
			for _, event := range []string{
				`{"type":"input_audio_buffer.committed","item_id":"exhausted-input"}`,
				`{"type":"response.done","response":{"id":"exhausted-response","status":"completed","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}`,
			} {
				if err := upstream.WriteMessage(wsconn.TextMessage, []byte(event)); err != nil {
					t.Fatal(err)
				}
				realtimeRouteReceive(t, frames)
			}
			denial := realtimeRouteReceive(t, frames)
			if !strings.Contains(string(denial), `"error"`) {
				t.Fatalf("missing post-settlement denial: %s", denial)
			}
			var user model.User
			var token model.Token
			if err := fixture.db.First(&user, fixture.userID).Error; err != nil {
				t.Fatal(err)
			}
			if err := fixture.db.First(&token, fixture.userID).Error; err != nil {
				t.Fatal(err)
			}
			if user.Quota != test.userQuota-7 || user.UsedQuota != 7 {
				t.Fatalf("already performed work not settled once: %+v", user)
			}
			if !test.unlimited && token.RemainQuota != test.tokenQuota-7 {
				t.Fatalf("token balance=%d", token.RemainQuota)
			}
			_ = client.WriteMessage(wsconn.TextMessage, []byte(`{"type":"input_audio_buffer.append","audio":"BBBB"}`))
			select {
			case event := <-fixture.frames:
				t.Fatalf("exhausted principal continued upstream work: %s", event)
			case <-time.After(30 * time.Millisecond):
			}
		})
	}
}

func TestRealtimeRouteRevocationSettlesAlreadyAdmittedManualWork(t *testing.T) {
	fixture := newRealtimeRouteFixture(t, false)
	client, err := fixture.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	frames := realtimeRouteClientFrames(t, client)
	realtimeRouteReceive(t, frames)
	upstream := <-fixture.upstream
	t.Cleanup(func() { upstream.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_done"}) })
	if err := client.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.create","response":{"max_output_tokens":100}}`)); err != nil {
		t.Fatal(err)
	}
	realtimeRouteReceive(t, fixture.frames)
	if err := fixture.db.Model(&model.Token{}).Where("id = ?", fixture.userID).Update("status", config.TokenStatusDisabled).Error; err != nil {
		t.Fatal(err)
	}
	if err := upstream.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.done","response":{"id":"revoked-response","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)); err != nil {
		t.Fatal(err)
	}
	if payload := realtimeRouteReceive(t, frames); !strings.Contains(string(payload), `"response.done"`) {
		t.Fatalf("terminal evidence discarded: %s", payload)
	}
	if payload := realtimeRouteReceive(t, frames); !strings.Contains(string(payload), "invalid_api_key") {
		t.Fatalf("revocation not surfaced: %s", payload)
	}
	var user model.User
	if err := fixture.db.First(&user, fixture.userID).Error; err != nil {
		t.Fatal(err)
	}
	if user.UsedQuota != 2 || user.Quota != 99998 {
		t.Fatalf("revocation discarded performed usage: used=%d balance=%d", user.UsedQuota, user.Quota)
	}
}
