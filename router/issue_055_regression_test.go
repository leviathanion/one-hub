package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/cache"
	"one-api/common/config"
	commonredis "one-api/common/redis"
	"one-api/model"

	redisclient "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// 使用显式本地Redis和本次唯一的key；连接失败只注入本测试的客户端。
func issue055Redis(t *testing.T) (*redisclient.Client, func(func())) {
	t.Helper()
	raw := os.Getenv("ONEHUB_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("需要显式ONEHUB_TEST_REDIS_URL本地Redis")
	}
	options, err := redisclient.ParseURL(raw)
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(options.Addr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		t.Fatal("Redis夹具只允许显式loopback地址")
	}
	client := redisclient.NewClient(options)
	if err := client.Ping(t.Context()).Err(); err != nil {
		t.Fatal(err)
	}
	oldRDB, oldEnabled := commonredis.RDB, config.RedisEnabled
	oldTokenKey, oldEnabledKey, oldTTL := model.UserTokensKey, model.UserEnabledCacheKey, model.TokenCacheSeconds
	prefix := fmt.Sprintf("onehub:i055:%d:", time.Now().UnixNano())
	model.UserTokensKey, model.UserEnabledCacheKey = prefix+"token:%s", prefix+"user:%d"
	model.TokenCacheSeconds = 600
	commonredis.RDB, config.RedisEnabled = client, true
	cache.InitCacheManager()
	t.Cleanup(func() {
		keys, err := client.Keys(context.Background(), prefix+"*").Result()
		if err == nil && len(keys) > 0 {
			_ = client.Del(context.Background(), keys...).Err()
		}
		commonredis.RDB, config.RedisEnabled = oldRDB, oldEnabled
		model.UserTokensKey, model.UserEnabledCacheKey, model.TokenCacheSeconds = oldTokenKey, oldEnabledKey, oldTTL
		cache.InitCacheManager()
		_ = client.Close()
	})
	return client, func(mutate func()) {
		failedOptions := *options
		failedOptions.MaxRetries = -1
		failedOptions.Dialer = func(context.Context, string, string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("本地测试注入Redis连接中断")}
		}
		failed := redisclient.NewClient(&failedOptions)
		defer failed.Close()
		commonredis.RDB = failed
		defer func() { commonredis.RDB = client }()
		if err := commonredis.RedisDel(prefix + "probe"); err == nil {
			t.Fatal("Redis故障未生效")
		}
		mutate()
	}
}

func TestFixI055_FreeResponsesUseCurrentCredentials(t *testing.T) {
	operations := []string{"retrieve", "input_items", "delete", "input_tokens", "input_tokens_previous"}
	scenarios := []string{"disable_normal", "delete_normal", "disable_redis_failure", "delete_redis_failure", "sql_failure", "zero_balance", "same_user_new_token", "other_user", "expired", "owner_mismatch"}
	for _, operation := range operations {
		for _, scenario := range scenarios {
			if operation == "input_tokens" && scenario == "other_user" {
				continue // 没有历史资源ID的新计算不受其他资源owner约束。
			}
			t.Run(operation+"/"+scenario, func(t *testing.T) {
				var upstreamCalls atomic.Int64
				fixture := newLongLivedRouteFixtureWithHTTP(t, false, true, func(w http.ResponseWriter, r *http.Request) {
					upstreamCalls.Add(1)
					if r.Header.Get("Authorization") != "Bearer upstream-secret" {
						t.Error("固定资源执行凭据改变")
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					switch {
					case r.Method == http.MethodDelete:
						fmt.Fprint(w, `{"id":"resp_i055","deleted":true}`)
					case strings.HasSuffix(r.URL.Path, "/input_items"):
						fmt.Fprint(w, `{"object":"list","data":[],"has_more":false}`)
					case strings.HasSuffix(r.URL.Path, "/input_tokens"):
						fmt.Fprint(w, `{"object":"response.input_tokens","input_tokens":2}`)
					default:
						fmt.Fprint(w, `{"id":"resp_i055","status":"completed"}`)
					}
				})
				sqlDB, err := fixture.db.DB()
				if err != nil {
					t.Fatal(err)
				}
				sqlDB.SetMaxOpenConns(1)
				client, disconnected := issue055Redis(t)
				var channel model.Channel
				if err := fixture.db.First(&channel).Error; err != nil {
					t.Fatal(err)
				}
				owner, err := model.NewResponseOwner("resp_i055", fixture.userID, fixture.userID, channel.Id, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				if err := model.CreateResponseOwner(t.Context(), owner); err != nil {
					t.Fatal(err)
				}
				credential := fixture.token
				if scenario == "same_user_new_token" || scenario == "other_user" {
					userID := fixture.userID
					if scenario == "other_user" {
						userID += 1000000
						if err := fixture.db.Create(&model.User{Id: userID, Username: "other-user", AffCode: "other-aff", Password: "password", AccessToken: "other-access", Status: config.UserStatusEnabled, Group: "realtime-route"}).Error; err != nil {
							t.Fatal(err)
						}
					}
					credential, err = common.GenerateToken(fixture.userID+2000000, userID)
					if err != nil {
						t.Fatal(err)
					}
					token := &model.Token{Id: fixture.userID + 2000000, UserId: userID, Key: credential, Status: config.TokenStatusEnabled, ExpiredTime: -1}
					if err := fixture.db.Session(&gorm.Session{SkipHooks: true}).Create(token).Error; err != nil {
						t.Fatal(err)
					}
					if err := model.DeleteTokenById(fixture.userID, fixture.userID); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "zero_balance" || scenario == "same_user_new_token" {
					if err := fixture.db.Model(&model.User{}).Where("id = ?", fixture.userID).Update("quota", 0).Error; err != nil {
						t.Fatal(err)
					}
					if err := fixture.db.Model(&model.Token{}).Where("user_id = ?", fixture.userID).Update("remain_quota", 0).Error; err != nil {
						t.Fatal(err)
					}
				}
				cachedToken, err := model.ValidateUserToken(credential)
				if err != nil {
					t.Fatal(err)
				}
				cacheKey := fmt.Sprintf(model.UserTokensKey, credential)
				if exists := client.Exists(t.Context(), cacheKey).Val(); exists != 1 {
					t.Fatal("未建立真实Redis enabled缓存")
				}
				if strings.HasPrefix(scenario, "disable_") || strings.HasPrefix(scenario, "delete_") {
					mutate := func() {
						var err error
						if strings.HasPrefix(scenario, "disable_") {
							cachedToken.Status = config.TokenStatusDisabled
							err = cachedToken.UpdateMutableFields(nil)
						} else {
							err = model.DeleteTokenById(cachedToken.Id, cachedToken.UserId)
						}
						if err != nil {
							t.Fatalf("撤销SQL未提交：%v", err)
						}
					}
					if strings.HasSuffix(scenario, "redis_failure") {
						disconnected(mutate)
						stale, err := model.ValidateUserToken(credential)
						if err != nil || stale.Status != config.TokenStatusEnabled {
							t.Fatalf("故障恢复后未保留旧enabled缓存：%v", err)
						}
					} else {
						mutate()
						if client.Exists(t.Context(), cacheKey).Val() != 0 {
							t.Fatal("正常失效未删除缓存")
						}
					}
				}
				if scenario == "expired" || scenario == "owner_mismatch" {
					field, value := "expired_time", time.Now().Unix()-1
					if scenario == "owner_mismatch" {
						field, value = "user_id", int64(fixture.userID+999)
					}
					if err := fixture.db.Model(&model.Token{}).Where("id = ?", fixture.userID).Update(field, value).Error; err != nil {
						t.Fatal(err)
					}
				}
				var moneyWrites atomic.Int64
				if err := fixture.db.Callback().Update().After("gorm:update").Register("i055_money", func(tx *gorm.DB) {
					if (tx.Statement.Table == "users" || tx.Statement.Table == "tokens") && strings.Contains(tx.Statement.SQL.String(), "quota") {
						moneyWrites.Add(1)
					}
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = fixture.db.Callback().Update().Remove("i055_money") })
				if scenario == "sql_failure" {
					if err := fixture.db.Callback().Query().Before("gorm:query").Register("i055_sql_failure", func(tx *gorm.DB) {
						if tx.Statement.Table == "tokens" {
							_ = tx.AddError(errors.New("本地测试SQL不可用"))
						}
					}); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = fixture.db.Callback().Query().Remove("i055_sql_failure") })
				}
				method, path, body := http.MethodGet, "/resp_i055", ""
				switch operation {
				case "input_items":
					path += "/input_items"
				case "delete":
					method = http.MethodDelete
				case "input_tokens", "input_tokens_previous":
					method, path = http.MethodPost, "/input_tokens"
					body = `{"model":"gpt-4o","input":"hello"}`
					if operation == "input_tokens_previous" {
						body = `{"model":"gpt-4o","previous_response_id":"resp_i055","input":"hello"}`
					}
				}
				request, err := http.NewRequest(method, "http"+strings.TrimPrefix(fixture.url, "ws")+path, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer "+credential)
				request.Header.Set("Content-Type", "application/json")
				response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				raw, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				fixture.shutdown()
				allowed := scenario == "zero_balance" || scenario == "same_user_new_token"
				wantStatus := http.StatusUnauthorized
				if allowed {
					wantStatus = http.StatusOK
				} else if scenario == "sql_failure" {
					wantStatus = http.StatusServiceUnavailable
				} else if scenario == "other_user" {
					wantStatus = http.StatusNotFound
					if operation == "input_tokens_previous" {
						wantStatus = http.StatusBadRequest
					}
				}
				if response.StatusCode != wantStatus {
					t.Errorf("status=%d want=%d body=%s", response.StatusCode, wantStatus, raw)
				}
				wantCalls := int64(0)
				if allowed {
					wantCalls = 1
				}
				if got := upstreamCalls.Load(); got != wantCalls {
					t.Errorf("上游调用=%d want=%d", got, wantCalls)
				}
				if moneyWrites.Load() != 0 {
					t.Errorf("免费操作产生%d次额度写入", moneyWrites.Load())
				}
				var current model.ResponseOwner
				if err := fixture.db.First(&current, owner.ID).Error; err != nil {
					t.Fatal(err)
				}
				wantState := model.ResponseOwnerStateActive
				if allowed && operation == "delete" {
					wantState = model.ResponseOwnerStateDeleted
				}
				if current.State != wantState || current.UserID != fixture.userID || current.ChannelID != channel.Id || current.TokenID != owner.TokenID {
					t.Errorf("资源owner意外改变：%+v", current)
				}
			})
		}
	}
}
