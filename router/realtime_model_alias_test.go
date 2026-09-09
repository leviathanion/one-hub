package router

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"one-api/common/wsconn"
	"one-api/model"
)

func TestRealtimeRouteAuthorizesPublicAliasAndPreservesBillingMapping(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		for _, originalPrice := range []bool{false, true} {
			t.Run(fmt.Sprintf("automatic=%v/original_price=%v", automatic, originalPrice), func(t *testing.T) {
				fixture := newRealtimeRouteFixture(t, false)
				mapped := "upstream-voice"
				if originalPrice {
					mapped = "+" + mapped
				}
				mapping, _ := json.Marshal(map[string]string{"voice-public": mapped})
				if err := fixture.db.Model(&model.Channel{}).Where("id > 0").Updates(map[string]any{"models": "voice-public", "model_mapping": string(mapping)}).Error; err != nil {
					t.Fatal(err)
				}
				if err := model.ChannelGroup.Load(); err != nil {
					t.Fatal(err)
				}
				setting := model.TokenSetting{Limits: model.LimitsConfig{LimitModelSetting: model.LimitModelSetting{Enabled: true, Models: []string{"voice-public"}}}}
				encoded, _ := json.Marshal(setting)
				if err := fixture.db.Model(&model.Token{}).Where("id = ?", fixture.userID).Update("setting", string(encoded)).Error; err != nil {
					t.Fatal(err)
				}
				for name, price := range map[string]float64{"voice-public": 3, "upstream-voice": 1} {
					if err := fixture.db.Create(&model.Price{Model: name, Type: model.TokensPriceType, Input: price, Output: price}).Error; err != nil {
						t.Fatal(err)
					}
				}
				model.PricingInstance = &model.Pricing{Prices: make(map[string]*model.Price)}
				if err := model.PricingInstance.Init(); err != nil {
					t.Fatal(err)
				}
				fixture.url = strings.Replace(fixture.url, "model=gpt-realtime", "model=voice-public", 1)
				client, err := fixture.dial(t)
				if err != nil {
					t.Fatalf("public alias handshake: %v", err)
				}
				frames := realtimeRouteClientFrames(t, client)
				realtimeRouteReceive(t, frames)
				upstream := <-fixture.upstream
				t.Cleanup(func() { upstream.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "test_done"}) })
				if modelName := <-fixture.queryModels; modelName != "upstream-voice" {
					t.Fatalf("dial model=%q", modelName)
				}
				input := `{"type":"response.create","response":{"model":"voice-public","max_output_tokens":100}}`
				if automatic {
					input = `{"type":"input_audio_buffer.append","audio":"AAAA"}`
				}
				if err := client.WriteMessage(wsconn.TextMessage, []byte(input)); err != nil {
					t.Fatal(err)
				}
				forwarded := realtimeRouteReceive(t, fixture.frames)
				if !automatic && !strings.Contains(string(forwarded), `"model":"upstream-voice"`) {
					t.Fatalf("response model was not mapped: %s", forwarded)
				}
				if automatic {
					if err := upstream.WriteMessage(wsconn.TextMessage, []byte(`{"type":"input_audio_buffer.committed","item_id":"alias-input"}`)); err != nil {
						t.Fatal(err)
					}
					realtimeRouteReceive(t, frames)
				}
				if err := upstream.WriteMessage(wsconn.TextMessage, []byte(`{"type":"response.done","response":{"id":"alias-response","model":"upstream-voice","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)); err != nil {
					t.Fatal(err)
				}
				terminal := realtimeRouteReceive(t, frames)
				if !strings.Contains(string(terminal), `"response.done"`) || strings.Contains(string(terminal), `"error"`) {
					t.Fatalf("authorized turn failed: %s", terminal)
				}
				var user model.User
				if err := fixture.db.First(&user, fixture.userID).Error; err != nil {
					t.Fatal(err)
				}
				want := 2
				if originalPrice {
					want = 6
				}
				if user.UsedQuota != want || user.Quota != 100000-want {
					t.Fatalf("billing mapping lost: used=%d balance=%d want=%d", user.UsedQuota, user.Quota, want)
				}
			})
		}
	}
}
