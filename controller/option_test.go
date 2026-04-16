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
	runtimeaffinity "one-api/runtime/channelaffinity"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useControllerTestOptionDB(t *testing.T) {
	t.Helper()

	originalDB := model.DB
	originalOptionManager := config.GlobalOption
	originalAutomaticRecoverEnabled := config.AutomaticRecoverChannelsEnabled
	originalAutomaticRecoverInterval := config.AutomaticRecoverChannelsIntervalMinutes

	testDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.Option{}); err != nil {
		t.Fatalf("expected option schema migration, got %v", err)
	}

	model.DB = testDB
	config.GlobalOption = config.NewOptionManager()
	config.AutomaticRecoverChannelsEnabled = false
	config.AutomaticRecoverChannelsIntervalMinutes = 10
	model.InitOptionMap()

	t.Cleanup(func() {
		model.DB = originalDB
		config.GlobalOption = originalOptionManager
		config.AutomaticRecoverChannelsEnabled = originalAutomaticRecoverEnabled
		config.AutomaticRecoverChannelsIntervalMinutes = originalAutomaticRecoverInterval
	})
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

func TestUpdateOptionRejectsEnablingAutomaticRecoverWithoutPositiveInterval(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	config.AutomaticRecoverChannelsIntervalMinutes = 0

	body := bytes.NewBufferString(`{"key":"AutomaticRecoverChannelsEnabled","value":"true"}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/", body)

	UpdateOption(ctx)

	var payload struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Success {
		t.Fatalf("expected enablement validation failure, got %#v", payload)
	}
	if payload.Message == "" {
		t.Fatalf("expected validation message, got %#v", payload)
	}
}

func TestUpdateOptionResetsAutomaticRecoverScheduleWhenEnablementChanges(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	originalRecover := recoverAutoDisabledChannelsFunc
	originalNow := currentTimeFunc
	t.Cleanup(func() {
		recoverAutoDisabledChannelsFunc = originalRecover
		currentTimeFunc = originalNow
		resetAutomaticRecoverSchedule()
	})

	now := time.Date(2026, 4, 16, 9, 0, 0, 0, time.UTC)
	currentTimeFunc = func() time.Time {
		return now
	}
	recoverRuns := 0
	recoverAutoDisabledChannelsFunc = func() error {
		recoverRuns++
		return nil
	}

	automaticRecoverStateLock.Lock()
	automaticRecoverLastRunAt = now
	automaticRecoverStateLock.Unlock()
	config.AutomaticRecoverChannelsEnabled = false
	config.AutomaticRecoverChannelsIntervalMinutes = 10

	body := bytes.NewBufferString(`{"key":"AutomaticRecoverChannelsEnabled","value":"true"}`)
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
		t.Fatalf("expected enablement update to succeed, got %#v", payload)
	}

	AutomaticRecoverChannelsTick()
	if recoverRuns != 1 {
		t.Fatalf("expected re-enabled recovery schedule to run immediately, got %d runs", recoverRuns)
	}
}

func TestUpdateOptionMapsLegacyAutomaticRecoverIntervalKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	body := bytes.NewBufferString(`{"key":"AutomaticEnableChannelRecoverFrequency","value":"21"}`)
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
		t.Fatalf("expected success payload, got %#v", payload)
	}
	if got := config.GlobalOption.Get("AutomaticRecoverChannelsIntervalMinutes"); got != "21" {
		t.Fatalf("expected legacy key write to update new interval option, got %q", got)
	}

	storedOption, err := model.GetOption("AutomaticRecoverChannelsIntervalMinutes")
	if err != nil {
		t.Fatalf("expected stored new interval option lookup to succeed, got %v", err)
	}
	if storedOption.Value != "21" {
		t.Fatalf("expected stored new interval option value 21, got %q", storedOption.Value)
	}
}

func TestUpdateOptionAcceptsNumericAutomaticRecoverIntervalValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerTestOptionDB(t)

	body := bytes.NewBufferString(`{"key":"AutomaticRecoverChannelsIntervalMinutes","value":10}`)
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
		t.Fatalf("expected numeric value payload to succeed, got %#v", payload)
	}
	if got := config.GlobalOption.Get("AutomaticRecoverChannelsIntervalMinutes"); got != "10" {
		t.Fatalf("expected numeric value to persist as string 10, got %q", got)
	}
}
