package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/logger"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestPath2RelayAndLimitModelHelpers(t *testing.T) {
	ginCtx := newRelayTestContext(nil)

	paths := []string{
		"/v1/chat/completions",
		"/v1/completions",
		"/v1/embeddings",
		"/v1/moderations",
		"/v1/images/generations",
		"/recraftAI/v1/images/generations",
		"/v1/images/edits",
		"/v1/images/variations",
		"/v1/audio/speech",
		"/v1/audio/transcriptions",
		"/v1/audio/translations",
		"/claude",
		"/gemini",
		"/v1/responses",
		"/recraftAI/v1/styles",
	}
	for _, path := range paths {
		if relay := Path2Relay(ginCtx, path); relay == nil {
			t.Fatalf("expected path %q to resolve to a relay handler", path)
		}
	}
	if relay := Path2Relay(ginCtx, "/unknown"); relay != nil {
		t.Fatalf("expected unmatched path to return nil relay, got %#v", relay)
	}

	if err := checkLimitModel(ginCtx, "gpt-5"); err != nil {
		t.Fatalf("expected missing token_setting not to restrict models, got %v", err)
	}
	ginCtx.Set("token_setting", "wrong-type")
	if err := checkLimitModel(ginCtx, "gpt-5"); err != nil {
		t.Fatalf("expected wrong typed token_setting not to restrict models, got %v", err)
	}

	setting := &model.TokenSetting{}
	ginCtx.Set("token_setting", setting)
	if err := checkLimitModel(ginCtx, "gpt-5"); err != nil {
		t.Fatalf("expected disabled model limits not to restrict models, got %v", err)
	}

	setting.Limits.LimitModelSetting.Enabled = true
	if err := checkLimitModel(ginCtx, "gpt-5"); err == nil || !strings.Contains(err.Error(), "No available models") {
		t.Fatalf("expected empty model allow-list to reject usage, got %v", err)
	}

	setting.Limits.LimitModelSetting.Models = []string{"gpt-5"}
	if err := checkLimitModel(ginCtx, "gpt-5"); err != nil {
		t.Fatalf("expected allow-listed model to pass, got %v", err)
	}
	if err := checkLimitModel(ginCtx, "gpt-4o"); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected mismatched model to fail, got %v", err)
	}
}

func TestProviderSelectionCachingAndChannelSelectionHelpers(t *testing.T) {
	ctx := newRelayTestContext(nil)
	provider := &relayTestBaseProvider{channel: newRelayTestCodexChannel(11)}

	ctx.Set("channel_id", 11)
	ctx.Set("channel_type", config.ChannelTypeCodex)
	ctx.Set("billing_original_model", true)
	ctx.Set("skip_only_chat", true)
	ctx.Set("is_stream", false)
	cacheProviderSelection(ctx, "gpt-5", provider, "gpt-5-codex")

	cachedProvider, newModelName, ok := consumeCachedProviderSelection(ctx, "gpt-5")
	if !ok || cachedProvider != provider || newModelName != "gpt-5-codex" {
		t.Fatalf("expected cached provider selection round-trip, got provider=%#v model=%q ok=%v", cachedProvider, newModelName, ok)
	}
	if ctx.GetInt("channel_id") != 11 || ctx.GetString("new_model") != "gpt-5-codex" || !ctx.GetBool("billing_original_model") {
		t.Fatalf("expected provider selection context to be restored, got channel_id=%d new_model=%q billing_original=%v", ctx.GetInt("channel_id"), ctx.GetString("new_model"), ctx.GetBool("billing_original_model"))
	}

	cacheProviderSelection(ctx, "gpt-5", provider, "gpt-5-codex")
	ctx.Set("skip_only_chat", false)
	if _, _, ok := consumeCachedProviderSelection(ctx, "gpt-5"); ok {
		t.Fatal("expected cache miss when skip_only_chat changes")
	}
	ctx.Set("skip_only_chat", true)

	cacheProviderSelection(ctx, "gpt-5", provider, "gpt-5-codex")
	if _, _, ok := consumeCachedProviderSelection(ctx, "gpt-4o"); ok {
		t.Fatal("expected cache miss when original model changes")
	}

	if explicitChannelPinID(nil) != 0 {
		t.Fatal("expected nil context to have no explicit channel pin")
	}
	ctx.Set("specific_channel_id", 99)
	if explicitChannelPinID(ctx) != 99 {
		t.Fatalf("expected explicit channel pin id, got %d", explicitChannelPinID(ctx))
	}
	ctx.Set("specific_channel_id_ignore", true)
	if explicitChannelPinID(ctx) != 0 {
		t.Fatalf("expected ignored specific channel id to return zero, got %d", explicitChannelPinID(ctx))
	}
	ctx.Set("specific_channel_id_ignore", false)

	setPreferredChannelFromAffinity(ctx, 88)
	ctx.Set(channelAffinityIgnoreCooldownContextKey, true)
	ctx.Set(channelAffinityStrictContextKey, true)
	ctx.Set("skip_channel_ids", []int{5, 6})
	ctx.Set("allow_channel_type", []int{config.ChannelTypeCodex})
	selection := currentRealtimeChannelSelection(ctx)
	if selection.preferredChannelID != 88 || !selection.ignorePreferredCooldown || !selection.strictPreferredChannel {
		t.Fatalf("expected affinity selection flags to be captured, got %+v", selection)
	}
	if len(selection.skipChannelIDs) != 2 || len(selection.allowChannelTypes) != 1 {
		t.Fatalf("expected selection filters to be copied from gin context, got %+v", selection)
	}

	originalWaitBudget := config.PreferredChannelWaitMilliseconds
	originalWaitPoll := config.PreferredChannelWaitPollMilliseconds
	config.PreferredChannelWaitMilliseconds = 0
	config.PreferredChannelWaitPollMilliseconds = 0
	if preferredChannelWaitBudget() != 0 || preferredChannelWaitPollInterval() != 50*time.Millisecond {
		t.Fatalf("expected wait helpers to normalize zero config values, got budget=%v poll=%v", preferredChannelWaitBudget(), preferredChannelWaitPollInterval())
	}
	config.PreferredChannelWaitMilliseconds = 125
	config.PreferredChannelWaitPollMilliseconds = 10
	if preferredChannelWaitBudget() != 125*time.Millisecond || preferredChannelWaitPollInterval() != 10*time.Millisecond {
		t.Fatalf("unexpected wait helper values, got budget=%v poll=%v", preferredChannelWaitBudget(), preferredChannelWaitPollInterval())
	}
	config.PreferredChannelWaitMilliseconds = originalWaitBudget
	config.PreferredChannelWaitPollMilliseconds = originalWaitPoll

	if requestContextErr(nil) != nil {
		t.Fatal("expected nil request context error to be nil")
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	req := ctx.Request.WithContext(cancelCtx)
	ctx.Request = req
	if !errors.Is(requestContextErr(ctx), context.Canceled) {
		t.Fatalf("expected canceled request context to surface, got %v", requestContextErr(ctx))
	}

	recordPreferredChannelWaitMeta(ctx, 250*time.Millisecond, 100*time.Millisecond, true, false)
	meta := currentChannelAffinityLogMeta(ctx)
	if meta["channel_affinity_wait_budget_ms"] != int64(250) || meta["channel_affinity_wait_exhausted"] != true {
		t.Fatalf("expected wait metadata to be recorded, got %#v", meta)
	}
}

func TestGroupManagerFallbackAndFetchChannelByID(t *testing.T) {
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	originalUserGroups := model.GlobalUserGroupRatio.UserGroup
	originalAPILimiter := model.GlobalUserGroupRatio.APILimiter
	originalPublicGroups := append([]string(nil), model.GlobalUserGroupRatio.PublicGroup...)
	model.GlobalUserGroupRatio.UserGroup = map[string]*model.UserGroup{
		"backup": {Symbol: "backup", Ratio: 1.75},
	}
	model.GlobalUserGroupRatio.APILimiter = nil
	model.GlobalUserGroupRatio.PublicGroup = nil
	t.Cleanup(func() {
		model.GlobalUserGroupRatio.UserGroup = originalUserGroups
		model.GlobalUserGroupRatio.APILimiter = originalAPILimiter
		model.GlobalUserGroupRatio.PublicGroup = originalPublicGroups
	})

	ctx := newRelayTestContext(nil)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	ctx.Set("token_group", "primary")
	ctx.Set("token_backup_group", "backup")

	groupManager := NewGroupManager(ctx)
	var attempted []string
	channel, err := groupManager.TryWithGroups("gpt-5", nil, func(group string) (*model.Channel, error) {
		attempted = append(attempted, group)
		if group == "primary" {
			return nil, errors.New("primary unavailable")
		}
		return &model.Channel{Id: 7}, nil
	})
	if err != nil || channel == nil || channel.Id != 7 {
		t.Fatalf("expected fallback group to succeed, got channel=%#v err=%v", channel, err)
	}
	if len(attempted) != 2 || attempted[0] != "primary" || attempted[1] != "backup" {
		t.Fatalf("expected primary then backup group attempts, got %#v", attempted)
	}
	if !ctx.GetBool("is_backupGroup") || ctx.GetFloat64("group_ratio") != 1.75 {
		t.Fatalf("expected fallback group metadata to be written to context, got is_backupGroup=%v group_ratio=%v", ctx.GetBool("is_backupGroup"), ctx.GetFloat64("group_ratio"))
	}
	if got := groupctx.CurrentRoutingGroup(ctx); got != "backup" {
		t.Fatalf("expected fallback to update routing group, got %q", got)
	}
	if got := groupctx.CurrentRoutingGroupSource(ctx); got != groupctx.RoutingGroupSourceBackupGroup {
		t.Fatalf("expected fallback to update routing group source, got %q", got)
	}

	if _, err := groupManager.tryGroup("", "gpt-5", nil, nil); err == nil {
		t.Fatal("expected empty group lookup to fail")
	}
	if err := groupManager.setGroupRatio("missing"); err == nil {
		t.Fatal("expected missing group ratio lookup to fail")
	}
	if err := groupManager.createGroupError("backup", "gpt-5", &model.Channel{Id: 9}); err == nil || !strings.Contains(err.Error(), "数据库一致性") {
		t.Fatalf("expected broken channel reference error, got %v", err)
	}

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("expected channel schema migration, got %v", err)
	}
	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
	})

	enabled := newRelayTestCodexChannel(101)
	disabled := newRelayTestCodexChannel(102)
	disabled.Status = config.ChannelStatusManuallyDisabled
	if err := testDB.Create(enabled).Error; err != nil {
		t.Fatalf("expected enabled channel insert, got %v", err)
	}
	if err := testDB.Create(disabled).Error; err != nil {
		t.Fatalf("expected disabled channel insert, got %v", err)
	}

	if channel, err := fetchChannelById(enabled.Id); err != nil || channel == nil || channel.Id != enabled.Id {
		t.Fatalf("expected enabled channel lookup to succeed, got channel=%#v err=%v", channel, err)
	}
	if _, err := fetchChannelById(9999); err == nil || !strings.Contains(err.Error(), "无效的渠道 Id") {
		t.Fatalf("expected missing channel lookup to fail, got %v", err)
	}
	if _, err := fetchChannelById(disabled.Id); err == nil || !strings.Contains(err.Error(), "已被禁用") {
		t.Fatalf("expected disabled channel lookup to fail, got %v", err)
	}
}

func TestRelayCommonStreamingAndRetryHelpers(t *testing.T) {
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-5", nil)

	stream := &fakeRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	go func() {
		stream.dataChan <- "event: response.created\ndata: {\"id\":\"resp_stream\"}\n\n"
		stream.errChan <- io.EOF
	}()
	firstResponseTime := responseGeneralStreamClient(ginCtx, stream, func() string {
		return "event: response.done\ndata: {\"id\":\"resp_stream\"}\n\n"
	})
	if firstResponseTime.IsZero() {
		t.Fatal("expected responseGeneralStreamClient to record first response time")
	}
	if body := recorder.Body.String(); !strings.Contains(body, "response.created") || !strings.Contains(body, "response.done") {
		t.Fatalf("expected general stream client to write stream data and end payload, got %q", body)
	}

	observerRecorder := httptest.NewRecorder()
	observerCtx, _ := gin.CreateTestContext(observerRecorder)
	observerCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-5", nil)
	var observed []string
	observerStream := &fakeRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	go func() {
		observerStream.dataChan <- "event: response.created\ndata: {\"id\":\"resp_observed\"}\n\n"
		observerStream.errChan <- errors.New("stream broken")
	}()
	firstResponseTime = responseGeneralStreamClientWithObserver(observerCtx, observerStream, func() string {
		return "ignored-end"
	}, func(line string) {
		observed = append(observed, line)
	})
	if firstResponseTime.IsZero() {
		t.Fatal("expected observed general stream client to record first response time")
	}
	if len(observed) != 1 || !strings.Contains(observed[0], "resp_observed") {
		t.Fatalf("expected observer to receive upstream stream data, got %#v", observed)
	}
	if body := observerRecorder.Body.String(); !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "stream broken") {
		t.Fatalf("expected general stream client error payload, got %q", body)
	}

	jsonRecorder := httptest.NewRecorder()
	jsonCtx, _ := gin.CreateTestContext(jsonRecorder)
	jsonCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-5", nil)
	if errWithCode := responseJsonClient(jsonCtx, map[string]any{"ok": true}); errWithCode != nil {
		t.Fatalf("expected json client helper to succeed, got %v", errWithCode)
	}
	if body := jsonRecorder.Body.String(); !strings.Contains(body, `"ok":true`) {
		t.Fatalf("expected json client helper to write response body, got %q", body)
	}

	retryCtx := newRelayTestContext(nil)
	if !shouldRetry(retryCtx, &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusInternalServerError}, config.ChannelTypeCodex) {
		t.Fatal("expected 5xx responses to remain retryable")
	}
	retryCtx.Set("specific_channel_id", 99)
	if shouldRetry(retryCtx, &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusTooManyRequests}, config.ChannelTypeCodex) {
		t.Fatal("expected explicit channel pins to disable retries")
	}
	if shouldRetry(newRelayTestContext(nil), &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusTooManyRequests, LocalError: true}, config.ChannelTypeCodex) {
		t.Fatal("expected local realtime errors to disable retries")
	}
}

func TestRelayCommonAdditionalProviderAndSelectionBranches(t *testing.T) {
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	if provider, modelName, err := prepareProviderForChannel(newRelayTestContext(nil), "gpt-5", nil); err == nil || provider != nil || modelName != "" {
		t.Fatalf("expected nil channel provider preparation to fail, provider=%#v model=%q err=%v", provider, modelName, err)
	}

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("expected channel schema migration, got %v", err)
	}
	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
	})

	pinnedChannel := newRelayTestCodexChannel(301)
	if err := testDB.Create(pinnedChannel).Error; err != nil {
		t.Fatalf("expected pinned channel fixture to persist, got %v", err)
	}

	pinnedCtx := newRelayTestContext(nil)
	pinnedCtx.Set("specific_channel_id", pinnedChannel.Id)
	if channel, err := fetchChannel(pinnedCtx, "gpt-5"); err != nil || channel == nil || channel.Id != pinnedChannel.Id {
		t.Fatalf("expected fetchChannel to honor explicit channel pin, got channel=%#v err=%v", channel, err)
	}

	canceledCtx := newRelayTestContext(nil)
	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()
	canceledCtx.Request = canceledCtx.Request.WithContext(reqCtx)
	if channel, err := fetchChannelByModelWithSelection(canceledCtx, "gpt-5", realtimeChannelSelection{}); !errors.Is(err, context.Canceled) || channel != nil {
		t.Fatalf("expected canceled request context to stop channel selection, channel=%#v err=%v", channel, err)
	}

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})
	model.ChannelGroup = buildRealtimeTestChannelGroup(11)

	filterCtx := newRelayTestContext(nil)
	filterCtx.Set("token_group", "default")
	filterCtx.Set("skip_only_chat", true)
	filterCtx.Set("is_stream", true)
	if _, err := fetchChannelByModelWithSelection(filterCtx, "missing-model", realtimeChannelSelection{
		skipChannelIDs:    []int{11},
		allowChannelTypes: []int{config.ChannelTypeCodex},
	}); err == nil {
		t.Fatal("expected filtered selection with a missing model to fail")
	}

	observerRecorder := httptest.NewRecorder()
	observerCtx, _ := gin.CreateTestContext(observerRecorder)
	observerCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	var observed []string
	eofStream := &fakeRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	go func() {
		eofStream.dataChan <- "event: response.created\ndata: {\"id\":\"resp_end\"}\n\n"
		eofStream.errChan <- io.EOF
	}()
	responseGeneralStreamClientWithObserver(observerCtx, eofStream, func() string {
		return "event: response.done\ndata: {\"id\":\"resp_end\"}\n\n"
	}, func(line string) {
		observed = append(observed, line)
	})
	if len(observed) != 2 || !strings.Contains(observed[1], "response.done") {
		t.Fatalf("expected observer to receive stream end payload as well, got %#v", observed)
	}

	closedRecorder := httptest.NewRecorder()
	closedCtx, _ := gin.CreateTestContext(closedRecorder)
	closedCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	closedStream := &fakeRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	close(closedStream.dataChan)
	if firstResponseTime := responseGeneralStreamClientWithObserver(closedCtx, closedStream, nil, nil); !firstResponseTime.IsZero() {
		t.Fatalf("expected closed stream without data to return zero first response time, got %v", firstResponseTime)
	}

	closedAfterDataRecorder := httptest.NewRecorder()
	closedAfterDataCtx, _ := gin.CreateTestContext(closedAfterDataRecorder)
	closedAfterDataCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	closedAfterDataStream := &fakeRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	go func() {
		closedAfterDataStream.dataChan <- "event: response.created\ndata: {\"id\":\"resp_close\"}\n\n"
		close(closedAfterDataStream.dataChan)
	}()
	firstResponseTime := responseGeneralStreamClientWithObserver(closedAfterDataCtx, closedAfterDataStream, nil, nil)
	if firstResponseTime.IsZero() {
		t.Fatal("expected data channel close after first chunk to preserve first response time")
	}
	if body := closedAfterDataRecorder.Body.String(); !strings.Contains(body, "resp_close") {
		t.Fatalf("expected closed-after-data stream body to preserve upstream payload, got %q", body)
	}
}
