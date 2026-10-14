package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/authutil"
	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/credentials"
	"one-api/common/requester"
	commonTest "one-api/common/test"
	"one-api/middleware"
	"one-api/model"
	"one-api/providers/antigravity"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

type antigravityOAuthRoundTripper func(*http.Request) (*http.Response, error)

func (f antigravityOAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type antigravityOAuthTestServer struct {
	tokenCalls atomic.Int32
	modelCalls atomic.Int32
	modelAuth  string
	subject    string
	project    string
	tokenForm  url.Values
	onToken    func()
}

func setupAntigravityOAuthTest(t *testing.T) *antigravityOAuthTestServer {
	t.Helper()
	gin.SetMode(gin.TestMode)
	useControllerChannelTagTestDB(t)
	oldRedis, oldClient := config.RedisEnabled, requester.HTTPClient
	config.RedisEnabled = false
	cache.InitCacheManager()
	t.Cleanup(func() {
		config.RedisEnabled, requester.HTTPClient = oldRedis, oldClient
		cache.InitCacheManager()
	})
	server := &antigravityOAuthTestServer{subject: "google-subject", project: "project-one"}
	requester.HTTPClient = &http.Client{Transport: antigravityOAuthRoundTripper(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.URL.Host + req.URL.Path {
		case "oauth2.googleapis.com/token":
			server.tokenCalls.Add(1)
			raw, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			server.tokenForm, err = url.ParseQuery(string(raw))
			if err != nil {
				t.Fatal(err)
			}
			if server.onToken != nil {
				server.onToken()
			}
			body = `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600,"token_type":"Bearer"}`
		case "www.googleapis.com/oauth2/v2/userinfo":
			data, _ := json.Marshal(map[string]string{"id": server.subject, "email": "admin@example.test"})
			body = string(data)
		case "cloudcode-pa.googleapis.com/v1internal:loadCodeAssist":
			data, _ := json.Marshal(map[string]string{"cloudaicompanionProject": server.project})
			body = string(data)
		case "cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels", "daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels":
			server.modelCalls.Add(1)
			server.modelAuth = req.Header.Get("Authorization")
			body = `{"models":{"gemini-example":{},"claude-example":{}}}`
		default:
			t.Fatalf("unexpected upstream request: %s", req.URL.Redacted())
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	return server
}

func callAntigravityOAuth(t *testing.T, handler gin.HandlerFunc, adminID int, payload any) (codexOAuthAPIResult, *httptest.ResponseRecorder) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	c, recorder := commonTest.GetContext(http.MethodPost, "/api/antigravity/oauth", commonTest.RequestJSONConfig(), bytes.NewBuffer(raw))
	c.Set("id", adminID)
	handler(c)
	var result codexOAuthAPIResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("OAuth responses must not be cached")
	}
	return result, recorder
}

func startAntigravityOAuthTest(t *testing.T, adminID, channelID int, proxy ...string) (antigravityOAuthState, map[string]any) {
	t.Helper()
	payload := map[string]any{"channel_id": channelID}
	if len(proxy) > 0 {
		payload["proxy"] = proxy[0]
	}
	result, _ := callAntigravityOAuth(t, StartAntigravityOAuth, adminID, payload)
	if !result.Success {
		t.Fatalf("OAuth start failed: %s", result.Message)
	}
	var data map[string]any
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	sessionID := data["session_id"].(string)
	state, err := cache.GetCache[antigravityOAuthState](antigravityOAuthCacheKey(adminID, sessionID))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(data["auth_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("state") != sessionID || u.Query().Get("code_challenge") != authutil.PKCEChallenge(state.CodeVerifier) || u.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("authorization URL does not bind state and PKCE")
	}
	return state, data
}

func antigravityCallbackPayload(state antigravityOAuthState) map[string]any {
	query := url.Values{"state": {state.State}, "code": {"one-time-code"}}
	return map[string]any{"session_id": state.State, "callback_url": state.RedirectURI + "?" + query.Encode()}
}

func TestAntigravityOAuthDraftIsOneTimeAndBoundToAdministrator(t *testing.T) {
	server := setupAntigravityOAuthTest(t)
	state, _ := startAntigravityOAuthTest(t, 1, 0)
	payload := antigravityCallbackPayload(state)
	wrongAdmin, _ := callAntigravityOAuth(t, AntigravityOAuthCallback, 2, payload)
	if wrongAdmin.Success || server.tokenCalls.Load() != 0 {
		t.Fatal("another administrator must not exchange or consume this session")
	}
	result, _ := callAntigravityOAuth(t, AntigravityOAuthCallback, 1, payload)
	if !result.Success {
		t.Fatalf("exchange failed: %s", result.Message)
	}
	var data struct {
		Key       string `json:"key"`
		Operation string `json:"operation"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	if _, err := antigravity.ParseCredentials(data.Key); err != nil || data.Operation != "create" {
		t.Fatalf("invalid credential draft: %v", err)
	}
	if server.tokenForm.Get("code_verifier") != state.CodeVerifier || server.tokenForm.Get("redirect_uri") != state.RedirectURI {
		t.Fatal("exchange did not use the saved verifier and redirect")
	}
	repeated, _ := callAntigravityOAuth(t, AntigravityOAuthCallback, 1, payload)
	if repeated.Success || server.tokenCalls.Load() != 1 {
		t.Fatal("an OAuth code must be exchanged at most once")
	}
	var count int64
	if err := model.DB.Model(&model.Channel{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatal("draft exchange must not create a routable channel")
	}
}

func TestAntigravityOAuthRejectsInvalidCallbackBeforeDispatch(t *testing.T) {
	for _, scenario := range []string{"wrong_state", "duplicate_state", "duplicate_code", "wrong_host", "wrong_path", "fragment", "raw_code", "expired", "wrong_admin_binding"} {
		t.Run(scenario, func(t *testing.T) {
			server := setupAntigravityOAuthTest(t)
			state, _ := startAntigravityOAuthTest(t, 1, 0)
			payload := antigravityCallbackPayload(state)
			callback := payload["callback_url"].(string)
			switch scenario {
			case "wrong_state":
				callback = strings.Replace(callback, state.State, "other-session", 1)
			case "duplicate_state":
				callback += "&state=" + state.State
			case "duplicate_code":
				callback += "&code=other-code"
			case "wrong_host":
				callback = strings.Replace(callback, "localhost", "example.test", 1)
			case "wrong_path":
				callback = strings.Replace(callback, "/oauth-callback", "/different", 1)
			case "fragment":
				callback += "#fragment"
			case "raw_code":
				callback = "a-raw-authorization-code"
			case "expired":
				state.CreatedAt = time.Now().Add(-antigravityOAuthStateTTL).Unix()
			case "wrong_admin_binding":
				state.AdminID = 2
			}
			if err := cache.SetCache(antigravityOAuthCacheKey(1, state.State), state, antigravityOAuthStateTTL); err != nil {
				t.Fatal(err)
			}
			payload["callback_url"] = callback
			result, _ := callAntigravityOAuth(t, AntigravityOAuthCallback, 1, payload)
			if result.Success || server.tokenCalls.Load() != 0 {
				t.Fatal("invalid callback reached Google exchange")
			}
			if _, err := cache.GetCache[antigravityOAuthState](antigravityOAuthCacheKey(1, state.State)); err == nil {
				t.Fatal("failed callback must still consume the session")
			}
		})
	}
}

func TestAntigravityOAuthConcurrentCallbacksExchangeOnce(t *testing.T) {
	server := setupAntigravityOAuthTest(t)
	state, _ := startAntigravityOAuthTest(t, 1, 0)
	payload := antigravityCallbackPayload(state)
	var successes atomic.Int32
	var callers sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			<-start
			result, _ := callAntigravityOAuth(t, AntigravityOAuthCallback, 1, payload)
			if result.Success {
				successes.Add(1)
			}
		}()
	}
	close(start)
	callers.Wait()
	if successes.Load() != 1 || server.tokenCalls.Load() != 1 {
		t.Fatalf("callbacks must have one winner: successes=%d exchanges=%d", successes.Load(), server.tokenCalls.Load())
	}
}

func TestAntigravityOAuthSessionsAreIndependentAndBounded(t *testing.T) {
	server := setupAntigravityOAuthTest(t)
	first, _ := startAntigravityOAuthTest(t, 1, 0)
	second, _ := startAntigravityOAuthTest(t, 1, 0)
	if first.State == second.State {
		t.Fatal("independent starts must have different state")
	}
	for _, state := range []antigravityOAuthState{first, second} {
		result, _ := callAntigravityOAuth(t, AntigravityOAuthCallback, 1, antigravityCallbackPayload(state))
		if !result.Success {
			t.Fatal("a newer start must not replace an older independent session")
		}
	}
	if server.tokenCalls.Load() != 2 {
		t.Fatal("both independent sessions should exchange")
	}
	result, _ := callAntigravityOAuth(t, StartAntigravityOAuth, 1, map[string]string{"proxy": "http://proxy.example/" + strings.Repeat("a", antigravityOAuthStateLimit)})
	if result.Success || server.tokenCalls.Load() != 2 {
		t.Fatal("oversized session must be rejected locally")
	}
}

func antigravityOAuthChannelFixture(t *testing.T, fence bool) model.Channel {
	t.Helper()
	credential := &antigravity.Credentials{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), Subject: "google-subject", ProjectID: "project-one"}
	key, err := credential.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	channel := model.Channel{Id: 71, Type: config.ChannelTypeAntigravity, Key: key, Name: "original", Models: "test-model", Group: "default", Status: config.ChannelStatusEnabled}
	if fence {
		channel.InternalState, err = credentials.WriteRefresh(nil, &credentials.Refresh{AttemptID: "pending-refresh", StartedAt: time.Now().Unix()})
		if err != nil {
			t.Fatal(err)
		}
		channel.Version = 8
	}
	if err := model.DB.Create(&channel).Error; err != nil {
		t.Fatal(err)
	}
	return channel
}

func TestAntigravityOAuthEditReturnsDraftAtOriginalVersion(t *testing.T) {
	server := setupAntigravityOAuthTest(t)
	channel := antigravityOAuthChannelFixture(t, false)
	server.subject = "replacement-subject"
	state, data := startAntigravityOAuthTest(t, 1, channel.Id)
	if data["operation"] != "edit" || state.Version != channel.Version {
		t.Fatal("edit authorization must retain its original version")
	}
	result, _ := callAntigravityOAuth(t, AntigravityOAuthCallback, 1, antigravityCallbackPayload(state))
	if !result.Success {
		t.Fatal(result.Message)
	}
	var draft struct {
		Key             string `json:"key"`
		ExpectedVersion uint64 `json:"expected_version"`
	}
	if err := json.Unmarshal(result.Data, &draft); err != nil {
		t.Fatal(err)
	}
	parsed, err := antigravity.ParseCredentials(draft.Key)
	if err != nil || parsed.Subject != server.subject || draft.ExpectedVersion != state.Version {
		t.Fatal("edit must permit independent authorization while preserving save version")
	}
	persisted, err := model.GetChannelById(channel.Id)
	if err != nil || persisted.Key != channel.Key || persisted.Version != channel.Version {
		t.Fatal("draft exchange must not mutate the channel")
	}
}

func TestAntigravityOAuthUsesExpandedProxySnapshot(t *testing.T) {
	for _, operation := range []string{"create", "edit", "recover"} {
		for _, proxy := range []struct {
			name  string
			value string
		}{
			{name: "direct"},
			{name: "fixed", value: "http://proxy.example:8080"},
			{name: "session_template", value: "http://user-session-%s:password@proxy.example:8080"},
		} {
			t.Run(operation+"/"+proxy.name, func(t *testing.T) {
				server := setupAntigravityOAuthTest(t)
				channel := model.Channel{}
				if operation != "create" {
					channel = antigravityOAuthChannelFixture(t, operation == "recover")
					if err := model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).UpdateColumn("proxy", proxy.value).Error; err != nil {
						t.Fatal(err)
					}
				}
				expectedProxy := proxy.value
				channel.Proxy = &expectedProxy
				channel.SetProxy()
				state, _ := startAntigravityOAuthTest(t, 1, channel.Id, proxy.value)
				if state.Operation != operation || state.Proxy != expectedProxy {
					t.Fatalf("授权会话未保存渠道的实际代理：operation=%q proxy=%q", state.Operation, state.Proxy)
				}
				result, _ := callAntigravityOAuth(t, AntigravityOAuthCallback, 1, antigravityCallbackPayload(state))
				if !result.Success || server.tokenCalls.Load() != 1 {
					t.Fatalf("授权交换失败：%s", result.Message)
				}
				if operation == "create" {
					var draft struct {
						Key string `json:"key"`
					}
					if err := json.Unmarshal(result.Data, &draft); err != nil {
						t.Fatal(err)
					}
					body, err := json.Marshal(model.Channel{
						Type: config.ChannelTypeAntigravity, Key: draft.Key, Proxy: &proxy.value,
						Name: "google", Models: "test-model", Group: "default",
					})
					if err != nil {
						t.Fatal(err)
					}
					c, recorder := commonTest.GetContext(http.MethodPost, "/api/channel", commonTest.RequestJSONConfig(), bytes.NewBuffer(body))
					AddChannel(c)
					var created codexOAuthAPIResult
					if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
						t.Fatal(err)
					}
					if !created.Success {
						t.Fatalf("保存授权草稿失败：%s", created.Message)
					}
					if err := model.DB.First(&channel).Error; err != nil {
						t.Fatal(err)
					}
				}
				persisted, err := model.GetChannelById(channel.Id)
				if err != nil {
					t.Fatal(err)
				}
				if persisted.Proxy == nil || *persisted.Proxy != proxy.value {
					t.Fatal("授权流程修改了已保存的代理模板")
				}
			})
		}
	}
}

func TestAntigravityOAuthRecoveryPreservesIdentityAndSnapshot(t *testing.T) {
	for _, scenario := range []string{"same_identity", "different_subject", "different_project", "version_changed_before_exchange", "version_changed_during_exchange", "fence_changed"} {
		t.Run(scenario, func(t *testing.T) {
			server := setupAntigravityOAuthTest(t)
			channel := antigravityOAuthChannelFixture(t, true)
			state, _ := startAntigravityOAuthTest(t, 1, channel.Id)
			if state.Operation != "recover" {
				t.Fatal("pending refresh must fix authorization to recovery")
			}
			advanceVersion := func() {
				if err := model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).UpdateColumn("version", channel.Version+1).Error; err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "different_subject":
				server.subject = "different-subject"
			case "different_project":
				server.project = "different-project"
			case "version_changed_before_exchange":
				advanceVersion()
			case "version_changed_during_exchange":
				server.onToken = advanceVersion
			case "fence_changed":
				internal, _ := credentials.WriteRefresh(nil, &credentials.Refresh{AttemptID: "new-attempt", StartedAt: time.Now().Unix()})
				if err := model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).UpdateColumn("internal_state", internal).Error; err != nil {
					t.Fatal(err)
				}
			}
			result, _ := callAntigravityOAuth(t, AntigravityOAuthCallback, 1, antigravityCallbackPayload(state))
			persisted, err := model.GetChannelById(channel.Id)
			if err != nil {
				t.Fatal(err)
			}
			refresh, err := credentials.ReadRefresh(persisted.InternalState)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "same_identity" {
				if !result.Success || refresh != nil || persisted.Version != channel.Version+1 || persisted.Key == channel.Key || bytes.Contains(result.Data, []byte("access_token")) || bytes.Contains(result.Data, []byte(`"key"`)) {
					t.Fatalf("recovery must commit once and return no credentials: %s", result.Message)
				}
			} else if result.Success || refresh == nil || persisted.Key != channel.Key {
				t.Fatalf("conflicting recovery changed credentials: %s", scenario)
			}
			if (scenario == "version_changed_before_exchange" || scenario == "fence_changed") && server.tokenCalls.Load() != 0 {
				t.Fatal("known stale snapshot must fail before token exchange")
			}
		})
	}
}

func TestAntigravityOAuthRequiresAdministratorRoute(t *testing.T) {
	setupAntigravityOAuthTest(t)
	router := gin.New()
	router.Use(sessions.Sessions("session", cookie.NewStore([]byte("antigravity-oauth-test-cookie-key"))))
	group := router.Group("/oauth", middleware.AdminAuth())
	group.POST("/start", StartAntigravityOAuth)
	group.POST("/exchange-code", AntigravityOAuthCallback)
	for _, path := range []string{"start", "exchange-code"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/oauth/"+path, strings.NewReader(`{}`)))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s returned %d", path, recorder.Code)
		}
	}
}

func TestAntigravityOAuthSnapshotRejectsInvalidOperation(t *testing.T) {
	for _, state := range []antigravityOAuthState{{Operation: "recover"}, {Operation: "unknown"}, {Operation: "create", ChannelID: 1}} {
		if _, err := antigravityOAuthSnapshot(context.Background(), state); err == nil {
			t.Fatal("invalid server operation must fail closed")
		}
	}
}
