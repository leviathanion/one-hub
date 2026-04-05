package relay_util

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/logger"
	commonredis "one-api/common/redis"
	"one-api/internal/billing"
	"one-api/internal/testutil/fakeredis"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type quotaMoreContextKey string

func TestNewQuotaDetachesContextAndCopiesAffinityMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logger.Logger = zap.NewNop()

	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{}}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	reqCtx := context.WithValue(context.Background(), quotaMoreContextKey("trace"), "trace-123")
	reqCtx, cancel := context.WithCancel(reqCtx)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil).WithContext(reqCtx)
	ctx.Request.RemoteAddr = "203.0.113.5:1234"
	ctx.Request.Header.Set("User-Agent", "  Codex/1.2  ")
	ctx.Set("id", 11)
	ctx.Set("channel_id", 22)
	ctx.Set("token_id", 33)
	ctx.Set("token_unlimited_quota", true)
	ctx.Set("is_backupGroup", true)
	ctx.Set("token_name", "token-alpha")
	ctx.Set("token_group", "team-a")
	ctx.Set("token_backup_group", "team-b")
	groupctx.SetRoutingGroup(ctx, "team-b", groupctx.RoutingGroupSourceBackupGroup)
	ctx.Set("group_ratio", 1.75)
	ctx.Set(config.GinChannelAffinityMetaKey, map[string]any{
		"channel_affinity_hit":  true,
		"channel_affinity_rule": "realtime-session",
	})

	quota := NewQuota(ctx, "gpt-5", 12)
	if quota == nil {
		t.Fatal("expected NewQuota to build quota state")
	}
	if quota.userId != 11 || quota.channelId != 22 || quota.tokenId != 33 || quota.callerNS != "token:33" || !quota.unlimitedQuota {
		t.Fatalf("expected quota identity fields to be copied from gin context, got %+v", quota)
	}
	if !quota.isBackupGroup || quota.tokenName != "token-alpha" || quota.groupName != "team-b" || quota.tokenGroupName != "team-a" || quota.backupGroupName != "team-b" {
		t.Fatalf("expected quota group metadata to be copied, got %+v", quota)
	}
	if quota.userAgent != "Codex/1.2" {
		t.Fatalf("expected quota user-agent to be normalized from request headers, got %q", quota.userAgent)
	}
	if quota.routingGroupSource != groupctx.RoutingGroupSourceBackupGroup {
		t.Fatalf("expected quota to copy routing group source, got %+v", quota)
	}
	if quota.affinityMeta["channel_affinity_rule"] != "realtime-session" {
		t.Fatalf("expected affinity metadata copy, got %+v", quota.affinityMeta)
	}

	cancel()
	select {
	case <-quota.requestContext.Done():
		t.Fatal("expected detached quota request context to ignore parent cancellation")
	default:
	}
	if got := quota.requestContext.Value(quotaMoreContextKey("trace")); got != "trace-123" {
		t.Fatalf("expected detached quota context to preserve values, got %#v", got)
	}

	affinityMeta := ctx.MustGet(config.GinChannelAffinityMetaKey).(map[string]any)
	affinityMeta["channel_affinity_rule"] = "mutated"
	if quota.affinityMeta["channel_affinity_rule"] != "realtime-session" {
		t.Fatalf("expected quota affinity metadata to be cloned, got %+v", quota.affinityMeta)
	}
}

func TestQuotaComputationMetadataAndRealtimeHelpers(t *testing.T) {
	logger.Logger = zap.NewNop()

	extraRatios := datatypes.NewJSONType(map[string]float64{
		config.UsageExtraInputAudio: 2,
		config.UsageExtraReasoning:  3,
	})
	quota := &Quota{
		modelName:          "gpt-4o-realtime-preview",
		price:              model.Price{Type: model.TokensPriceType, Input: 1, Output: 2, ExtraRatios: &extraRatios},
		groupName:          "team-b",
		tokenGroupName:     "team-a",
		backupGroupName:    "team-b",
		routingGroupSource: groupctx.RoutingGroupSourceBackupGroup,
		isBackupGroup:      true,
		groupRatio:         1.5,
		inputRatio:         1.5,
		outputRatio:        2.5,
		affinityMeta:       map[string]any{"channel_affinity_hit": true},
		extraBillingData:   map[string]ExtraBillingData{"web_search": {ServiceType: types.APIToolTypeWebSearchPreview, CallCount: 1, Price: 0.01}},
		userAgent:          "Codex/1.2",
	}

	usage := &types.Usage{
		PromptTokens:     10,
		CompletionTokens: 4,
		PromptTokensDetails: types.PromptTokensDetails{
			CachedTokens: 2,
		},
		ExtraTokens: map[string]int{
			config.UsageExtraInputAudio: 5,
			config.UsageExtraReasoning:  7,
		},
	}
	promptTokens, completionTokens := quota.getComputeTokensByUsage(usage)
	if promptTokens != 15 || completionTokens != 18 {
		t.Fatalf("expected usage compute tokens to include extra ratios, got prompt=%d completion=%d", promptTokens, completionTokens)
	}

	usageEvent := &types.UsageEvent{
		InputTokens:  8,
		OutputTokens: 3,
		InputTokenDetails: types.PromptTokensDetails{
			AudioTokens: 4,
		},
		OutputTokenDetails: types.CompletionTokensDetails{
			ReasoningTokens: 2,
		},
	}
	eventPromptTokens, eventCompletionTokens := quota.getComputeTokensByUsageEvent(usageEvent)
	if eventPromptTokens != 12 || eventCompletionTokens != 7 {
		t.Fatalf("expected usage event compute tokens to include extra ratios, got prompt=%d completion=%d", eventPromptTokens, eventCompletionTokens)
	}

	if total := quota.GetTotalQuota(0, 0, nil); total != 0 {
		t.Fatalf("expected zero token usage to produce zero quota, got %d", total)
	}
	if total := quota.GetTotalQuotaByUsage(usage); total <= 0 {
		t.Fatalf("expected token usage to produce positive quota, got %d", total)
	}

	timesQuota := &Quota{price: model.Price{Type: model.TimesPriceType}, inputRatio: 1.25}
	if total := timesQuota.GetTotalQuota(2, 3, nil); total != 1250 {
		t.Fatalf("expected times pricing to ignore tokens and scale on input ratio, got %d", total)
	}

	startedAt := time.Unix(1700000000, 0)
	firstResponseAt := startedAt.Add(150 * time.Millisecond)
	quota.SeedTiming(startedAt, firstResponseAt, startedAt.Add(time.Second))
	quota.GetExtraBillingData(map[string]types.ExtraBilling{
		types.APIToolTypeWebSearchPreview: {
			ServiceType: types.APIToolTypeWebSearchPreview,
			CallCount:   1,
		},
	})
	meta := quota.GetLogMeta(usage)
	if meta["group_name"] != "team-b" || meta["using_group"] != "team-b" || meta["token_group"] != "team-a" || meta["backup_group_name"] != "team-b" || meta["is_backup_group"] != true {
		t.Fatalf("expected group metadata in log meta, got %#v", meta)
	}
	if meta["routing_group_source"] != groupctx.RoutingGroupSourceBackupGroup {
		t.Fatalf("expected routing group source in log meta, got %#v", meta)
	}
	if meta["user_agent"] != "Codex/1.2" {
		t.Fatalf("expected normalized user-agent in log meta, got %#v", meta)
	}
	if meta["channel_affinity_hit"] != true || meta["first_response"] != firstResponseAt.Sub(startedAt).Milliseconds() {
		t.Fatalf("expected timing and affinity metadata in log meta, got %#v", meta)
	}
	if _, ok := meta[config.UsageExtraCache]; !ok {
		t.Fatalf("expected usage extra tokens in log meta, got %#v", meta)
	}
	if _, ok := meta["extra_billing"]; !ok {
		t.Fatalf("expected extra billing metadata in log meta, got %#v", meta)
	}
	if quota.GetInputRatio() != 1.5 {
		t.Fatalf("expected GetInputRatio passthrough, got %v", quota.GetInputRatio())
	}

	observed := &types.UsageEvent{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}
	if err := quota.UpdateUserRealtimeQuota(observed, &types.UsageEvent{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}); err != nil {
		t.Fatalf("expected realtime quota update without redis to succeed, got %v", err)
	}
	if observed.InputTokens != 3 || observed.TotalTokens != 7 {
		t.Fatalf("expected realtime usage merge even without redis, got %+v", observed)
	}

	quota.GetExtraBillingData(nil)
	if quota.extraBillingData != nil {
		t.Fatalf("expected empty extra billing to clear metadata, got %+v", quota.extraBillingData)
	}
}

func TestQuotaNormalizesAndTruncatesUserAgent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logger.Logger = zap.NewNop()

	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{}}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	longUA := strings.Repeat("A", 540)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	ctx.Request.Header.Set("User-Agent", longUA)

	quota := NewQuota(ctx, "gpt-5", 1)
	if quota == nil {
		t.Fatal("expected quota to be created")
	}
	if len([]rune(quota.userAgent)) != 512 {
		t.Fatalf("expected user-agent to be truncated to 512 characters, got %d", len([]rune(quota.userAgent)))
	}

	meta := quota.GetLogMeta(&types.Usage{})
	got, _ := meta["user_agent"].(string)
	if len([]rune(got)) != 512 {
		t.Fatalf("expected user-agent metadata to preserve truncation, got %d characters", len([]rune(got)))
	}
}

func useQuotaReserveTestDB(t *testing.T) {
	t.Helper()

	logger.Logger = zap.NewNop()

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.User{}, &model.Token{}); err != nil {
		t.Fatalf("expected quota reserve schema migration to succeed, got %v", err)
	}

	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
	})
}

func insertQuotaReserveFixtures(t *testing.T, quota int) {
	t.Helper()

	if err := model.DB.Create(&model.User{
		Id:          1,
		Username:    "alice",
		Password:    "password123",
		AccessToken: "access-token-1",
		Quota:       quota,
		Group:       "default",
		Status:      config.UserStatusEnabled,
		Role:        config.RoleCommonUser,
		DisplayName: "Alice",
		CreatedTime: 1,
	}).Error; err != nil {
		t.Fatalf("expected user fixture to persist, got %v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id:          1,
		UserId:      1,
		Key:         "token-key-1",
		Name:        "token-alpha",
		RemainQuota: quota,
		Group:       "default",
	}).Error; err != nil {
		t.Fatalf("expected token fixture to persist, got %v", err)
	}
}

func TestForcePreConsumeBypassesTrustedSkipForAsyncTasks(t *testing.T) {
	useQuotaReserveTestDB(t)
	insertQuotaReserveFixtures(t, 200000)

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalRedisEnabled := config.RedisEnabled
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"task-model": {
				Model: "task-model",
				Type:  model.TimesPriceType,
				Input: 1,
			},
		},
	}
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.RedisEnabled = originalRedisEnabled
	})

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/tasks", nil)
	ctx.Request.RemoteAddr = "203.0.113.10:1234"
	ctx.Set("id", 1)
	ctx.Set("channel_id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("token_unlimited_quota", false)
	ctx.Set("token_name", "token-alpha")
	ctx.Set("group_ratio", 1.0)

	trustedQuota := NewQuota(ctx, "task-model", 1000)
	if errWithCode := trustedQuota.PreQuotaConsumption(); errWithCode != nil {
		t.Fatalf("expected trusted pre-consume check to succeed, got %+v", errWithCode)
	}

	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected trusted user lookup to succeed, got %v", err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected trusted token lookup to succeed, got %v", err)
	}
	if user.Quota != 200000 || token.RemainQuota != 200000 || token.UsedQuota != 0 {
		t.Fatalf("expected trusted path to skip reserve, got user=%d token_remain=%d token_used=%d", user.Quota, token.RemainQuota, token.UsedQuota)
	}

	forcedQuota := NewQuota(ctx, "task-model", 1000)
	forcedQuota.ForcePreConsume()
	if errWithCode := forcedQuota.PreQuotaConsumption(); errWithCode != nil {
		t.Fatalf("expected forced async reserve to succeed, got %+v", errWithCode)
	}

	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected forced user lookup to succeed, got %v", err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected forced token lookup to succeed, got %v", err)
	}
	if user.Quota != 199000 {
		t.Fatalf("expected forced async reserve to debit user quota immediately, got %d", user.Quota)
	}
	if token.RemainQuota != 199000 || token.UsedQuota != 1000 {
		t.Fatalf("expected forced async reserve to debit token quota immediately, got remain=%d used=%d", token.RemainQuota, token.UsedQuota)
	}
}

func TestQuotaAdditionalNilAndRequestTimeBranches(t *testing.T) {
	var nilQuota *Quota
	if cloned := nilQuota.Clone(); cloned != nil {
		t.Fatalf("expected nil quota clones to stay nil, got %+v", cloned)
	}
	nilQuota.SeedTiming(time.Now(), time.Now(), time.Now())

	frozenNegative := &Quota{
		requestFrozen:   true,
		requestDuration: -time.Second,
	}
	if got := frozenNegative.getRequestTime(); got != 0 {
		t.Fatalf("expected negative frozen request durations to clamp to zero, got %d", got)
	}

	if got := (&Quota{}).getRequestTime(); got != 0 {
		t.Fatalf("expected zero-value quotas to report zero request time, got %d", got)
	}
}

func TestConsumeUsageSettlementReconcilesRealtimeQuotaOnError(t *testing.T) {
	logger.Logger = zap.NewNop()

	server, err := fakeredis.Start()
	if err != nil {
		t.Fatalf("expected fake redis server to start, got %v", err)
	}
	defer server.Close()
	server.RegisterLuaScript(`
		local key = KEYS[1]
		local increment = tonumber(ARGV[1])
		local expiration = tonumber(ARGV[2])

		local exists = redis.call("EXISTS", key)
		if exists == 0 then
			if increment < 0 then
				return 0
			end
			redis.call("SET", key, "0", "EX", expiration)
		end

		local newValue = redis.call("INCRBY", key, increment)
		redis.call("EXPIRE", key, expiration)

		return newValue
	`, func(keys, args []string) int64 {
		currentRaw, exists := server.GetRaw(keys[0])
		currentValue := int64(0)
		if exists {
			fmt.Sscanf(currentRaw, "%d", &currentValue)
		}
		var increment int64
		fmt.Sscanf(args[0], "%d", &increment)
		if !exists && increment < 0 {
			return 0
		}
		newValue := currentValue + increment
		server.SetRaw(keys[0], fmt.Sprintf("%d", newValue))
		return newValue
	})

	originalRedisEnabled := config.RedisEnabled
	originalRedisClient := commonredis.RDB
	originalDB := model.DB
	config.RedisEnabled = true
	commonredis.RDB = server.Client()
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	model.DB = testDB
	t.Cleanup(func() {
		config.RedisEnabled = originalRedisEnabled
		commonredis.RDB = originalRedisClient
		model.DB = originalDB
	})

	realtimeKey := fmt.Sprintf(model.UserRealtimeQuotaKey, 1)
	server.SetRaw(realtimeKey, "80")

	quota := &Quota{
		modelName:      "gpt-5",
		price:          model.Price{Type: model.TokensPriceType, Input: 1, Output: 1},
		inputRatio:     1,
		outputRatio:    1,
		userId:         1,
		tokenId:        1,
		channelId:      1,
		cacheQuota:     80,
		requestContext: context.Background(),
	}

	err = quota.ConsumeUsageWithIdentity(
		&types.Usage{PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20},
		false,
		billing.SettlementRequestKindRealtimeTurn,
		"session-1:1:finalize",
		false,
	)
	if err == nil {
		t.Fatal("expected settlement to fail against an unmigrated database")
	}

	if quota.cacheQuota != 0 {
		t.Fatalf("expected failed settlement to reconcile realtime cache, got %d", quota.cacheQuota)
	}
	if got, _ := server.GetRaw(realtimeKey); got != "0" {
		t.Fatalf("expected realtime quota key to be cleared after settlement failure, got %q", got)
	}
}
