package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
	originalRecover := recoverAutoDisabledChannelsFunc
	originalNow := currentTimeFunc
	originalThreshold := config.ChannelDisableThreshold
	originalDisable := config.AutomaticDisableChannelEnabled
	originalEnable := config.AutomaticEnableChannelEnabled
	originalAutomaticRecoverEnabled := config.AutomaticRecoverChannelsEnabled
	originalAutomaticRecoverInterval := config.AutomaticRecoverChannelsIntervalMinutes
	originalRequestInterval := config.RequestInterval

	channelProbeStateLock.Lock()
	fullChannelProbeRunning = false
	autoRecoverProbeRunning = false
	channelProbeStateLock.Unlock()

	automaticRecoverStateLock.Lock()
	automaticRecoverLastRunAt = time.Time{}
	automaticRecoverStateLock.Unlock()

	t.Cleanup(func() {
		probeChannelFunc = originalProbe
		recoverAutoDisabledChannelsFunc = originalRecover
		currentTimeFunc = originalNow
		config.ChannelDisableThreshold = originalThreshold
		config.AutomaticDisableChannelEnabled = originalDisable
		config.AutomaticEnableChannelEnabled = originalEnable
		config.AutomaticRecoverChannelsEnabled = originalAutomaticRecoverEnabled
		config.AutomaticRecoverChannelsIntervalMinutes = originalAutomaticRecoverInterval
		config.RequestInterval = originalRequestInterval

		channelProbeStateLock.Lock()
		fullChannelProbeRunning = false
		autoRecoverProbeRunning = false
		channelProbeStateLock.Unlock()

		automaticRecoverStateLock.Lock()
		automaticRecoverLastRunAt = time.Time{}
		automaticRecoverStateLock.Unlock()
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

func TestRecoverAutoDisabledChannelsEnablesHealthyChannel(t *testing.T) {
	useControllerTestChannelDB(t)
	resetChannelProbeTestState(t)

	config.ChannelDisableThreshold = 5

	insertControllerTestChannel(t, &model.Channel{
		Id:        1,
		Name:      "recover-me",
		Status:    config.ChannelStatusAutoDisabled,
		TestModel: "gpt-5",
	})
	insertControllerTestChannel(t, &model.Channel{
		Id:        2,
		Name:      "manual",
		Status:    config.ChannelStatusManuallyDisabled,
		TestModel: "gpt-5",
	})

	probeChannelFunc = func(channel *model.Channel, testModel string) channelProbeResult {
		if channel.Id != 1 {
			t.Fatalf("expected only auto-disabled channels to be probed, got #%d", channel.Id)
		}
		if testModel != "" {
			t.Fatalf("expected recovery probe to use persisted test model, got %q", testModel)
		}
		return channelProbeResult{milliseconds: 1200}
	}

	if err := recoverAutoDisabledChannels(); err != nil {
		t.Fatalf("expected auto-disabled recovery to succeed, got %v", err)
	}

	recovered, err := model.GetChannelById(1)
	if err != nil {
		t.Fatalf("expected recovered channel lookup to succeed, got %v", err)
	}
	if recovered.Status != config.ChannelStatusEnabled {
		t.Fatalf("expected recovered channel to be enabled, got %d", recovered.Status)
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

func TestRecoverAutoDisabledChannelsSkipsMissingTestModel(t *testing.T) {
	useControllerTestChannelDB(t)
	resetChannelProbeTestState(t)

	insertControllerTestChannel(t, &model.Channel{
		Id:     3,
		Name:   "missing-test-model",
		Status: config.ChannelStatusAutoDisabled,
	})

	probeChannelFunc = func(channel *model.Channel, testModel string) channelProbeResult {
		t.Fatalf("expected missing test_model channel to be skipped before probe, got %#v", channel)
		return channelProbeResult{}
	}

	if err := recoverAutoDisabledChannels(); err != nil {
		t.Fatalf("expected missing test model to be skipped without failing recovery, got %v", err)
	}

	channel, err := model.GetChannelById(3)
	if err != nil {
		t.Fatalf("expected skipped channel lookup to succeed, got %v", err)
	}
	if channel.Status != config.ChannelStatusAutoDisabled {
		t.Fatalf("expected skipped channel to remain auto-disabled, got %d", channel.Status)
	}
	if channel.TestTime != 0 || channel.ResponseTime != 0 {
		t.Fatalf("expected skipped channel timing data to remain untouched, got test_time=%d response_time=%d", channel.TestTime, channel.ResponseTime)
	}
}

func TestRecoverAutoDisabledChannelsKeepsSlowChannelDisabled(t *testing.T) {
	useControllerTestChannelDB(t)
	resetChannelProbeTestState(t)

	config.ChannelDisableThreshold = 1

	insertControllerTestChannel(t, &model.Channel{
		Id:        4,
		Name:      "too-slow",
		Status:    config.ChannelStatusAutoDisabled,
		TestModel: "gpt-5",
	})

	probeChannelFunc = func(channel *model.Channel, testModel string) channelProbeResult {
		return channelProbeResult{milliseconds: 1500}
	}

	if err := recoverAutoDisabledChannels(); err != nil {
		t.Fatalf("expected slow channel recovery pass to complete, got %v", err)
	}

	channel, err := model.GetChannelById(4)
	if err != nil {
		t.Fatalf("expected slow channel lookup to succeed, got %v", err)
	}
	if channel.Status != config.ChannelStatusAutoDisabled {
		t.Fatalf("expected slow channel to remain auto-disabled, got %d", channel.Status)
	}
	if channel.TestTime != 0 || channel.ResponseTime != 0 {
		t.Fatalf("expected slow channel timing data to remain untouched, got test_time=%d response_time=%d", channel.TestTime, channel.ResponseTime)
	}
}

func TestRecoverAutoDisabledChannelsDoesNotOverrideManualDisableDuringProbe(t *testing.T) {
	useControllerTestChannelDB(t)
	resetChannelProbeTestState(t)

	config.RequestInterval = 0

	insertControllerTestChannel(t, &model.Channel{
		Id:        5,
		Name:      "manual-wins",
		Status:    config.ChannelStatusAutoDisabled,
		TestModel: "gpt-5",
	})

	probeChannelFunc = func(channel *model.Channel, testModel string) channelProbeResult {
		model.UpdateChannelStatusById(channel.Id, config.ChannelStatusManuallyDisabled)
		return channelProbeResult{milliseconds: 800}
	}

	if err := recoverAutoDisabledChannels(); err != nil {
		t.Fatalf("expected recovery pass to finish cleanly, got %v", err)
	}

	channel, err := model.GetChannelById(5)
	if err != nil {
		t.Fatalf("expected channel lookup to succeed, got %v", err)
	}
	if channel.Status != config.ChannelStatusManuallyDisabled {
		t.Fatalf("expected manual disable to win over recovery, got %d", channel.Status)
	}
	if channel.TestTime != 0 || channel.ResponseTime != 0 {
		t.Fatalf("expected skipped recovery not to update timing data, got test_time=%d response_time=%d", channel.TestTime, channel.ResponseTime)
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

func TestRecoverAutoDisabledChannelsDoesNotBlockFullChannelTests(t *testing.T) {
	useControllerTestChannelDB(t)
	resetChannelProbeTestState(t)

	config.RequestInterval = 0

	insertControllerTestChannel(t, &model.Channel{
		Id:        7,
		Name:      "recovering",
		Status:    config.ChannelStatusAutoDisabled,
		TestModel: "gpt-5",
	})
	insertControllerTestChannel(t, &model.Channel{
		Id:        8,
		Name:      "enabled",
		Status:    config.ChannelStatusEnabled,
		TestModel: "gpt-5",
	})

	recoveryProbeStarted := make(chan struct{})
	allowBlockedProbe := make(chan struct{})
	var autoDisabledProbeCount int32

	probeChannelFunc = func(channel *model.Channel, testModel string) channelProbeResult {
		if channel.Id == 7 && atomic.AddInt32(&autoDisabledProbeCount, 1) == 1 {
			close(recoveryProbeStarted)
			<-allowBlockedProbe
		}
		return channelProbeResult{milliseconds: 200}
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- recoverAutoDisabledChannels()
	}()

	select {
	case <-recoveryProbeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("expected auto-recover probe to start")
	}

	if err := testAllChannels(false); err != nil {
		t.Fatalf("expected full channel test to start while recovery probe is running, got %v", err)
	}
	if !isFullChannelProbeRunning() {
		t.Fatal("expected full channel probe to be marked as running")
	}

	close(allowBlockedProbe)

	select {
	case err := <-errCh:
		if err != nil && err != autoRecoverInterruptedByFullProbeErr {
			t.Fatalf("expected recovery to finish or yield to full probe, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected recovery probe to finish")
	}

	waitForFullChannelProbeCompletion(t)
}

func TestAutomaticRecoverChannelsTickHonorsEnablementAndInterval(t *testing.T) {
	resetChannelProbeTestState(t)

	runCount := 0
	recoverAutoDisabledChannelsFunc = func() error {
		runCount++
		return nil
	}

	now := time.Date(2026, 4, 14, 10, 0, 0, 0, time.UTC)
	currentTimeFunc = func() time.Time {
		return now
	}

	config.AutomaticEnableChannelEnabled = true
	config.AutomaticRecoverChannelsEnabled = false
	config.AutomaticRecoverChannelsIntervalMinutes = 10
	AutomaticRecoverChannelsTick()
	if runCount != 0 {
		t.Fatalf("expected passive auto-enable setting alone to skip background recovery, got %d runs", runCount)
	}

	config.AutomaticEnableChannelEnabled = false
	config.AutomaticRecoverChannelsEnabled = false
	config.AutomaticRecoverChannelsIntervalMinutes = 10
	AutomaticRecoverChannelsTick()
	if runCount != 0 {
		t.Fatalf("expected disabled recovery tick to skip execution, got %d runs", runCount)
	}

	config.AutomaticRecoverChannelsEnabled = true
	AutomaticRecoverChannelsTick()
	if runCount != 1 {
		t.Fatalf("expected first enabled tick to run recovery once, got %d", runCount)
	}

	AutomaticRecoverChannelsTick()
	if runCount != 1 {
		t.Fatalf("expected interval guard to skip immediate rerun, got %d", runCount)
	}

	now = now.Add(11 * time.Minute)
	AutomaticRecoverChannelsTick()
	if runCount != 2 {
		t.Fatalf("expected recovery to rerun after interval, got %d", runCount)
	}
}

func TestAutomaticRecoverChannelsTickSkipsWhileFullProbeRunsWithoutAdvancingSchedule(t *testing.T) {
	resetChannelProbeTestState(t)

	runCount := 0
	recoverAutoDisabledChannelsFunc = func() error {
		runCount++
		return nil
	}

	now := time.Date(2026, 4, 14, 10, 0, 0, 0, time.UTC)
	currentTimeFunc = func() time.Time {
		return now
	}

	config.AutomaticRecoverChannelsEnabled = true
	config.AutomaticRecoverChannelsIntervalMinutes = 10

	channelProbeStateLock.Lock()
	fullChannelProbeRunning = true
	channelProbeStateLock.Unlock()

	AutomaticRecoverChannelsTick()
	if runCount != 0 {
		t.Fatalf("expected full channel probe to suppress recovery tick, got %d runs", runCount)
	}

	channelProbeStateLock.Lock()
	fullChannelProbeRunning = false
	channelProbeStateLock.Unlock()

	AutomaticRecoverChannelsTick()
	if runCount != 1 {
		t.Fatalf("expected recovery tick to run immediately after full probe finishes, got %d runs", runCount)
	}
}

func TestAutomaticRecoverChannelsTickUsesCompletionTimeForSchedule(t *testing.T) {
	resetChannelProbeTestState(t)

	runCount := 0
	now := time.Date(2026, 4, 14, 10, 0, 0, 0, time.UTC)
	currentTimeFunc = func() time.Time {
		return now
	}
	recoverAutoDisabledChannelsFunc = func() error {
		runCount++
		now = now.Add(11 * time.Minute)
		return nil
	}

	config.AutomaticRecoverChannelsEnabled = true
	config.AutomaticRecoverChannelsIntervalMinutes = 10

	AutomaticRecoverChannelsTick()
	if runCount != 1 {
		t.Fatalf("expected first recovery tick to run once, got %d", runCount)
	}

	AutomaticRecoverChannelsTick()
	if runCount != 1 {
		t.Fatalf("expected second tick at recovery completion time to skip rerun, got %d", runCount)
	}
}
