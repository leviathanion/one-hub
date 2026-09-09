package router

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/wsconn"
	"one-api/model"
)

func TestFixI007_LongLivedRoutesKeepCurrentAuthorization(t *testing.T) {
	for _, protocol := range []string{"realtime", "responses"} {
		for _, source := range []string{"primary", "backup", "admin"} {
			t.Run(protocol+"/"+source, func(t *testing.T) {
				responses := protocol == "responses"
				fixture := newLongLivedRouteFixture(t, false, responses)
				sqlDB, err := fixture.db.DB()
				if err != nil {
					t.Fatal(err)
				}
				sqlDB.SetMaxOpenConns(1)
				oldEncoders := config.DisableTokenEncoders
				config.DisableTokenEncoders = true
				t.Cleanup(func() { config.DisableTokenEncoders = oldEncoders })
				if err := fixture.db.Model(&model.UserGroup{}).Where("symbol = ?", "realtime-route").Update("api_rate", 100).Error; err != nil {
					t.Fatal(err)
				}
				ratio := 1
				if source != "primary" {
					enabled := true
					if err := fixture.db.Create(&model.UserGroup{Symbol: "route-backup", Ratio: 2, APIRate: 100, Enable: &enabled, Public: true}).Error; err != nil {
						t.Fatal(err)
					}
					if err := fixture.db.Model(&model.Channel{}).Where("1 = 1").Update("group", "route-backup").Error; err != nil {
						t.Fatal(err)
					}
					if source == "backup" {
						ratio = 2
						if err := fixture.db.Model(&model.Token{}).Where("id = ?", fixture.userID).Update("backup_group", "route-backup").Error; err != nil {
							t.Fatal(err)
						}
					} else {
						if err := fixture.db.Model(&model.User{}).Where("id = ?", fixture.userID).Update("role", config.RoleAdminUser).Error; err != nil {
							t.Fatal(err)
						}
						var channel model.Channel
						if err := fixture.db.First(&channel).Error; err != nil {
							t.Fatal(err)
						}
						fixture.token += fmt.Sprintf("#%d", channel.Id)
					}
				}
				if _, err := model.BumpPublicationVersionCAS(context.Background(), fixture.db, model.PublicationOwnerUserGroup, 1); err != nil {
					t.Fatal(err)
				}
				if err := model.GlobalUserGroupRatio.Load(); err != nil {
					t.Fatal(err)
				}
				if err := model.ChannelGroup.Load(); err != nil {
					t.Fatal(err)
				}
				client, err := fixture.dial(t)
				if err != nil {
					t.Fatalf("合法路由升级失败: %v（上游连接%d次）", err, fixture.dials.Load())
				}
				frames := realtimeRouteClientFrames(t, client)
				if !responses {
					realtimeRouteReceive(t, frames)
				}
				create := `{"type":"response.create","response":{"max_output_tokens":100}}`
				if responses {
					create = `{"type":"response.create","model":"gpt-realtime","input":"hello","store":false,"max_output_tokens":100}`
				}
				var upstream *wsconn.ManagedConn
				for turn := 0; turn < 2; turn++ {
					if err := client.WriteMessage(wsconn.TextMessage, []byte(create)); err != nil {
						t.Fatal(err)
					}
					select {
					case <-fixture.frames:
					case payload := <-frames:
						t.Fatalf("合法第%d轮未发送上游: %s", turn+1, payload)
					case <-time.After(3 * time.Second):
						t.Fatalf("合法工作未到达上游，上游连接%d次", fixture.dials.Load())
					}
					if upstream == nil {
						upstream = <-fixture.upstream
					}
					terminalType := "response.done"
					if responses {
						terminalType = "response.completed"
						created := fmt.Sprintf(`{"type":"response.created","sequence_number":%d,"response":{"id":"route-response-%d","model":"gpt-realtime","status":"in_progress"}}`, turn*2+1, turn)
						if err := upstream.WriteMessage(wsconn.TextMessage, []byte(created)); err != nil {
							t.Fatal(err)
						}
						realtimeRouteReceive(t, frames)
					}
					terminal := fmt.Sprintf(`{"type":%q,"sequence_number":%d,"response":{"id":"route-response-%d","model":"gpt-realtime","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`, terminalType, turn*2+2, turn)
					if err := upstream.WriteMessage(wsconn.TextMessage, []byte(terminal)); err != nil {
						t.Fatal(err)
					}
					if payload := realtimeRouteReceive(t, frames); !strings.Contains(string(payload), terminalType) {
						t.Fatalf("终态未交付: %s", payload)
					}
					want := (turn + 1) * 2 * ratio
					deadline := time.Now().Add(3 * time.Second)
					for {
						var user model.User
						var token model.Token
						if err := fixture.db.First(&user, fixture.userID).Error; err != nil {
							t.Fatal(err)
						}
						if err := fixture.db.First(&token, fixture.userID).Error; err != nil {
							t.Fatal(err)
						}
						if user.UsedQuota == want && user.Quota == 100000-want && token.RemainQuota == 100000-want {
							break
						}
						if time.Now().After(deadline) {
							t.Fatalf("实际分组计费错误: want=%d user=%d/%d token=%d", want, user.UsedQuota, user.Quota, token.RemainQuota)
						}
						time.Sleep(time.Millisecond)
					}
				}
				if source == "admin" {
					err = fixture.db.Model(&model.User{}).Where("id = ?", fixture.userID).Update("role", config.RoleCommonUser).Error
				} else if source == "backup" {
					err = fixture.db.Model(&model.Token{}).Where("id = ?", fixture.userID).Update("backup_group", "").Error
				} else {
					err = fixture.db.Model(&model.Token{}).Where("id = ?", fixture.userID).Update("status", config.TokenStatusDisabled).Error
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := client.WriteMessage(wsconn.TextMessage, []byte(create)); err != nil {
					t.Fatal(err)
				}
				select {
				case payload := <-fixture.frames:
					t.Fatalf("撤权后仍发送上游: %s", payload)
				case payload := <-frames:
					var event map[string]any
					if json.Unmarshal(payload, &event) != nil || event["type"] != "error" {
						t.Fatalf("未返回拒绝错误: %s", payload)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("撤权未得到明确拒绝")
				}
				if fixture.dials.Load() != 1 {
					t.Fatalf("上游连接次数=%d，期望1", fixture.dials.Load())
				}
				fixture.shutdown()
				select {
				case payload := <-fixture.frames:
					t.Fatalf("撤权后仍有上游工作: %s", payload)
				default:
				}
				var user model.User
				var token model.Token
				if err := fixture.db.First(&user, fixture.userID).Error; err != nil {
					t.Fatal(err)
				}
				if err := fixture.db.First(&token, fixture.userID).Error; err != nil {
					t.Fatal(err)
				}
				if user.UsedQuota != 4*ratio || user.Quota != 100000-4*ratio || token.RemainQuota != 100000-4*ratio {
					t.Fatal("撤权拒绝改变了已完成工作的结算")
				}
			})
		}
	}
}

func TestFixI007_OrdinaryPrincipalCannotSelectChannel(t *testing.T) {
	for _, responses := range []bool{false, true} {
		t.Run(fmt.Sprint(responses), func(t *testing.T) {
			fixture := newLongLivedRouteFixture(t, false, responses)
			var channel model.Channel
			if err := fixture.db.First(&channel).Error; err != nil {
				t.Fatal(err)
			}
			fixture.token += fmt.Sprintf("#%d", channel.Id)
			client, err := fixture.dial(t)
			if client != nil {
				client.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})
			}
			if err == nil || fixture.dials.Load() != 0 || fixture.upgrades.Load() != 0 {
				t.Fatalf("普通用户显式选渠道未前置拒绝: err=%v upstream=%d", err, fixture.dials.Load())
			}
		})
	}
}
