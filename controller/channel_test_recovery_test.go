package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useControllerTestChannelDB(t *testing.T) {
	t.Helper()

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("expected channel schema migration for test database, got %v", err)
	}

	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
	})
}

func insertControllerTestChannel(t *testing.T, channel *model.Channel) {
	t.Helper()
	if err := model.DB.Create(channel).Error; err != nil {
		t.Fatalf("expected channel fixture to persist, got %v", err)
	}
}

func resetChannelProbeTestState(t *testing.T) {
	t.Helper()

	logger.SetupLogger()

	originalProbe := probeChannelFunc
	originalNow := currentTimeFunc
	originalThreshold := config.ChannelDisableThreshold
	originalDisable := config.AutomaticDisableChannelEnabled
	originalEnable := config.AutomaticEnableChannelEnabled
	originalRequestInterval := config.RequestInterval

	channelProbeStateLock.Lock()
	fullChannelProbeRunning = false
	channelProbeStateLock.Unlock()

	t.Cleanup(func() {
		probeChannelFunc = originalProbe
		currentTimeFunc = originalNow
		config.ChannelDisableThreshold = originalThreshold
		config.AutomaticDisableChannelEnabled = originalDisable
		config.AutomaticEnableChannelEnabled = originalEnable
		config.RequestInterval = originalRequestInterval

		channelProbeStateLock.Lock()
		fullChannelProbeRunning = false
		channelProbeStateLock.Unlock()
	})
}

func waitForFullChannelProbeCompletion(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !isFullChannelProbeRunning() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("expected full channel probe task to finish")
}

func TestTestAllChannelsAutoRecoversHealthyAutoDisabledChannel(t *testing.T) {
	useControllerTestChannelDB(t)
	resetChannelProbeTestState(t)

	config.AutomaticEnableChannelEnabled = true
	config.ChannelDisableThreshold = 5
	config.RequestInterval = 0

	insertControllerTestChannel(t, &model.Channel{
		Id:        1,
		Name:      "recover-me",
		Status:    config.ChannelStatusAutoDisabled,
		TestModel: "gpt-5",
	})
	insertControllerTestChannel(t, &model.Channel{
		Id:        2,
		Name:      "manual-stays-disabled",
		Status:    config.ChannelStatusManuallyDisabled,
		TestModel: "gpt-5",
	})

	probeChannelFunc = func(channel *model.Channel, testModel string) channelProbeResult {
		return channelProbeResult{milliseconds: 1200}
	}

	if err := testAllChannels(false); err != nil {
		t.Fatalf("expected full channel test to start, got %v", err)
	}
	waitForFullChannelProbeCompletion(t)

	recovered, err := model.GetChannelById(1)
	if err != nil {
		t.Fatalf("expected recovered channel lookup to succeed, got %v", err)
	}
	if recovered.Status != config.ChannelStatusEnabled {
		t.Fatalf("expected auto-disabled channel to be enabled, got %d", recovered.Status)
	}
	if recovered.ResponseTime != 1200 {
		t.Fatalf("expected recovered response time to be stored, got %d", recovered.ResponseTime)
	}
	if recovered.TestTime == 0 {
		t.Fatal("expected recovered channel test time to be updated")
	}

	manual, err := model.GetChannelById(2)
	if err != nil {
		t.Fatalf("expected manual channel lookup to succeed, got %v", err)
	}
	if manual.Status != config.ChannelStatusManuallyDisabled {
		t.Fatalf("expected manual channel status to remain unchanged, got %d", manual.Status)
	}
}

func TestTestAllChannelsAutoRecoverDoesNotOverrideManualDisableDuringProbe(t *testing.T) {
	useControllerTestChannelDB(t)
	resetChannelProbeTestState(t)

	config.AutomaticEnableChannelEnabled = true
	config.RequestInterval = 0

	insertControllerTestChannel(t, &model.Channel{
		Id:        6,
		Name:      "still-manual",
		Status:    config.ChannelStatusAutoDisabled,
		TestModel: "gpt-5",
	})

	probeChannelFunc = func(channel *model.Channel, testModel string) channelProbeResult {
		model.UpdateChannelStatusById(channel.Id, config.ChannelStatusManuallyDisabled)
		return channelProbeResult{milliseconds: 600}
	}

	if err := testAllChannels(false); err != nil {
		t.Fatalf("expected full channel test to start, got %v", err)
	}
	waitForFullChannelProbeCompletion(t)

	channel, err := model.GetChannelById(6)
	if err != nil {
		t.Fatalf("expected channel lookup to succeed, got %v", err)
	}
	if channel.Status != config.ChannelStatusManuallyDisabled {
		t.Fatalf("expected manual disable to win during full-channel recovery path, got %d", channel.Status)
	}
	if channel.TestTime != 0 || channel.ResponseTime != 0 {
		t.Fatalf("expected skipped auto-enable not to update timing data, got test_time=%d response_time=%d", channel.TestTime, channel.ResponseTime)
	}
}

func TestTestChannelDoesNotOverrideManualDisable(t *testing.T) {
	useControllerTestChannelDB(t)
	resetChannelProbeTestState(t)
	gin.SetMode(gin.TestMode)

	config.AutomaticDisableChannelEnabled = true

	insertControllerTestChannel(t, &model.Channel{
		Id:        9,
		Name:      "manual-disable",
		Type:      config.ChannelTypeOpenAI,
		Status:    config.ChannelStatusManuallyDisabled,
		TestModel: "gpt-5",
	})

	probeChannelFunc = func(channel *model.Channel, testModel string) channelProbeResult {
		return channelProbeResult{
			openaiErr: &types.OpenAIErrorWithStatusCode{
				StatusCode: http.StatusUnauthorized,
				OpenAIError: types.OpenAIError{
					Message: "invalid key",
					Type:    "authentication_error",
					Code:    "invalid_api_key",
				},
			},
			err: fmt.Errorf("invalid key"),
		}
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "9"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/test/9", nil)

	TestChannel(ctx)

	channel, err := model.GetChannelById(9)
	if err != nil {
		t.Fatalf("expected channel lookup to succeed, got %v", err)
	}
	if channel.Status != config.ChannelStatusManuallyDisabled {
		t.Fatalf("expected manual disable to survive single-channel test, got %d", channel.Status)
	}

	var payload struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected valid json payload, got %v", err)
	}
	if payload.Success {
		t.Fatalf("expected failed probe response, got %#v", payload)
	}
	if payload.Message == "" || payload.Message == "测速失败，已被禁用，原因：invalid key" {
		t.Fatalf("expected generic failure message without auto-disable claim, got %#v", payload)
	}
}
