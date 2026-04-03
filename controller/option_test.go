package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"one-api/common/config"
	runtimeaffinity "one-api/runtime/channelaffinity"

	"github.com/gin-gonic/gin"
)

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
