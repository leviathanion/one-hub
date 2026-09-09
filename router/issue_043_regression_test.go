package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/wsconn"
	"one-api/model"

	"gorm.io/gorm"
)

func TestFixI043_OwnedResponsesRequireCurrentGroupBeforeNewWork(t *testing.T) {
	for _, operation := range []string{"create", "http_continuation", "input_tokens", "ws_first", "ws_next"} {
		for _, authorization := range []string{"revoked", "paid", "backup", "admin", "private_backup", "admin_without_selector", "revoked_with_basic_channel"} {
			if operation == "create" && authorization == "revoked_with_basic_channel" {
				continue
			}
			t.Run(operation+"/"+authorization, func(t *testing.T) {
				var creates atomic.Int64
				var computes atomic.Int64
				fixture := newLongLivedRouteFixtureWithHTTP(t, false, true, func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost && r.Header.Get("Authorization") != "Bearer upstream-secret" {
						t.Errorf("旧资源被转到其他执行身份")
						w.WriteHeader(http.StatusNotFound)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					// 合法分块JSON响应，响应表示头回归由I-015单独覆盖。
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					if r.Method == http.MethodPost {
						if strings.HasSuffix(r.URL.Path, "/input_tokens") {
							computes.Add(1)
							fmt.Fprint(w, `{"object":"response.input_tokens","input_tokens":2}`)
							return
						}
						n := creates.Add(1)
						fmt.Fprintf(w, `{"id":"resp_i043_%d","model":"gpt-4o","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`, n)
						return
					}
					if r.Method == http.MethodDelete {
						fmt.Fprint(w, `{"id":"resp_i043_1","deleted":true}`)
						return
					}
					if strings.HasSuffix(r.URL.Path, "/input_items") {
						fmt.Fprint(w, `{"object":"list","data":[],"has_more":false}`)
						return
					}
					fmt.Fprint(w, `{"id":"resp_i043_1","status":"completed"}`)
				})
				sqlDB, err := fixture.db.DB()
				if err != nil {
					t.Fatal(err)
				}
				sqlDB.SetMaxOpenConns(1)
				oldEncoders := config.DisableTokenEncoders
				config.DisableTokenEncoders = true
				t.Cleanup(func() { config.DisableTokenEncoders = oldEncoders })
				enabled := true
				for _, group := range []*model.UserGroup{{Symbol: "paid", Ratio: 2, APIRate: 100, Enable: &enabled, Public: authorization == "backup"}, {Symbol: "basic", Ratio: 1, APIRate: 100, Enable: &enabled}} {
					if err := fixture.db.Create(group).Error; err != nil {
						t.Fatal(err)
					}
				}
				role := config.RoleCommonUser
				if authorization == "admin" || authorization == "admin_without_selector" {
					role = config.RoleAdminUser
				}
				if err := fixture.db.Model(&model.User{}).Where("id = ?", fixture.userID).Updates(map[string]any{"group": "paid", "role": role}).Error; err != nil {
					t.Fatal(err)
				}
				if err := fixture.db.Model(&model.Channel{}).Where("1 = 1").Update("group", "paid").Error; err != nil {
					t.Fatal(err)
				}
				if authorization == "backup" || authorization == "private_backup" {
					if err := fixture.db.Model(&model.Token{}).Where("id = ?", fixture.userID).Update("backup_group", "paid").Error; err != nil {
						t.Fatal(err)
					}
				}
				if authorization == "admin" {
					var channel model.Channel
					if err := fixture.db.First(&channel).Error; err != nil {
						t.Fatal(err)
					}
					fixture.token += fmt.Sprintf("#%d", channel.Id)
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
				call := func(method, path, payload, credential string) (int, []byte) {
					request, err := http.NewRequest(method, "http"+strings.TrimPrefix(fixture.url, "ws")+path, strings.NewReader(payload))
					if err != nil {
						t.Fatal(err)
					}
					request.Header.Set("Authorization", "Bearer "+credential)
					request.Header.Set("Content-Type", "application/json")
					client := &http.Client{Timeout: 3 * time.Second}
					response, err := client.Do(request)
					if err != nil {
						t.Fatal(err)
					}
					defer response.Body.Close()
					raw, err := io.ReadAll(response.Body)
					if err != nil {
						t.Fatal(err)
					}
					return response.StatusCode, raw
				}
				post := func(previous, inputTokens bool) (int, []byte) {
					body := map[string]any{"model": "gpt-4o", "input": "hello", "store": !previous}
					if previous {
						body["previous_response_id"] = "resp_i043_1"
					}
					path := ""
					if inputTokens {
						delete(body, "store")
						path = "/input_tokens"
					}
					payload, _ := json.Marshal(body)
					return call(http.MethodPost, path, string(payload), fixture.token)
				}
				if status, body := post(false, false); status != http.StatusOK {
					t.Fatalf("paid创建资源失败：%d %s", status, body)
				}
				owner, err := model.GetResponseOwner(context.Background(), "resp_i043_1", fixture.userID)
				if err != nil {
					t.Fatal(err)
				}
				fixture.awaitQuota(t, 4)
				charged := 4
				var client, upstream *wsconn.ManagedConn
				var frames <-chan []byte
				createFrame := `{"type":"response.create","model":"gpt-4o","previous_response_id":"resp_i043_1","store":false,"input":"continue","max_output_tokens":100}`
				openWS := func() {
					var err error
					client, err = fixture.dial(t)
					if err != nil {
						t.Fatal(err)
					}
					frames = realtimeRouteClientFrames(t, client)
				}
				completeWS := func(turn int) {
					if upstream == nil {
						upstream = <-fixture.upstream
					}
					for _, event := range []string{fmt.Sprintf(`{"type":"response.created","sequence_number":%d,"response":{"id":"resp_i043_ws_%d","model":"gpt-4o","status":"in_progress"}}`, turn*2+1, turn), fmt.Sprintf(`{"type":"response.completed","sequence_number":%d,"response":{"id":"resp_i043_ws_%d","model":"gpt-4o","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`, turn*2+2, turn)} {
						if err := upstream.WriteMessage(wsconn.TextMessage, []byte(event)); err != nil {
							t.Fatal(err)
						}
						if payload := realtimeRouteReceive(t, frames); strings.Contains(string(payload), `"type":"error"`) {
							t.Fatalf("合法终态变成错误：%s", payload)
						}
					}
				}
				if operation == "ws_next" {
					openWS()
					if err := client.WriteMessage(wsconn.TextMessage, []byte(createFrame)); err != nil {
						t.Fatal(err)
					}
					select {
					case <-fixture.frames:
					case payload := <-frames:
						t.Fatalf("paid首轮被拒：%s", payload)
					case <-time.After(3 * time.Second):
						t.Fatal("paid首轮未发送")
					}
					completeWS(0)
					charged += 4
					fixture.awaitQuota(t, charged)
				}
				if authorization != "paid" {
					if err := fixture.db.Model(&model.User{}).Where("id = ?", fixture.userID).Update("group", "basic").Error; err != nil {
						t.Fatal(err)
					}
				}
				if authorization == "revoked_with_basic_channel" {
					var alternate model.Channel
					if err := fixture.db.First(&alternate, owner.ChannelID).Error; err != nil {
						t.Fatal(err)
					}
					alternate.Id = 0
					alternate.Group = "basic"
					alternate.Key = "alternate-test-key"
					if err := fixture.db.Create(&alternate).Error; err != nil {
						t.Fatal(err)
					}
					if err := model.ChannelGroup.Load(); err != nil {
						t.Fatal(err)
					}
				}
				var moneyWrites atomic.Int64
				callback := "i043_money_write"
				if err := fixture.db.Callback().Update().After("gorm:update").Register(callback, func(tx *gorm.DB) {
					if (tx.Statement.Table == "users" || tx.Statement.Table == "tokens") && strings.Contains(tx.Statement.SQL.String(), "quota") {
						moneyWrites.Add(1)
					}
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = fixture.db.Callback().Update().Remove(callback) })
				allowed := authorization == "paid" || authorization == "backup" || authorization == "admin"
				if operation == "create" || operation == "http_continuation" || operation == "input_tokens" {
					status, body := post(operation != "create", operation == "input_tokens")
					if allowed && status != http.StatusOK {
						t.Fatalf("当前合法授权被拒：%d %s", status, body)
					}
					if !allowed && status < 400 {
						t.Fatalf("basic新工作越权：%d %s", status, body)
					}
				} else {
					if client == nil {
						openWS()
					}
					if err := client.WriteMessage(wsconn.TextMessage, []byte(createFrame)); err != nil {
						t.Fatal(err)
					}
					select {
					case payload := <-fixture.frames:
						if !allowed {
							t.Fatalf("basic新工作已发上游：%s", payload)
						}
						turn := 0
						if operation == "ws_next" {
							turn = 1
						}
						completeWS(turn)
					case payload := <-frames:
						if allowed || !strings.Contains(string(payload), `"type":"error"`) {
							t.Fatalf("当前授权返回错误结果：%s", payload)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("新工作没有完成准入判断")
					}
				}
				if allowed {
					extra := 4
					if authorization == "admin" {
						extra = 2
					}
					if operation == "input_tokens" {
						extra = 0
					}
					charged += extra
				}
				fixture.awaitQuota(t, charged)
				ownerState := model.ResponseOwnerStateActive
				if operation == "http_continuation" && !allowed {
					newTokenID := fixture.userID + 100000
					newCredential, err := common.GenerateToken(newTokenID, fixture.userID)
					if err != nil {
						t.Fatal(err)
					}
					if err := fixture.db.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{Id: newTokenID, UserId: fixture.userID, Key: newCredential, Status: config.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 100000}).Error; err != nil {
						t.Fatal(err)
					}
					if err := fixture.db.Model(&model.Channel{}).Where("id = ?", owner.ChannelID).Update("status", config.ChannelStatusManuallyDisabled).Error; err != nil {
						t.Fatal(err)
					}
					if err := model.ChannelGroup.Load(); err != nil {
						t.Fatal(err)
					}
					for _, request := range []struct{ method, path string }{{http.MethodGet, "/resp_i043_1"}, {http.MethodGet, "/resp_i043_1/input_items"}, {http.MethodDelete, "/resp_i043_1"}} {
						if status, body := call(request.method, request.path, "", newCredential); status != http.StatusOK {
							t.Fatalf("同用户新Token无法操作禁用渠道的旧资源：%s %d %s", request.method, status, body)
						}
					}
					ownerState = model.ResponseOwnerStateDeleted
				}
				fixture.shutdown()
				if !allowed {
					if creates.Load() != 1 || computes.Load() != 0 || moneyWrites.Load() != 0 {
						t.Fatalf("拒绝前已发生工作/预扣：HTTP创建=%d 计算=%d 资金写入=%d", creates.Load(), computes.Load(), moneyWrites.Load())
					}
					select {
					case payload := <-fixture.frames:
						t.Fatalf("撤权后残留上游工作：%s", payload)
					default:
					}
					wantDials := int64(0)
					if operation == "ws_next" {
						wantDials = 1
					}
					if fixture.dials.Load() != wantDials {
						t.Fatalf("未在上游连接前拒绝：dials=%d", fixture.dials.Load())
					}
				}
				current, err := model.GetResponseOwner(context.Background(), owner.ResponseID, fixture.userID)
				if err != nil || current.ChannelID != owner.ChannelID || current.State != ownerState {
					t.Fatalf("原owner被改变：%+v %v", current, err)
				}
			})
		}
	}
}
