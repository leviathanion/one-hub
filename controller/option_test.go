package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/model"
	"one-api/providers/codex"
	runtimeaffinity "one-api/runtime/channelaffinity"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useControllerTestOptionDB(t *testing.T) {
	t.Helper()

	originalDB := model.DB
	originalOptionManager := config.GlobalOption
	originalGitHubOAuthEnabled := config.GitHubOAuthEnabled
	originalGitHubClientID := config.GitHubClientId
	originalGitHubClientSecret := config.GitHubClientSecret
	originalGitHubOldIDCloseEnabled := config.GitHubOldIdCloseEnabled
	originalWeChatAuthEnabled := config.WeChatAuthEnabled
	originalWeChatServerAddress := config.WeChatServerAddress
	originalWeChatServerToken := config.WeChatServerToken
	originalWeChatQRCode := config.WeChatAccountQRCodeImageURL
	originalLarkAuthEnabled := config.LarkAuthEnabled
	originalLarkClientID := config.LarkClientId
	originalLarkClientSecret := config.LarkClientSecret
	originalOIDCAuthEnabled := config.OIDCAuthEnabled
	originalOIDCClientID := config.OIDCClientId
	originalOIDCClientSecret := config.OIDCClientSecret
	originalOIDCIssuer := config.OIDCIssuer
	originalOIDCScopes := config.OIDCScopes
	originalOIDCUsernameClaims := config.OIDCUsernameClaims
	originalTurnstileEnabled := config.TurnstileCheckEnabled
	originalTurnstileSiteKey := config.TurnstileSiteKey
	originalTurnstileSecretKey := config.TurnstileSecretKey
	originalEmailDomainRestrictionEnabled := config.EmailDomainRestrictionEnabled
	originalEmailDomainWhitelist := append([]string(nil), config.EmailDomainWhitelist...)

	testDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.Option{}); err != nil {
		t.Fatalf("expected option schema migration, got %v", err)
	}
	if err := testDB.Exec("DELETE FROM options").Error; err != nil {
		t.Fatalf("expected option table reset, got %v", err)
	}

	model.DB = testDB
	config.GlobalOption = config.NewOptionManager()
	config.GitHubOAuthEnabled = false
	config.GitHubClientId = ""
	config.GitHubClientSecret = ""
	config.GitHubOldIdCloseEnabled = false
	config.WeChatAuthEnabled = false
	config.WeChatServerAddress = ""
	config.WeChatServerToken = ""
	config.WeChatAccountQRCodeImageURL = ""
	config.LarkAuthEnabled = false
	config.LarkClientId = ""
	config.LarkClientSecret = ""
	config.OIDCAuthEnabled = false
	config.OIDCClientId = ""
	config.OIDCClientSecret = ""
	config.OIDCIssuer = ""
	config.OIDCScopes = ""
	config.OIDCUsernameClaims = ""
	config.TurnstileCheckEnabled = false
	config.TurnstileSiteKey = ""
	config.TurnstileSecretKey = ""
	config.EmailDomainRestrictionEnabled = false
	config.EmailDomainWhitelist = append([]string(nil), originalEmailDomainWhitelist...)
	model.InitOptionMap()
	config.GlobalOption.RegisterCustomOptionWithValidator("ChannelAffinitySetting", func() string {
		return config.ChannelAffinitySettingsInstance.JSONString()
	}, func(value string) error {
		return config.ChannelAffinitySettingsInstance.SetFromJSON(value)
	}, func(value string) error {
		settings := config.DefaultChannelAffinitySettings()
		return settings.SetFromJSON(value)
	}, config.OptionMetadata{
		Visibility: config.OptionVisibilityPublic,
	}, config.ChannelAffinitySettingsInstance.JSONString())
	config.GlobalOption.RegisterCustomOptionWithValidator("GeminiOpenThink", func() string {
		return config.GeminiSettingsInstance.GetOpenThinkJSONString()
	}, func(value string) error {
		return config.GeminiSettingsInstance.SetOpenThink(value)
	}, func(value string) error {
		return config.ValidateGeminiOpenThink(value)
	}, config.OptionMetadata{
		Visibility: config.OptionVisibilityPublic,
	}, "")
	config.GlobalOption.RegisterCustomOptionWithValidator("CodexRoutingHintSetting", func() string {
		return codex.RoutingHintSettingsInstance.JSONString()
	}, func(value string) error {
		return codex.RoutingHintSettingsInstance.SetFromJSON(value)
	}, func(value string) error {
		settings := codex.DefaultRoutingHintSettings()
		return settings.SetFromJSON(value)
	}, config.OptionMetadata{
		Visibility: config.OptionVisibilityPublic,
	}, codex.RoutingHintSettingsInstance.JSONString())

	t.Cleanup(func() {
		model.DB = originalDB
		config.GlobalOption = originalOptionManager
		config.GitHubOAuthEnabled = originalGitHubOAuthEnabled
		config.GitHubClientId = originalGitHubClientID
		config.GitHubClientSecret = originalGitHubClientSecret
		config.GitHubOldIdCloseEnabled = originalGitHubOldIDCloseEnabled
		config.WeChatAuthEnabled = originalWeChatAuthEnabled
		config.WeChatServerAddress = originalWeChatServerAddress
		config.WeChatServerToken = originalWeChatServerToken
		config.WeChatAccountQRCodeImageURL = originalWeChatQRCode
		config.LarkAuthEnabled = originalLarkAuthEnabled
		config.LarkClientId = originalLarkClientID
		config.LarkClientSecret = originalLarkClientSecret
		config.OIDCAuthEnabled = originalOIDCAuthEnabled
		config.OIDCClientId = originalOIDCClientID
		config.OIDCClientSecret = originalOIDCClientSecret
		config.OIDCIssuer = originalOIDCIssuer
		config.OIDCScopes = originalOIDCScopes
		config.OIDCUsernameClaims = originalOIDCUsernameClaims
		config.TurnstileCheckEnabled = originalTurnstileEnabled
		config.TurnstileSiteKey = originalTurnstileSiteKey
		config.TurnstileSecretKey = originalTurnstileSecretKey
		config.EmailDomainRestrictionEnabled = originalEmailDomainRestrictionEnabled
		config.EmailDomainWhitelist = append([]string(nil), originalEmailDomainWhitelist...)
	})
}

func seedControllerTestOptions(t *testing.T, values map[string]string) {
	t.Helper()

	options := make([]model.Option, 0, len(values))
	for key, value := range values {
		normalizedKey := config.GlobalOption.NormalizeKey(key)
		options = append(options, model.Option{
			Key:   normalizedKey,
			Value: value,
		})
	}
	if err := model.SaveOptionsTx(model.DB, options); err != nil {
		t.Fatalf("expected raw option seed to persist, got %v", err)
	}
	for _, option := range options {
		if err := config.GlobalOption.Set(option.Key, option.Value); err != nil {
			t.Fatalf("expected in-memory option seed for %s, got %v", option.Key, err)
		}
	}
}

func TestGetOptionsIncludesSensitiveOptionStatusesWithoutValues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	if err := model.UpdateOption("GitHubClientId", "cli_test"); err != nil {
		t.Fatalf("expected github client id seed to persist, got %v", err)
	}
	if err := model.UpdateOption("GitHubClientSecret", "sec_test"); err != nil {
		t.Fatalf("expected github client secret seed to persist, got %v", err)
	}
	if err := model.UpdateOption("CFWorkerImageKey", "cf-secret"); err != nil {
		t.Fatalf("expected cf worker image key seed to persist, got %v", err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/option/", nil)

	GetOptions(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 status, got %d", recorder.Code)
	}

	var payload struct {
		Success bool           `json:"success"`
		Data    []model.Option `json:"data"`
		Meta    struct {
			SensitiveOptions map[string]config.SensitiveOptionStatus `json:"sensitive_options"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if !payload.Success {
		t.Fatalf("expected success payload, got %#v", payload)
	}

	for _, option := range payload.Data {
		if option.Key == "GitHubClientSecret" || option.Key == "CFWorkerImageKey" {
			t.Fatalf("expected sensitive options to be omitted from public payload, got %#v", payload.Data)
		}
	}
	if !payload.Meta.SensitiveOptions["GitHubClientSecret"].Configured {
		t.Fatalf("expected GitHubClientSecret configured status, got %#v", payload.Meta.SensitiveOptions)
	}
	if !payload.Meta.SensitiveOptions["CFWorkerImageKey"].Configured {
		t.Fatalf("expected CFWorkerImageKey configured status, got %#v", payload.Meta.SensitiveOptions)
	}
	if payload.Meta.SensitiveOptions["SMTPToken"].Configured {
		t.Fatalf("expected empty SMTPToken to report unconfigured, got %#v", payload.Meta.SensitiveOptions["SMTPToken"])
	}
}

func TestUpdateOptionAllowsClearingSensitiveOption(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	if err := model.UpdateOption("CFWorkerImageKey", "cf-secret"); err != nil {
		t.Fatalf("expected cf worker image key seed to persist, got %v", err)
	}

	body := bytes.NewBufferString(`{"key":"CFWorkerImageKey","value":""}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/", body)

	UpdateOption(ctx)

	var payload struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if !payload.Success {
		t.Fatalf("expected sensitive option clear to succeed, got %#v", payload)
	}
	if got := config.GlobalOption.Get("CFWorkerImageKey"); got != "" {
		t.Fatalf("expected in-memory secret to be cleared, got %q", got)
	}

	stored, err := model.GetOption("CFWorkerImageKey")
	if err != nil {
		t.Fatalf("expected stored cf worker image key lookup to succeed, got %v", err)
	}
	if stored.Value != "" {
		t.Fatalf("expected stored secret to be cleared, got %q", stored.Value)
	}
}

func TestUpdateOptionBatchRejectsClearingGitHubSecretWhileOAuthEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	if err := model.UpdateOption("GitHubClientId", "cli_seed"); err != nil {
		t.Fatalf("expected github client id seed to persist, got %v", err)
	}
	if err := model.UpdateOption("GitHubClientSecret", "sec_seed"); err != nil {
		t.Fatalf("expected github client secret seed to persist, got %v", err)
	}
	if err := model.UpdateOption("GitHubOAuthEnabled", "true"); err != nil {
		t.Fatalf("expected github oauth seed to persist, got %v", err)
	}

	body := bytes.NewBufferString(`{"updates":[{"key":"GitHubClientSecret","value":""}]}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/batch", body)

	UpdateOptionBatch(ctx)

	var payload struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Success {
		t.Fatalf("expected github secret clear while enabled to fail, got %#v", payload)
	}
	if payload.Data["failed_key"] != "GitHubOAuthEnabled" {
		t.Fatalf("expected failed_key GitHubOAuthEnabled, got %#v", payload.Data["failed_key"])
	}
	if got := config.GlobalOption.Get("GitHubClientSecret"); got != "sec_seed" {
		t.Fatalf("expected failed batch to preserve secret, got %q", got)
	}
}

func TestUpdateOptionBatchAllowsDisablingGitHubOAuthAndClearingSecretTogether(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	if err := model.UpdateOption("GitHubClientId", "cli_seed"); err != nil {
		t.Fatalf("expected github client id seed to persist, got %v", err)
	}
	if err := model.UpdateOption("GitHubClientSecret", "sec_seed"); err != nil {
		t.Fatalf("expected github client secret seed to persist, got %v", err)
	}
	if err := model.UpdateOption("GitHubOAuthEnabled", "true"); err != nil {
		t.Fatalf("expected github oauth seed to persist, got %v", err)
	}

	body := bytes.NewBufferString(`{"updates":[{"key":"GitHubOAuthEnabled","value":"false"},{"key":"GitHubClientSecret","value":""}]}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/batch", body)

	UpdateOptionBatch(ctx)

	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			UpdatedKeys []string `json:"updated_keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if !payload.Success {
		t.Fatalf("expected github disable and secret clear batch to succeed, got %#v", payload)
	}
	if got := config.GlobalOption.Get("GitHubOAuthEnabled"); got != "false" {
		t.Fatalf("expected github oauth to be disabled, got %q", got)
	}
	if got := config.GlobalOption.Get("GitHubClientSecret"); got != "" {
		t.Fatalf("expected github secret to be cleared, got %q", got)
	}
}

func TestUpdateOptionBatchRejectsEnablingGitHubOAuthWithoutSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	body := bytes.NewBufferString(`{"updates":[{"key":"GitHubClientId","value":"cli_a"},{"key":"GitHubOAuthEnabled","value":"true"}]}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/batch", body)

	UpdateOptionBatch(ctx)

	var payload struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Success {
		t.Fatalf("expected github enablement without secret to fail, got %#v", payload)
	}
	if payload.Data["failed_key"] != "GitHubOAuthEnabled" {
		t.Fatalf("expected failed_key GitHubOAuthEnabled, got %#v", payload.Data["failed_key"])
	}
	if _, err := model.GetOption("GitHubClientId"); err == nil {
		t.Fatal("expected failed github batch to avoid partial persistence")
	}
	if got := config.GlobalOption.Get("GitHubOAuthEnabled"); got != "false" {
		t.Fatalf("expected failed github batch to preserve disabled auth state, got %q", got)
	}
}

func TestUpdateOptionBatchRejectsIncrementalGitHubRepairWhenConfigStillInvalid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	seedControllerTestOptions(t, map[string]string{
		"GitHubOAuthEnabled": "true",
	})

	body := bytes.NewBufferString(`{"updates":[{"key":"GitHubClientId","value":"cli_partial"}]}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/batch", body)

	UpdateOptionBatch(ctx)

	var payload struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Success {
		t.Fatalf("expected partial github repair batch to fail, got %#v", payload)
	}
	if payload.Data["failed_key"] != "GitHubOAuthEnabled" {
		t.Fatalf("expected failed_key GitHubOAuthEnabled, got %#v", payload.Data["failed_key"])
	}
	if got := config.GlobalOption.Get("GitHubClientId"); got != "" {
		t.Fatalf("expected failed batch to preserve empty github client id, got %q", got)
	}
	if _, err := model.GetOption("GitHubClientId"); err == nil {
		t.Fatal("expected failed github repair batch to avoid partial persistence")
	}
}

func TestGetExecutionSessionCacheReturnsStatsPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/option/execution_session_cache", nil)

	GetExecutionSessionCache(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200 status, got %d", recorder.Code)
	}

	var payload struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if !payload.Success {
		t.Fatalf("expected success response, got %#v", payload)
	}
	if payload.Data["managed_provider"] != "codex" {
		t.Fatalf("expected managed_provider=codex, got %#v", payload.Data["managed_provider"])
	}
	if _, ok := payload.Data["backend"]; !ok {
		t.Fatalf("expected backend field in payload, got %#v", payload.Data)
	}
	if _, ok := payload.Data["local_sessions"]; !ok {
		t.Fatalf("expected local_sessions field in payload, got %#v", payload.Data)
	}
	if _, ok := payload.Data["backend_bindings"]; !ok {
		t.Fatalf("expected backend_bindings field in payload, got %#v", payload.Data)
	}
}

func TestGetChannelAffinityCacheReturnsSettingsAndStats(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalSettings := config.ChannelAffinitySettingsInstance
	config.ChannelAffinitySettingsInstance = config.ChannelAffinitySettings{
		Enabled:           true,
		DefaultTTLSeconds: 120,
		MaxEntries:        7,
		Rules: []config.ChannelAffinityRule{
			{Name: "realtime", Enabled: true, Kind: "realtime"},
		},
	}
	t.Cleanup(func() {
		config.ChannelAffinitySettingsInstance = originalSettings
	})

	manager := runtimeaffinity.ConfigureDefault(runtimeaffinity.ManagerOptions{
		DefaultTTL:      120 * time.Second,
		JanitorInterval: 0,
		MaxEntries:      7,
		RedisPrefix:     "test:controller:channel-affinity",
	})
	manager.Clear()
	t.Cleanup(func() {
		manager.Clear()
	})
	manager.Set("affinity:key", 99, time.Minute)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/option/channel_affinity_cache", nil)

	GetChannelAffinityCache(ctx)

	var payload struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if !payload.Success {
		t.Fatalf("expected success payload, got %#v", payload)
	}
	if payload.Data["enabled"] != true {
		t.Fatalf("expected enabled=true, got %#v", payload.Data["enabled"])
	}
	if payload.Data["default_ttl_seconds"] != float64(120) {
		t.Fatalf("expected default_ttl_seconds=120, got %#v", payload.Data["default_ttl_seconds"])
	}
	if payload.Data["max_entries"] != float64(7) {
		t.Fatalf("expected max_entries=7, got %#v", payload.Data["max_entries"])
	}
	if payload.Data["backend"] != "memory" {
		t.Fatalf("expected memory backend, got %#v", payload.Data["backend"])
	}
	if payload.Data["local_entries"] != float64(1) {
		t.Fatalf("expected one local affinity entry, got %#v", payload.Data["local_entries"])
	}
	if payload.Data["rules_count"] != float64(1) {
		t.Fatalf("expected one configured rule, got %#v", payload.Data["rules_count"])
	}
}

func TestClearChannelAffinityCacheReturnsClearedCount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalSettings := config.ChannelAffinitySettingsInstance
	config.ChannelAffinitySettingsInstance = config.ChannelAffinitySettings{
		Enabled:           true,
		DefaultTTLSeconds: 60,
		MaxEntries:        5,
	}
	t.Cleanup(func() {
		config.ChannelAffinitySettingsInstance = originalSettings
	})

	manager := runtimeaffinity.ConfigureDefault(runtimeaffinity.ManagerOptions{
		DefaultTTL:      time.Minute,
		JanitorInterval: 0,
		MaxEntries:      5,
		RedisPrefix:     "test:controller:channel-affinity-clear",
	})
	manager.Clear()
	t.Cleanup(func() {
		manager.Clear()
	})
	manager.Set("affinity:a", 1, time.Minute)
	manager.Set("affinity:b", 2, time.Minute)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/option/channel_affinity_cache/clear", nil)

	ClearChannelAffinityCache(ctx)

	var payload struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if !payload.Success {
		t.Fatalf("expected success payload, got %#v", payload)
	}
	if payload.Data["cleared"] != float64(2) {
		t.Fatalf("expected cleared=2, got %#v", payload.Data["cleared"])
	}
}

func TestUpdateOptionRejectsInvalidPreferredChannelWaitMillisecondsValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	body := bytes.NewBufferString(`{"key":"PreferredChannelWaitMilliseconds","value":1.5}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/", body)

	UpdateOption(ctx)

	var payload struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Success {
		t.Fatalf("expected wait integer validation failure, got %#v", payload)
	}
	if payload.Message != "Codex 首选渠道等待时间必须是整数毫秒！" {
		t.Fatalf("expected friendly wait validation message, got %#v", payload.Message)
	}
	if payload.Data["failed_key"] != "PreferredChannelWaitMilliseconds" {
		t.Fatalf("expected failed_key PreferredChannelWaitMilliseconds, got %#v", payload.Data["failed_key"])
	}
	if _, err := model.GetOption("PreferredChannelWaitMilliseconds"); err == nil {
		t.Fatal("expected invalid wait value request to avoid database persistence")
	}
}

func TestUpdateOptionBatchPersistsCodexSettingsAtomically(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	originalPreferredWait := config.PreferredChannelWaitMilliseconds
	originalPreferredPollWait := config.PreferredChannelWaitPollMilliseconds
	originalRoutingHint := codex.RoutingHintSettingsInstance
	t.Cleanup(func() {
		config.PreferredChannelWaitMilliseconds = originalPreferredWait
		config.PreferredChannelWaitPollMilliseconds = originalPreferredPollWait
		codex.RoutingHintSettingsInstance = originalRoutingHint
	})

	body := bytes.NewBufferString(`{"updates":[{"key":"PreferredChannelWaitMilliseconds","value":25},{"key":"PreferredChannelWaitPollMilliseconds","value":5},{"key":"CodexRoutingHintSetting","value":"{\"prompt_cache_key_strategy\":\"auto\",\"model_regex\":\"^gpt-5$\"}"}]}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/batch", body)

	UpdateOptionBatch(ctx)

	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			UpdatedKeys []string `json:"updated_keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if !payload.Success {
		t.Fatalf("expected batch update to succeed, got %#v", payload)
	}
	if len(payload.Data.UpdatedKeys) != 3 {
		t.Fatalf("expected three updated keys, got %#v", payload.Data.UpdatedKeys)
	}
	if got := config.GlobalOption.Get("PreferredChannelWaitMilliseconds"); got != "25" {
		t.Fatalf("expected preferred wait to persist as 25, got %q", got)
	}
	if got := config.GlobalOption.Get("PreferredChannelWaitPollMilliseconds"); got != "5" {
		t.Fatalf("expected preferred poll wait to persist as 5, got %q", got)
	}
	if codex.RoutingHintSettingsInstance.ModelRegex != "^gpt-5$" {
		t.Fatalf("expected routing hint model regex to update, got %#v", codex.RoutingHintSettingsInstance)
	}

	storedWait, err := model.GetOption("PreferredChannelWaitMilliseconds")
	if err != nil {
		t.Fatalf("expected stored preferred wait option, got %v", err)
	}
	if storedWait.Value != "25" {
		t.Fatalf("expected stored preferred wait value 25, got %q", storedWait.Value)
	}
}

func TestUpdateOptionBatchRejectsInvalidCodexJSONWithoutPartialPersistence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	if err := model.UpdateOption("PreferredChannelWaitMilliseconds", "10"); err != nil {
		t.Fatalf("expected initial wait value to persist, got %v", err)
	}

	body := bytes.NewBufferString(`{"updates":[{"key":"PreferredChannelWaitMilliseconds","value":25},{"key":"CodexRoutingHintSetting","value":"{"}]}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/batch", body)

	UpdateOptionBatch(ctx)

	var payload struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Success {
		t.Fatalf("expected batch validation failure, got %#v", payload)
	}
	if payload.Data["failed_key"] != "CodexRoutingHintSetting" {
		t.Fatalf("expected failed_key CodexRoutingHintSetting, got %#v", payload.Data["failed_key"])
	}
	if got := config.GlobalOption.Get("PreferredChannelWaitMilliseconds"); got != "10" {
		t.Fatalf("expected failed batch to preserve in-memory wait value 10, got %q", got)
	}

	storedWait, err := model.GetOption("PreferredChannelWaitMilliseconds")
	if err != nil {
		t.Fatalf("expected stored preferred wait option, got %v", err)
	}
	if storedWait.Value != "10" {
		t.Fatalf("expected failed batch to preserve stored wait value 10, got %q", storedWait.Value)
	}
}

func TestUpdateOptionBatchRejectsDuplicateKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	body := bytes.NewBufferString(`{"updates":[{"key":"PreferredChannelWaitMilliseconds","value":25},{"key":"PreferredChannelWaitMilliseconds","value":30}]}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/batch", body)

	UpdateOptionBatch(ctx)

	var payload struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Success {
		t.Fatalf("expected duplicate key validation failure, got %#v", payload)
	}
	if payload.Data["failed_key"] != "PreferredChannelWaitMilliseconds" {
		t.Fatalf("expected duplicate key failure for PreferredChannelWaitMilliseconds, got %#v", payload.Data["failed_key"])
	}
	if _, err := model.GetOption("PreferredChannelWaitMilliseconds"); err == nil {
		t.Fatal("expected duplicate key batch to avoid database persistence")
	}
}

func TestUpdateOptionBatchRejectsUnknownKeysWithoutPartialPersistence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	if err := model.UpdateOption("PreferredChannelWaitMilliseconds", "10"); err != nil {
		t.Fatalf("expected initial wait value to persist, got %v", err)
	}

	body := bytes.NewBufferString(`{"updates":[{"key":"PreferredChannelWaitMilliseconds","value":25},{"key":"UnknownOption","value":"value"}]}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/batch", body)

	UpdateOptionBatch(ctx)

	var payload struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Success {
		t.Fatalf("expected unknown key validation failure, got %#v", payload)
	}
	if payload.Data["failed_key"] != "UnknownOption" {
		t.Fatalf("expected failed_key UnknownOption, got %#v", payload.Data["failed_key"])
	}
	if got := config.GlobalOption.Get("PreferredChannelWaitMilliseconds"); got != "10" {
		t.Fatalf("expected failed batch to preserve in-memory wait value 10, got %q", got)
	}

	storedWait, err := model.GetOption("PreferredChannelWaitMilliseconds")
	if err != nil {
		t.Fatalf("expected stored preferred wait option, got %v", err)
	}
	if storedWait.Value != "10" {
		t.Fatalf("expected failed batch to preserve stored wait value 10, got %q", storedWait.Value)
	}
	if _, err := model.GetOption("UnknownOption"); err == nil {
		t.Fatal("expected unknown option not to be persisted")
	}
}

func TestUpdateOptionRejectsNonCanonicalBoolString(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	original := config.GlobalOption.Get("PasswordLoginEnabled")

	body := bytes.NewBufferString(`{"key":"PasswordLoginEnabled","value":"TRUE"}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/", body)

	UpdateOption(ctx)

	var payload struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Success {
		t.Fatalf("expected invalid bool validation failure, got %#v", payload)
	}
	if payload.Data["failed_key"] != "PasswordLoginEnabled" {
		t.Fatalf("expected failed_key PasswordLoginEnabled, got %#v", payload.Data["failed_key"])
	}
	if got := config.GlobalOption.Get("PasswordLoginEnabled"); got != original {
		t.Fatalf("expected invalid bool request to preserve original value %q, got %q", original, got)
	}
	if _, err := model.GetOption("PasswordLoginEnabled"); err == nil {
		t.Fatal("expected invalid bool request to avoid database persistence")
	}
}

func TestUpdateOptionBatchRejectsInvalidCodexRegexWithoutPartialPersistence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	if err := model.UpdateOption("PreferredChannelWaitMilliseconds", "10"); err != nil {
		t.Fatalf("expected initial wait value to persist, got %v", err)
	}

	body := bytes.NewBufferString(`{"updates":[{"key":"PreferredChannelWaitMilliseconds","value":25},{"key":"CodexRoutingHintSetting","value":"{\"prompt_cache_key_strategy\":\"auto\",\"model_regex\":\"[\"}"}]}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/batch", body)

	UpdateOptionBatch(ctx)

	var payload struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Success {
		t.Fatalf("expected invalid regex validation failure, got %#v", payload)
	}
	if payload.Data["failed_key"] != "CodexRoutingHintSetting" {
		t.Fatalf("expected failed_key CodexRoutingHintSetting, got %#v", payload.Data["failed_key"])
	}
	if got := config.GlobalOption.Get("PreferredChannelWaitMilliseconds"); got != "10" {
		t.Fatalf("expected failed batch to preserve in-memory wait value 10, got %q", got)
	}

	storedWait, err := model.GetOption("PreferredChannelWaitMilliseconds")
	if err != nil {
		t.Fatalf("expected stored preferred wait option, got %v", err)
	}
	if storedWait.Value != "10" {
		t.Fatalf("expected failed batch to preserve stored wait value 10, got %q", storedWait.Value)
	}
}

func TestUpdateOptionAllowsUnrelatedUpdateWhenExistingGitHubConfigInvalid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	seedControllerTestOptions(t, map[string]string{
		"GitHubOAuthEnabled": "true",
	})

	body := bytes.NewBufferString(`{"key":"PreferredChannelWaitMilliseconds","value":15}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/", body)

	UpdateOption(ctx)

	var payload struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if !payload.Success {
		t.Fatalf("expected unrelated update to succeed despite invalid github config, got %#v", payload)
	}
	if got := config.GlobalOption.Get("PreferredChannelWaitMilliseconds"); got != "15" {
		t.Fatalf("expected unrelated update to persist in memory, got %q", got)
	}

	storedWait, err := model.GetOption("PreferredChannelWaitMilliseconds")
	if err != nil {
		t.Fatalf("expected stored preferred wait option, got %v", err)
	}
	if storedWait.Value != "15" {
		t.Fatalf("expected unrelated update to persist stored value 15, got %q", storedWait.Value)
	}
}

func TestUpdateOptionAllowsIncrementalGitHubRepairWhenExistingConfigInvalid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	seedControllerTestOptions(t, map[string]string{
		"GitHubOAuthEnabled": "true",
	})

	firstBody := bytes.NewBufferString(`{"key":"GitHubClientId","value":"cli_repair"}`)
	firstRecorder := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRecorder)
	firstCtx.Request = httptest.NewRequest(http.MethodPut, "/api/option/", firstBody)

	UpdateOption(firstCtx)

	var firstPayload struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(firstRecorder.Body.Bytes(), &firstPayload); err != nil {
		t.Fatalf("expected valid json payload for first repair step, got %v", err)
	}
	if !firstPayload.Success {
		t.Fatalf("expected first github repair step to succeed, got %#v", firstPayload)
	}
	if got := config.GlobalOption.Get("GitHubClientId"); got != "cli_repair" {
		t.Fatalf("expected github client id repair to persist, got %q", got)
	}

	secondBody := bytes.NewBufferString(`{"key":"GitHubClientSecret","value":"sec_repair"}`)
	secondRecorder := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRecorder)
	secondCtx.Request = httptest.NewRequest(http.MethodPut, "/api/option/", secondBody)

	UpdateOption(secondCtx)

	var secondPayload struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(secondRecorder.Body.Bytes(), &secondPayload); err != nil {
		t.Fatalf("expected valid json payload for second repair step, got %v", err)
	}
	if !secondPayload.Success {
		t.Fatalf("expected second github repair step to succeed, got %#v", secondPayload)
	}
	if got := config.GlobalOption.Get("GitHubClientSecret"); got != "sec_repair" {
		t.Fatalf("expected github client secret repair to persist, got %q", got)
	}

	storedSecret, err := model.GetOption("GitHubClientSecret")
	if err != nil {
		t.Fatalf("expected stored github client secret lookup to succeed, got %v", err)
	}
	if storedSecret.Value != "sec_repair" {
		t.Fatalf("expected stored github client secret value sec_repair, got %q", storedSecret.Value)
	}
}

func TestUpdateOptionRejectsNoProgressGitHubRepairWhenConfigStillInvalid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	seedControllerTestOptions(t, map[string]string{
		"GitHubOAuthEnabled": "true",
	})

	body := bytes.NewBufferString(`{"key":"GitHubClientId","value":""}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/", body)

	UpdateOption(ctx)

	var payload struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Success {
		t.Fatalf("expected no-progress github repair to fail, got %#v", payload)
	}
	if payload.Data["failed_key"] != "GitHubOAuthEnabled" {
		t.Fatalf("expected failed_key GitHubOAuthEnabled, got %#v", payload.Data["failed_key"])
	}
	if got := config.GlobalOption.Get("GitHubClientId"); got != "" {
		t.Fatalf("expected failed no-progress repair to preserve empty client id, got %q", got)
	}
	if _, err := model.GetOption("GitHubClientId"); err == nil {
		t.Fatal("expected failed no-progress repair to avoid persistence")
	}
}

func TestUpdateOptionRejectsWorseningGitHubRepairWhenConfigStillInvalid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	seedControllerTestOptions(t, map[string]string{
		"GitHubClientId":     "cli_seed",
		"GitHubOAuthEnabled": "true",
	})

	body := bytes.NewBufferString(`{"key":"GitHubClientId","value":""}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/", body)

	UpdateOption(ctx)

	var payload struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Success {
		t.Fatalf("expected worsening github repair to fail, got %#v", payload)
	}
	if payload.Data["failed_key"] != "GitHubOAuthEnabled" {
		t.Fatalf("expected failed_key GitHubOAuthEnabled, got %#v", payload.Data["failed_key"])
	}
	if got := config.GlobalOption.Get("GitHubClientId"); got != "cli_seed" {
		t.Fatalf("expected failed worsening repair to preserve client id, got %q", got)
	}
	storedClientID, err := model.GetOption("GitHubClientId")
	if err != nil {
		t.Fatalf("expected stored github client id lookup to succeed, got %v", err)
	}
	if storedClientID.Value != "cli_seed" {
		t.Fatalf("expected stored github client id value cli_seed, got %q", storedClientID.Value)
	}
}

func TestUpdateOptionRejectsClearingGitHubSecretWhileOAuthEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	if err := model.UpdateOption("GitHubClientId", "cli_seed"); err != nil {
		t.Fatalf("expected github client id seed to persist, got %v", err)
	}
	if err := model.UpdateOption("GitHubClientSecret", "sec_seed"); err != nil {
		t.Fatalf("expected github client secret seed to persist, got %v", err)
	}
	if err := model.UpdateOption("GitHubOAuthEnabled", "true"); err != nil {
		t.Fatalf("expected github oauth seed to persist, got %v", err)
	}

	body := bytes.NewBufferString(`{"key":"GitHubClientSecret","value":""}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/", body)

	UpdateOption(ctx)

	var payload struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Success {
		t.Fatalf("expected github secret clear while enabled to fail, got %#v", payload)
	}
	if payload.Data["failed_key"] != "GitHubOAuthEnabled" {
		t.Fatalf("expected failed_key GitHubOAuthEnabled, got %#v", payload.Data["failed_key"])
	}
	if got := config.GlobalOption.Get("GitHubClientSecret"); got != "sec_seed" {
		t.Fatalf("expected failed single update to preserve secret, got %q", got)
	}

	storedSecret, err := model.GetOption("GitHubClientSecret")
	if err != nil {
		t.Fatalf("expected stored github client secret lookup to succeed, got %v", err)
	}
	if storedSecret.Value != "sec_seed" {
		t.Fatalf("expected stored github client secret value sec_seed, got %q", storedSecret.Value)
	}
}

func TestUpdateOptionBatchEnablesLarkWhenCredentialsProvided(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	body := bytes.NewBufferString(`{"updates":[{"key":"LarkClientId","value":"cli_a"},{"key":"LarkClientSecret","value":"sec_b"},{"key":"LarkAuthEnabled","value":"true"}]}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/batch", body)

	UpdateOptionBatch(ctx)

	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			UpdatedKeys []string `json:"updated_keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if !payload.Success {
		t.Fatalf("expected lark batch enablement to succeed, got %#v", payload)
	}
	if len(payload.Data.UpdatedKeys) != 3 {
		t.Fatalf("expected three updated keys, got %#v", payload.Data.UpdatedKeys)
	}
	if got := config.GlobalOption.Get("LarkAuthEnabled"); got != "true" {
		t.Fatalf("expected lark auth to be enabled, got %q", got)
	}
	if got := config.GlobalOption.Get("LarkClientId"); got != "cli_a" {
		t.Fatalf("expected lark client id to persist, got %q", got)
	}
	if got := config.GlobalOption.Get("LarkClientSecret"); got != "sec_b" {
		t.Fatalf("expected lark client secret to persist, got %q", got)
	}

	storedSecret, err := model.GetOption("LarkClientSecret")
	if err != nil {
		t.Fatalf("expected stored lark client secret option, got %v", err)
	}
	if storedSecret.Value != "sec_b" {
		t.Fatalf("expected stored lark client secret value sec_b, got %q", storedSecret.Value)
	}
}

func TestUpdateOptionBatchRejectsEnablingLarkWithoutSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	body := bytes.NewBufferString(`{"updates":[{"key":"LarkClientId","value":"cli_a"},{"key":"LarkAuthEnabled","value":"true"}]}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/batch", body)

	UpdateOptionBatch(ctx)

	var payload struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Success {
		t.Fatalf("expected lark enablement without secret to fail, got %#v", payload)
	}
	if payload.Data["failed_key"] != "LarkAuthEnabled" {
		t.Fatalf("expected failed_key LarkAuthEnabled, got %#v", payload.Data["failed_key"])
	}
	if _, err := model.GetOption("LarkClientId"); err == nil {
		t.Fatal("expected failed lark batch to avoid partial persistence")
	}
	if got := config.GlobalOption.Get("LarkAuthEnabled"); got != "false" {
		t.Fatalf("expected failed lark batch to preserve disabled auth state, got %q", got)
	}
}
