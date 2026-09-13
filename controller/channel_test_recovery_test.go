package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useControllerTestChannelDB(t *testing.T) {
	t.Helper()

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	sqlDB, err := testDB.DB()
	if err != nil {
		t.Fatalf("expected sqlite connection pool, got %v", err)
	}
	// Shared-cache in-memory SQLite returns SQLITE_LOCKED when concurrent pooled
	// connections write the same table. A single connection keeps this fixture
	// deterministic while the controller workers themselves remain concurrent.
	sqlDB.SetMaxOpenConns(1)
	if err := testDB.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("expected channel schema migration for test database, got %v", err)
	}

	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
		_ = sqlDB.Close()
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
	originalGetProvider := getProviderFunc
	originalSendNotification := sendFullChannelProbeNotificationFunc
	originalThreshold := config.ChannelDisableThreshold
	originalDisable := config.AutomaticDisableChannelEnabled
	originalEnable := config.AutomaticEnableChannelEnabled
	originalRequestInterval := config.RequestInterval
	originalChannelTestConcurrency := config.ChannelTestConcurrency

	channelProbeStateLock.Lock()
	fullChannelProbeRunning = false
	channelProbeStateLock.Unlock()

	t.Cleanup(func() {
		probeChannelFunc = originalProbe
		currentTimeFunc = originalNow
		getProviderFunc = originalGetProvider
		sendFullChannelProbeNotificationFunc = originalSendNotification
		config.ChannelDisableThreshold = originalThreshold
		config.AutomaticDisableChannelEnabled = originalDisable
		config.AutomaticEnableChannelEnabled = originalEnable
		config.RequestInterval = originalRequestInterval
		config.ChannelTestConcurrency = originalChannelTestConcurrency

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

func TestFullChannelProbeConcurrencyNormalization(t *testing.T) {
	resetChannelProbeTestState(t)

	config.ChannelTestConcurrency = 0
	if got := fullChannelProbeConcurrency(); got != config.DefaultChannelTestConcurrency {
		t.Fatalf("expected zero concurrency to use default %d, got %d", config.DefaultChannelTestConcurrency, got)
	}

	config.ChannelTestConcurrency = -4
	if got := fullChannelProbeConcurrency(); got != config.DefaultChannelTestConcurrency {
		t.Fatalf("expected negative concurrency to use default %d, got %d", config.DefaultChannelTestConcurrency, got)
	}

	config.ChannelTestConcurrency = config.MaxChannelTestConcurrency + 10
	if got := fullChannelProbeConcurrency(); got != config.MaxChannelTestConcurrency {
		t.Fatalf("expected concurrency to be capped at %d, got %d", config.MaxChannelTestConcurrency, got)
	}

	config.ChannelTestConcurrency = 3
	if got := fullChannelProbeConcurrency(); got != 3 {
		t.Fatalf("expected configured concurrency to be used, got %d", got)
	}
}

func TestFullChannelProbeUsesCurrentThresholdAfterProbe(t *testing.T) {
	resetChannelProbeTestState(t)
	useControllerTestChannelDB(t)

	originalManager := config.GlobalOption
	manager := config.NewOptionManager()
	thresholdSeconds := 1.0
	manager.RegisterFloatOption("ChannelDisableThreshold", &thresholdSeconds, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{"ChannelDisableThreshold": "1"}); err != nil {
		t.Fatalf("publish initial threshold: %v", err)
	}
	config.GlobalOption = manager
	t.Cleanup(func() { config.GlobalOption = originalManager })

	channel := &model.Channel{Id: 71, Name: "threshold-refresh", Status: config.ChannelStatusEnabled}
	insertControllerTestChannel(t, channel)
	probeChannelFunc = func(*model.Channel, string) channelProbeResult {
		if _, err := manager.PublishRuntimeOverrides(2, map[string]string{"ChannelDisableThreshold": "10"}); err != nil {
			t.Fatalf("publish updated threshold: %v", err)
		}
		return channelProbeResult{milliseconds: 5_000}
	}

	report := testAllChannel(channel)
	if strings.Contains(report, "禁用") {
		t.Fatalf("post-probe decision retained the stale one-second threshold: %s", report)
	}
	var persisted model.Channel
	if err := model.DB.First(&persisted, channel.Id).Error; err != nil {
		t.Fatalf("reload channel: %v", err)
	}
	if persisted.Status != config.ChannelStatusEnabled {
		t.Fatalf("new ten-second threshold should keep the channel enabled, status=%d", persisted.Status)
	}
}

func TestRunFullChannelProbeTaskHonorsConcurrencyLimit(t *testing.T) {
	resetChannelProbeTestState(t)

	config.ChannelTestConcurrency = 2
	config.RequestInterval = 0

	channels := []*model.Channel{
		{Id: 1, Name: "channel-1", Status: config.ChannelStatusEnabled},
		{Id: 2, Name: "channel-2", Status: config.ChannelStatusEnabled},
		{Id: 3, Name: "channel-3", Status: config.ChannelStatusEnabled},
		{Id: 4, Name: "channel-4", Status: config.ChannelStatusEnabled},
		{Id: 5, Name: "channel-5", Status: config.ChannelStatusEnabled},
	}

	started := make(chan struct{}, len(channels))
	release := make(chan struct{})
	var mu sync.Mutex
	active := 0
	maxActive := 0

	probeChannelFunc = func(channel *model.Channel, testModel string) channelProbeResult {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()

		started <- struct{}{}
		<-release

		mu.Lock()
		active--
		mu.Unlock()

		return channelProbeResult{err: fmt.Errorf("probe failed")}
	}

	done := make(chan struct{})
	go func() {
		runFullChannelProbeTask(channels)
		close(done)
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("expected probe %d to start", i+1)
		}
	}

	select {
	case <-started:
		t.Fatal("expected third probe not to start before a worker is released")
	case <-time.After(30 * time.Millisecond):
	}

	close(release)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expected full channel probe task to finish")
	}

	if maxActive != 2 {
		t.Fatalf("expected at most 2 concurrent probes, got %d", maxActive)
	}
}

func TestRunFullChannelProbeTaskKeepsReportOrder(t *testing.T) {
	resetChannelProbeTestState(t)

	config.ChannelTestConcurrency = 3
	config.RequestInterval = 0

	channels := []*model.Channel{
		{Id: 1, Name: "slowfirst", Status: config.ChannelStatusEnabled},
		{Id: 2, Name: "fastsecond", Status: config.ChannelStatusEnabled},
		{Id: 3, Name: "fastthird", Status: config.ChannelStatusEnabled},
	}

	probeChannelFunc = func(channel *model.Channel, testModel string) channelProbeResult {
		if channel.Id == 1 {
			time.Sleep(20 * time.Millisecond)
		}
		return channelProbeResult{err: fmt.Errorf("probe failed")}
	}

	report := runFullChannelProbeTask(channels)
	first := strings.Index(report, "slowfirst")
	second := strings.Index(report, "fastsecond")
	third := strings.Index(report, "fastthird")
	if first < 0 || second < 0 || third < 0 {
		t.Fatalf("expected all channel names in report, got %q", report)
	}
	if !(first < second && second < third) {
		t.Fatalf("expected report to keep channel order, got %q", report)
	}
}

func TestTestAllChannelsRejectsConcurrentStart(t *testing.T) {
	useControllerTestChannelDB(t)
	resetChannelProbeTestState(t)

	config.RequestInterval = 0

	insertControllerTestChannel(t, &model.Channel{
		Id:        10,
		Name:      "blocked",
		Status:    config.ChannelStatusEnabled,
		TestModel: "gpt-5",
	})

	started := make(chan struct{})
	release := make(chan struct{})
	probeChannelFunc = func(channel *model.Channel, testModel string) channelProbeResult {
		close(started)
		<-release
		return channelProbeResult{err: fmt.Errorf("probe failed")}
	}

	if err := testAllChannels(false); err != nil {
		t.Fatalf("expected first full channel test to start, got %v", err)
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("expected first full channel test to enter probe")
	}

	if err := testAllChannels(false); err != fullChannelProbeRunningErr {
		t.Fatalf("expected concurrent start to be rejected with running error, got %v", err)
	}

	close(release)
	waitForFullChannelProbeCompletion(t)
}

func TestTestAllChannelsSkipsManuallyDisabledChannels(t *testing.T) {
	for _, tc := range []struct {
		name       string
		isNotify   bool
		onlyManual bool
		noChannels bool
	}{
		{name: "定时测速"},
		{name: "手动测试全部", isNotify: true},
		{name: "定时测速全部手动禁用", onlyManual: true},
		{name: "手动测试全部渠道均手动禁用", isNotify: true, onlyManual: true},
		{name: "定时测速无渠道", noChannels: true},
		{name: "手动测试全部无渠道", isNotify: true, noChannels: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useControllerTestChannelDB(t)
			resetChannelProbeTestState(t)
			config.AutomaticEnableChannelEnabled = true
			config.ChannelDisableThreshold = 5
			config.RequestInterval = 0

			var notificationTitles, notificationMessages []string
			sendFullChannelProbeNotificationFunc = func(title, message string) {
				notificationTitles = append(notificationTitles, title)
				notificationMessages = append(notificationMessages, message)
			}

			if !tc.noChannels {
				insertControllerTestChannel(t, &model.Channel{
					Id: 1, Name: "手动禁用", Status: config.ChannelStatusManuallyDisabled,
					TestModel: "gpt-5", TestTime: 123, ResponseTime: 456,
				})
			}
			if !tc.onlyManual && !tc.noChannels {
				insertControllerTestChannel(t, &model.Channel{
					Id: 2, Name: "已启用", Status: config.ChannelStatusEnabled, TestModel: "gpt-5",
				})
				insertControllerTestChannel(t, &model.Channel{
					Id: 3, Name: "自动禁用", Status: config.ChannelStatusAutoDisabled, TestModel: "gpt-5",
				})
			}

			probed := make(chan int, 3)
			probeChannelFunc = func(channel *model.Channel, _ string) channelProbeResult {
				probed <- channel.Id
				return channelProbeResult{milliseconds: 1200}
			}
			if err := testAllChannels(tc.isNotify); err != nil {
				t.Fatalf("启动批量测速失败：%v", err)
			}
			waitForFullChannelProbeCompletion(t)
			if tc.onlyManual || tc.noChannels {
				if err := testAllChannels(tc.isNotify); err != nil {
					t.Fatalf("无待测渠道时应释放运行标记，允许再次启动：%v", err)
				}
				if isFullChannelProbeRunning() {
					t.Fatal("无待测渠道时应同步释放运行标记")
				}
			}
			close(probed)

			counts := make(map[int]int)
			for id := range probed {
				counts[id]++
			}
			if counts[1] != 0 {
				t.Fatalf("手动禁用渠道不应测速，实际执行 %d 次", counts[1])
			}
			if !tc.onlyManual && !tc.noChannels && (counts[2] != 1 || counts[3] != 1) {
				t.Fatalf("已启用和自动禁用渠道应各测速一次，实际为 %v", counts)
			}
			if tc.isNotify && !tc.onlyManual && !tc.noChannels {
				if len(notificationMessages) != 1 || notificationTitles[0] != "通道测试完成" {
					t.Fatalf("手动批量测速应发送一次完成通知：titles=%v messages=%v", notificationTitles, notificationMessages)
				}
				report := notificationMessages[0]
				if strings.Contains(report, "手动禁用") || !strings.Contains(report, "已启用") || !strings.Contains(report, "自动禁用") {
					t.Fatalf("通知报告应只包含本轮待测渠道：%q", report)
				}
			} else if len(notificationMessages) != 0 {
				t.Fatalf("定时测速或无待测渠道时不应发送完成通知：%v", notificationMessages)
			}
			if tc.noChannels {
				if len(counts) != 0 {
					t.Fatalf("无渠道时不应执行测速：%v", counts)
				}
				return
			}

			manual, err := model.GetChannelById(1)
			if err != nil {
				t.Fatalf("读取手动禁用渠道失败：%v", err)
			}
			if manual.Status != config.ChannelStatusManuallyDisabled || manual.TestTime != 123 || manual.ResponseTime != 456 {
				t.Fatalf("手动禁用渠道的状态和测速记录不应变化：status=%d test_time=%d response_time=%d", manual.Status, manual.TestTime, manual.ResponseTime)
			}
		})
	}
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
