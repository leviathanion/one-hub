package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common"
	"one-api/common/authutil"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useTestAuthDB(t *testing.T) {
	t.Helper()

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.User{}, &model.Token{}); err != nil {
		t.Fatalf("expected auth schema migration, got %v", err)
	}

	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
	})

	originalTokenSecret := viper.GetString("user_token_secret")
	originalHashidsSalt := viper.GetString("hashids_salt")
	viper.Set("user_token_secret", "middleware-auth-test-secret")
	viper.Set("hashids_salt", "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890")
	if err := common.InitUserToken(); err != nil {
		t.Fatalf("expected test user-token helpers to initialize, got %v", err)
	}
	t.Cleanup(func() {
		viper.Set("user_token_secret", originalTokenSecret)
		viper.Set("hashids_salt", originalHashidsSalt)
		_ = common.InitUserToken()
	})
}

func newAuthTestContext(method, target string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, target, nil)
	return ctx, recorder
}

func createAuthTestToken(t *testing.T, username string, role int) *model.Token {
	t.Helper()

	user := &model.User{
		Username:    username,
		Password:    "password123",
		DisplayName: username,
		Role:        role,
		Status:      config.UserStatusEnabled,
		AccessToken: username + "-access",
		Group:       "default",
		AffCode:     username + "-aff",
	}
	if err := model.DB.Create(user).Error; err != nil {
		t.Fatalf("expected auth test user to persist, got %v", err)
	}

	token := &model.Token{
		UserId:      user.Id,
		Status:      config.TokenStatusEnabled,
		Name:        username + "-token",
		ExpiredTime: -1,
		RemainQuota: 100,
		Group:       "default",
	}
	if err := model.DB.Create(token).Error; err != nil {
		t.Fatalf("expected auth test token to persist, got %v", err)
	}
	if token.Key == "" {
		t.Fatal("expected auth test token key to be generated")
	}

	return token
}

func TestAuthWrappersRejectShortCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	openAICtx, openAIRecorder := newAuthTestContext(http.MethodGet, "/v1/chat/completions")
	openAICtx.Request.Header.Set("Authorization", "Bearer short-key")
	OpenaiAuth()(openAICtx)
	if openAIRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected OpenAI auth wrapper to reject short credentials, got %d", openAIRecorder.Code)
	}

	claudeCtx, claudeRecorder := newAuthTestContext(http.MethodGet, "/claude")
	claudeCtx.Request.Header.Set("x-api-key", "short-key")
	ClaudeAuth()(claudeCtx)
	if claudeRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected Claude auth wrapper to reject short credentials, got %d", claudeRecorder.Code)
	}

	geminiCtx, geminiRecorder := newAuthTestContext(http.MethodGet, "/gemini?key=short-key")
	GeminiAuth()(geminiCtx)
	if geminiRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected Gemini auth wrapper to reject short credentials, got %d", geminiRecorder.Code)
	}

	mjCtx, mjRecorder := newAuthTestContext(http.MethodGet, "/mj/mj-fast")
	mjCtx.Params = gin.Params{{Key: "mode", Value: "mj-fast"}}
	mjCtx.Request.Header.Set("mj-api-secret", "short-key")
	MjAuth()(mjCtx)
	if mjRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected Midjourney auth wrapper to reject short credentials, got %d", mjRecorder.Code)
	}
	if got := mjCtx.GetString("mj_model"); got != "fast" {
		t.Fatalf("expected Midjourney mode normalization before token auth, got %q", got)
	}
}

func TestTokenAuthAdminSelectorBranches(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useTestAuthDB(t)
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	originalRedisEnabled := config.RedisEnabled
	config.RedisEnabled = false
	t.Cleanup(func() {
		config.RedisEnabled = originalRedisEnabled
	})

	adminToken := createAuthTestToken(t, "admin-user", config.RoleAdminUser)
	commonToken := createAuthTestToken(t, "common-user", config.RoleCommonUser)

	skipCtx, _ := newAuthTestContext(http.MethodGet, "/v1/chat/completions")
	tokenAuth(skipCtx, authutil.Credential{
		Value:         adminToken.Key,
		SelectorParts: []string{"!7"},
	})
	gotValue, ok := skipCtx.Get("skip_channel_ids")
	got, ok := gotValue.([]int)
	if !ok || len(got) != 1 || got[0] != 7 {
		t.Fatalf("expected admin skip selector to populate skip_channel_ids, got %+v", got)
	}

	pinnedCtx, _ := newAuthTestContext(http.MethodGet, "/v1/chat/completions")
	tokenAuth(pinnedCtx, authutil.Credential{
		Value:         adminToken.Key,
		SelectorParts: []string{"9", "ignore"},
	})
	if got := pinnedCtx.GetInt("specific_channel_id"); got != 9 {
		t.Fatalf("expected admin selector to pin specific channel 9, got %d", got)
	}
	if !pinnedCtx.GetBool("specific_channel_id_ignore") {
		t.Fatal("expected admin selector to mark affinity ignore mode")
	}

	invalidCtx, invalidRecorder := newAuthTestContext(http.MethodGet, "/v1/chat/completions")
	tokenAuth(invalidCtx, authutil.Credential{
		Value:         adminToken.Key,
		SelectorParts: []string{"not-a-channel"},
	})
	if invalidRecorder.Code != http.StatusForbidden {
		t.Fatalf("expected invalid admin channel selectors to be rejected, got %d", invalidRecorder.Code)
	}

	commonCtx, commonRecorder := newAuthTestContext(http.MethodGet, "/v1/chat/completions")
	tokenAuth(commonCtx, authutil.Credential{
		Value:         commonToken.Key,
		SelectorParts: []string{"9"},
	})
	if commonRecorder.Code != http.StatusForbidden {
		t.Fatalf("expected non-admin channel selectors to be rejected, got %d", commonRecorder.Code)
	}
}
