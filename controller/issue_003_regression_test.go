package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	commonredis "one-api/common/redis"
	"one-api/middleware"
	"one-api/model"
	"one-api/providers/codex"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

type issue003APIResult struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func issue003JWT(account string) string {
	claims, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account}})
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims) + ".test-signature"
}

func issue003HTTPAPI(t *testing.T) func(string, any, string) issue003APIResult {
	t.Helper()
	if err := model.DB.AutoMigrate(&model.User{}); err != nil {
		t.Fatal(err)
	}
	for index, role := range []int{config.RoleAdminUser, config.RoleCommonUser} {
		user := &model.User{Id: index + 1, Username: fmt.Sprintf("i003-user-%d", index), Password: "password", AccessToken: fmt.Sprintf("i003-access-%d", index), AffCode: fmt.Sprintf("i003-aff-%d", index), Role: role, Status: config.UserStatusEnabled}
		if err := model.DB.Create(user).Error; err != nil {
			t.Fatal(err)
		}
	}
	engine := gin.New()
	engine.Use(sessions.Sessions("session", cookie.NewStore([]byte("i003-local-session-secret-32bytes"))))
	group := engine.Group("/api/codex/oauth", middleware.AdminAuth())
	group.POST("/start", StartCodexOAuth)
	group.POST("/exchange-code", CodexOAuthCallback)
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	// OAuth端点测试另行替换DefaultTransport；API请求必须固定连接本地应用。
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	return func(path string, payload any, credential string) issue003APIResult {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest(http.MethodPost, server.URL+"/api/codex/oauth/"+path, strings.NewReader(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+credential)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result issue003APIResult
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
}

func TestFixI003_RealOAuthRecoversOnlyOriginalCredentialSnapshot(t *testing.T) {
	for _, scenario := range []string{"same_account", "different_account", "exchange_error", "missing_access_token", "blank_access_token", "missing_state", "wrong_url_state", "missing_url_state", "duplicate_url_state", "payload_state_mismatch", "missing_verifier", "expired_session", "deleted", "type_changed", "account_changed", "revision_changed", "fence_changed", "revision_changed_during_exchange", "non_admin"} {
		t.Run(scenario, func(t *testing.T) {
			useControllerChannelTagTestDB(t)
			oldRedis := config.RedisEnabled
			config.RedisEnabled = false
			cache.InitCacheManager()
			t.Cleanup(func() { config.RedisEnabled = oldRedis; cache.InitCacheManager() })
			sqlDB, err := model.DB.DB()
			if err != nil {
				t.Fatal(err)
			}
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = sqlDB.Close() })
			key := `{"access_token":"i003-old-access","refresh_token":"i003-old-refresh","account_id":"account-a"}`
			channel := &model.Channel{Type: config.ChannelTypeCodex, Key: key, Status: config.ChannelStatusEnabled, Models: "gpt-5", Group: "default"}
			if err := model.DB.Create(channel).Error; err != nil {
				t.Fatal(err)
			}
			oldTicket := model.CredentialRotationTicket{ChannelID: channel.Id, ExpectedRevision: 0, AttemptID: "i003-unresolved-refresh"}
			if outcome, err := model.ClaimCredentialRotation(t.Context(), oldTicket, time.Now()); err != nil || outcome != model.CredentialRotationClaimAcquired {
				t.Fatalf("未建立未决刷新fence：%v %v", outcome, err)
			}
			call := issue003HTTPAPI(t)
			start := call("start", map[string]any{"channel_id": channel.Id}, "i003-access-0")
			var startData struct {
				SessionID string `json:"session_id"`
				AuthURL   string `json:"auth_url"`
			}
			if !start.Success || json.Unmarshal(start.Data, &startData) != nil || startData.SessionID == "" {
				t.Fatalf("启动授权失败：%s", start.Message)
			}
			stateData, err := cache.GetCache[CodexOAuthStateData](CodexOAuthStateCachePrefix + startData.SessionID)
			if err != nil || stateData.ChannelID != channel.Id || stateData.AccountID != "account-a" || stateData.CredentialRevision != 0 || stateData.CredentialFence != oldTicket.AttemptID {
				t.Fatal("state未固定原渠道、账号、revision/fence")
			}
			authorizeURL, err := url.Parse(startData.AuthURL)
			if err != nil || authorizeURL.Query().Get("state") != startData.SessionID || authorizeURL.Query().Get("code_challenge_method") != "S256" || authorizeURL.Query().Get("code_challenge") != generateCodexCodeChallenge(stateData.CodeVerifier) {
				t.Fatal("授权URL丢失state或PKCE约束")
			}
			if scenario == "payload_state_mismatch" || scenario == "missing_verifier" || scenario == "expired_session" {
				switch scenario {
				case "payload_state_mismatch":
					stateData.State = "another-state"
				case "missing_verifier":
					stateData.CodeVerifier = ""
				case "expired_session":
					stateData.CreatedAt = time.Now().Add(-2 * CodexOAuthStateCacheDuration).Unix()
				}
				if err := cache.SetCache(CodexOAuthStateCachePrefix+startData.SessionID, stateData, CodexOAuthStateCacheDuration); err != nil {
					t.Fatal(err)
				}
			}
			mutate := func(fields map[string]any) {
				if err := model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).Updates(fields).Error; err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "deleted":
				if err := channel.Delete(); err != nil {
					t.Fatal(err)
				}
			case "type_changed":
				mutate(map[string]any{"type": config.ChannelTypeOpenAI})
			case "account_changed":
				mutate(map[string]any{"key": `{"access_token":"other","account_id":"account-b"}`})
			case "revision_changed":
				mutate(map[string]any{"credential_revision": 2})
			case "fence_changed":
				mutate(map[string]any{"credential_refresh_fence": "other-fence"})
			}
			before, err := model.LoadCredentialRotationSnapshot(t.Context(), channel.Id)
			if err != nil {
				t.Fatal(err)
			}
			var exchanges atomic.Int64
			newAccess := issue003JWT("account-a")
			if scenario == "different_account" {
				newAccess = issue003JWT("account-b")
			}
			withTokenEndpointTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code_verifier") != stateData.CodeVerifier || r.Form.Get("code") != "i003-independent-authorization-code" || r.Form.Get("refresh_token") != "" {
					t.Error("恢复必须交换独立授权码并携带对应PKCE")
				}
				current, err := model.LoadCredentialRotationSnapshot(context.Background(), channel.Id)
				if err != nil || current.Key != before.Key || current.Revision != before.Revision || current.Fence == nil || *current.Fence != *before.Fence {
					t.Error("授权交换完成前改动了已有保护状态")
				}
				if scenario == "revision_changed_during_exchange" {
					if err := model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("credential_revision", 3).Error; err != nil {
						t.Error(err)
					}
				}
				if scenario == "exchange_error" {
					w.WriteHeader(http.StatusBadGateway)
					io.WriteString(w, `{"error":"authorization service unavailable"}`)
					return
				}
				if scenario == "missing_access_token" || scenario == "blank_access_token" {
					// 上游以 200 返回仅含原账号 id_token、缺失/空白 access_token 的响应：
					// 账号检查能通过，因此只靠账号一致不能放行，必须拒绝并保留原保护状态。
					payload := map[string]any{"id_token": issue003JWT("account-a")}
					if scenario == "blank_access_token" {
						payload["access_token"] = "   "
					}
					_ = json.NewEncoder(w).Encode(payload)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": newAccess, "refresh_token": "i003-new-refresh", "token_type": "Bearer", "expires_in": 3600})
			}))
			state, urlState, credential := startData.SessionID, startData.SessionID, "i003-access-0"
			if scenario == "missing_state" {
				state = "missing-oauth-session"
			}
			if scenario == "wrong_url_state" {
				urlState = "different-oauth-session"
			}
			if scenario == "non_admin" {
				credential = "i003-access-1"
			}
			callback := map[string]any{"session_id": state, "callback_url": codex.DefaultRedirectURI + "?code=i003-independent-authorization-code&state=" + url.QueryEscape(urlState)}
			if scenario == "missing_url_state" {
				callback["callback_url"] = codex.DefaultRedirectURI + "?code=i003-independent-authorization-code"
			}
			if scenario == "duplicate_url_state" {
				callback["callback_url"] = callback["callback_url"].(string) + "&state=" + url.QueryEscape(urlState)
			}
			result := call("exchange-code", callback, credential)
			after, err := model.LoadCredentialRotationSnapshot(t.Context(), channel.Id)
			if err != nil {
				t.Fatal(err)
			}
			wantSuccess := scenario == "same_account"
			if result.Success != wantSuccess {
				t.Errorf("恢复结果success=%v want=%v message=%s", result.Success, wantSuccess, result.Message)
			}
			if wantSuccess {
				credentials, err := codex.FromJSON(after.Key)
				if err != nil || credentials.AccessToken != newAccess || credentials.AccountID != "account-a" || after.Revision != 1 || after.Fence != nil || after.StartedAt != nil || after.Deleted || after.ChannelID != channel.Id {
					t.Error("原渠道未原子发布新凭据并解除保护状态")
				}
				currentChannel, err := model.GetChannelById(channel.Id)
				if err != nil {
					t.Fatal(err)
				}
				provider := (codex.CodexProviderFactory{}).Create(currentChannel).(*codex.CodexProvider)
				if access, err := provider.GetToken(); err != nil || access != newAccess {
					t.Error("恢复后原channel的provider未采用新凭据")
				}
				outcome, err := model.CommitCredentialRotation(t.Context(), oldTicket, "late-old-refresh")
				if err != nil || outcome != model.CredentialRotationCommitSuperseded {
					t.Errorf("旧刷新迟到仍可提交：%v %v", outcome, err)
				}
				again := call("exchange-code", callback, credential)
				if again.Success || exchanges.Load() != 1 {
					t.Error("OAuth state被重复交换或提交")
				}
				unchanged, _ := model.LoadCredentialRotationSnapshot(t.Context(), channel.Id)
				if unchanged.Key != after.Key || unchanged.Revision != after.Revision {
					t.Error("迟到刷新或重复state覆盖新凭据")
				}
			} else {
				wantRevision := before.Revision
				if scenario == "revision_changed_during_exchange" {
					wantRevision = 3
				}
				if after.Key != before.Key || after.Revision != wantRevision || after.Fence == nil || *after.Fence != *before.Fence || after.Deleted != before.Deleted {
					t.Error("失败恢复改写或清除了不属于本次授权的保护状态")
				}
			}
			if scenario == "missing_state" || scenario == "wrong_url_state" || scenario == "missing_url_state" || scenario == "duplicate_url_state" || scenario == "payload_state_mismatch" || scenario == "missing_verifier" || scenario == "expired_session" || scenario == "non_admin" {
				if exchanges.Load() != 0 {
					t.Errorf("会话/权限错误仍触发%d次授权码交换", exchanges.Load())
				}
			} else if scenario == "missing_access_token" || scenario == "blank_access_token" {
				if exchanges.Load() != 1 {
					t.Errorf("缺少 access_token 仍应完成一次授权码交换，实际 %d 次", exchanges.Load())
				}
				if after.Key != before.Key || after.Revision != before.Revision || after.Fence == nil || *after.Fence != *before.Fence {
					t.Error("缺少 access_token 的响应不得构造空凭据、保存或清除 fence")
				}
				if strings.Contains(result.Message, issue003JWT("account-a")) || strings.Contains(string(result.Data), issue003JWT("account-a")) || strings.Contains(result.Message, "i003-new-refresh") || strings.Contains(string(result.Data), "i003-new-refresh") || strings.Contains(result.Message, "i003-old-access") || strings.Contains(string(result.Data), "i003-old-access") || strings.Contains(result.Message, "i003-old-refresh") || strings.Contains(string(result.Data), "i003-old-refresh") {
					t.Error("失败响应泄露了令牌或 id_token 内容")
				}
				again := call("exchange-code", callback, credential)
				if again.Success || exchanges.Load() != 1 {
					t.Error("失败后同一 state 仍可再次交换或提交")
				}
				unchanged, _ := model.LoadCredentialRotationSnapshot(t.Context(), channel.Id)
				if unchanged.Key != after.Key || unchanged.Revision != after.Revision || unchanged.Fence == nil || *unchanged.Fence != *after.Fence {
					t.Error("重放失败响应改写或清除了保护状态")
				}
			} else if scenario == "same_account" || scenario == "different_account" || scenario == "exchange_error" || scenario == "revision_changed_during_exchange" {
				if exchanges.Load() != 1 {
					t.Errorf("独立授权码交换次数=%d want=1", exchanges.Load())
				}
			}
		})
	}
}

func TestFixI003_ConcurrentOAuthRecovery(t *testing.T) {
	for _, sameState := range []bool{true, false} {
		t.Run(fmt.Sprintf("same_state_%v", sameState), func(t *testing.T) {
			useControllerChannelTagTestDB(t)
			oldRedis := config.RedisEnabled
			config.RedisEnabled = false
			cache.InitCacheManager()
			t.Cleanup(func() { config.RedisEnabled = oldRedis; cache.InitCacheManager() })
			sqlDB, err := model.DB.DB()
			if err != nil {
				t.Fatal(err)
			}
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = sqlDB.Close() })
			fence := "i003-concurrent-unresolved"
			channel := &model.Channel{Type: config.ChannelTypeCodex, Key: `{"access_token":"old","refresh_token":"old-refresh","account_id":"account-a"}`, CredentialRefreshFence: &fence}
			if err := model.DB.Create(channel).Error; err != nil {
				t.Fatal(err)
			}
			call := issue003HTTPAPI(t)
			start := func() string {
				result := call("start", map[string]any{"channel_id": channel.Id}, "i003-access-0")
				var data struct {
					SessionID string `json:"session_id"`
				}
				if !result.Success || json.Unmarshal(result.Data, &data) != nil || data.SessionID == "" {
					t.Fatal("无法启动并发恢复授权")
				}
				return data.SessionID
			}
			states := []string{start(), ""}
			states[1] = states[0]
			if !sameState {
				states[1] = start()
			}
			entered := make(chan struct{}, 2)
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			var exchanges atomic.Int64
			withTokenEndpointTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				entered <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": issue003JWT("account-a"), "refresh_token": "i003-concurrent-new-refresh", "expires_in": 3600})
			}))
			t.Cleanup(unblock)
			results := make(chan issue003APIResult, 2)
			callback := func(state string) {
				results <- call("exchange-code", map[string]any{"session_id": state, "authorization_code": "i003-concurrent-code-123456"}, "i003-access-0")
			}
			go callback(states[0])
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("首个恢复未进入独立授权交换")
			}
			go callback(states[1])
			remaining := 2
			if sameState {
				select {
				case result := <-results:
					remaining--
					if result.Success {
						t.Error("正在使用的state被再次接受")
					}
				case <-entered:
					t.Error("同state触发第二次授权码交换")
				case <-time.After(3 * time.Second):
					t.Fatal("同state没有被及时拒绝")
				}
			} else {
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("两个独立state未各自交换授权码")
				}
			}
			unblock()
			successes := 0
			for i := 0; i < remaining; i++ {
				select {
				case result := <-results:
					if result.Success {
						successes++
					}
				case <-time.After(3 * time.Second):
					t.Fatal("并发恢复未结束")
				}
			}
			wantExchanges := int64(2)
			if sameState {
				wantExchanges = 1
			}
			if successes != 1 || exchanges.Load() != wantExchanges {
				t.Errorf("恢复成功数=%d，exchange=%d want=%d", successes, exchanges.Load(), wantExchanges)
			}
			snapshot, err := model.LoadCredentialRotationSnapshot(t.Context(), channel.Id)
			if err != nil || snapshot.Revision != 1 || snapshot.Fence != nil || snapshot.StartedAt != nil {
				t.Error("并发恢复没有唯一提交新凭据revision")
			}
		})
	}
}

// TestFixI003_OAuthStateConsumedExactlyOnceThroughRealRedis 验证 controller 的
// state 写入/消费与真实 Redis 的接线：Start 落盘真实 Redis，Callback 经
// ConsumeCacheContext 单次 GETDEL 消费；同 state 双并发只有一个能交换授权码，
// 消费后 key 不存在、重放被拒。无显式测试 Redis 时跳过。
func TestFixI003_OAuthStateConsumedExactlyOnceThroughRealRedis(t *testing.T) {
	url := os.Getenv("ONEHUB_TEST_REDIS_URL")
	if url == "" {
		t.Skip("真实 Redis 接线用例需要显式 ONEHUB_TEST_REDIS_URL")
	}
	options, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(options.Addr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		t.Fatal("真实 Redis 接线用例只接受显式 loopback 测试实例")
	}
	client := redis.NewClient(options)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	useControllerChannelTagTestDB(t)
	oldEnabled, oldRedis := config.RedisEnabled, commonredis.RDB
	config.RedisEnabled, commonredis.RDB = true, client
	cache.InitCacheManager()
	t.Cleanup(func() {
		config.RedisEnabled, commonredis.RDB = oldEnabled, oldRedis
		cache.InitCacheManager()
	})

	fence := "i003-redis-unresolved"
	channel := &model.Channel{Type: config.ChannelTypeCodex, Key: `{"access_token":"redis-old-access","refresh_token":"redis-old-refresh","account_id":"account-a"}`, CredentialRefreshFence: &fence}
	if err := model.DB.Create(channel).Error; err != nil {
		t.Fatal(err)
	}
	call := issue003HTTPAPI(t)
	result := call("start", map[string]any{"channel_id": channel.Id}, "i003-access-0")
	var startData struct {
		SessionID string `json:"session_id"`
	}
	if !result.Success || json.Unmarshal(result.Data, &startData) != nil || startData.SessionID == "" {
		t.Fatalf("启动授权失败：%s", result.Message)
	}
	stateKey := CodexOAuthStateCachePrefix + startData.SessionID
	existsCtx, cancelExists := context.WithTimeout(context.Background(), time.Second)
	defer cancelExists()
	if n, err := client.Exists(existsCtx, stateKey).Result(); err != nil || n != 1 {
		t.Fatalf("授权 state 未写入真实 Redis：n=%d err=%v", n, err)
	}

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var exchanges atomic.Int64
	newAccess := issue003JWT("account-a")
	withTokenEndpointTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": newAccess, "refresh_token": "i003-redis-new-refresh", "expires_in": 3600})
	}))
	// Cleanup 是 LIFO：unblock 必须在 withTokenEndpointTLSServer 之后注册，保证
	// 测试提前退出时先解除 handler 阻塞、再关闭 TLS server，避免 Close 等待仍在
	// release 上阻塞的 handler。
	t.Cleanup(unblock)
	callback := map[string]any{"session_id": startData.SessionID, "authorization_code": "i003-redis-code-123456"}
	results := make(chan issue003APIResult, 2)
	start := func() {
		go func() { results <- call("exchange-code", callback, "i003-access-0") }()
	}
	start()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("真实 Redis 首个恢复未进入独立授权交换")
	}
	start()
	remaining := 2
	// 首个回调仍在授权交换中：同 state 第二次请求必须因 state 已被单次 GETDEL
	// 消费而快速失败。若意外进入第二次交换，select 会消费 entered 信号记错，
	// remaining 保持 2，后续统一收齐两个结果再判定。
	select {
	case result := <-results:
		remaining--
		if result.Success {
			t.Error("真实 Redis 正在使用的 state 被再次接受")
		}
	case <-entered:
		t.Error("真实 Redis 同 state 触发第二次授权码交换")
	case <-time.After(3 * time.Second):
		t.Fatal("真实 Redis 同 state 没有被及时拒绝")
	}
	unblock()
	successes := 0
	for i := 0; i < remaining; i++ {
		select {
		case result := <-results:
			if result.Success {
				successes++
			}
		case <-time.After(3 * time.Second):
			t.Fatal("真实 Redis 并发恢复未结束")
		}
	}
	if successes != 1 || exchanges.Load() != 1 {
		t.Errorf("真实 Redis 恢复成功数=%d，exchange=%d want=1", successes, exchanges.Load())
	}
	existsCtx2, cancelExists2 := context.WithTimeout(context.Background(), time.Second)
	defer cancelExists2()
	if n, err := client.Exists(existsCtx2, stateKey).Result(); err != nil || n != 0 {
		t.Errorf("消费后 state 仍留在真实 Redis：n=%d err=%v", n, err)
	}
	again := call("exchange-code", callback, "i003-access-0")
	if again.Success || exchanges.Load() != 1 {
		t.Error("真实 Redis 消费后同一 state 仍可再次交换")
	}
	snapshot, err := model.LoadCredentialRotationSnapshot(t.Context(), channel.Id)
	if err != nil || snapshot.Revision != 1 || snapshot.Fence != nil || snapshot.StartedAt != nil {
		t.Error("真实 Redis 消费路径未唯一提交新凭据revision")
	}
	credentials, err := codex.FromJSON(snapshot.Key)
	if err != nil || credentials.AccessToken != newAccess || credentials.RefreshToken != "i003-redis-new-refresh" {
		t.Error("真实 Redis 消费路径未保存交换结果凭据")
	}
}
