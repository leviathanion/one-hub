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
	if total := quota.GetTotalQuota(0, 0, map[string]types.ExtraBilling{
		types.APIToolTypeWebSearchPreview: {
			ServiceType: types.APIToolTypeWebSearchPreview,
			Type:        "medium",
			CallCount:   1,
		},
	}); total != 0 {
		t.Fatalf("expected zero-token extra-billing-only usage to produce zero quota under legacy rules, got %d", total)
	}
	if total := quota.GetTotalQuotaByUsage(usage); total <= 0 {
		t.Fatalf("expected token usage to produce positive quota, got %d", total)
	}

	timesQuota := &Quota{price: model.Price{Type: model.TimesPriceType}, inputRatio: 1.25}
	if total := timesQuota.GetTotalQuota(2, 3, nil); total != 1250 {
		t.Fatalf("expected times pricing to ignore tokens and scale on input ratio, got %d", total)
	}
	if err := timesQuota.UpdateUserRealtimeQuota(&types.UsageEvent{}, &types.UsageEvent{InputTokens: 1, OutputTokens: 1}); err != nil {
		t.Fatalf("expected times pricing realtime deltas to skip hard-cap quota increments, got %v", err)
	}
	if timesQuota.cacheQuota != 0 {
		t.Fatalf("expected times pricing realtime deltas not to increase realtime cache quota, got %d", timesQuota.cacheQuota)
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

func TestQuotaNonRedisRealtimeHardCapRejectsOverBudgetUsage(t *testing.T) {
	useQuotaReserveTestDB(t)
	insertQuotaReserveFixtures(t, 5)

	originalRedisEnabled := config.RedisEnabled
	config.RedisEnabled = false
	t.Cleanup(func() {
		config.RedisEnabled = originalRedisEnabled
	})

	quota := &Quota{
		userId:      1,
		price:       model.Price{Type: model.TokensPriceType, Input: 1, Output: 1},
		inputRatio:  1,
		outputRatio: 1,
	}
	observed := &types.UsageEvent{}
	err := quota.UpdateUserRealtimeQuota(observed, &types.UsageEvent{InputTokens: 3, OutputTokens: 4, TotalTokens: 7})
	if err == nil || !strings.Contains(err.Error(), "user quota is not enough") {
		t.Fatalf("expected non-redis realtime hard cap to reject over-budget usage, got %v", err)
	}
	if observed.InputTokens != 3 || observed.OutputTokens != 4 || observed.TotalTokens != 7 {
		t.Fatalf("expected hard-cap check to preserve observed usage merge, got %+v", observed)
	}
}

func TestPreQuotaConsumptionRollbackableIsIdempotent(t *testing.T) {
	useQuotaReserveTestDB(t)
	insertQuotaReserveFixtures(t, 1000)

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalRedisEnabled := config.RedisEnabled
	originalPreConsumedQuota := config.PreConsumedQuota
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"gpt-5": {Model: "gpt-5", Type: model.TokensPriceType, Input: 1, Output: 1},
		},
	}
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	config.PreConsumedQuota = 500
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.RedisEnabled = originalRedisEnabled
		config.PreConsumedQuota = originalPreConsumedQuota
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("group_ratio", 1.0)

	quota := NewQuota(ctx, "gpt-5", 100)
	if apiErr := quota.PreQuotaConsumptionRollbackable(); apiErr != nil {
		t.Fatalf("expected first preconsume to succeed, got %+v", apiErr)
	}
	if apiErr := quota.PreQuotaConsumptionRollbackable(); apiErr != nil {
		t.Fatalf("expected repeated preconsume to be a no-op, got %+v", apiErr)
	}

	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup to succeed, got %v", err)
	}
	var token model.Token
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup to succeed, got %v", err)
	}
	if user.Quota != 400 || token.RemainQuota != 400 || token.UsedQuota != 600 {
		t.Fatalf("expected repeated preconsume to debit exactly once, user=%d token_remain=%d token_used=%d", user.Quota, token.RemainQuota, token.UsedQuota)
	}
}

func TestConsumeSettlementSuccessClearsPreConsumeRollbackState(t *testing.T) {
	useQuotaReserveTestDB(t)
	insertQuotaReserveFixtures(t, 1000)

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalRedisEnabled := config.RedisEnabled
	originalLogConsume := config.LogConsumeEnabled
	originalPreConsumedQuota := config.PreConsumedQuota
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"gpt-5": {Model: "gpt-5", Type: model.TokensPriceType, Input: 1, Output: 1},
		},
	}
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	config.LogConsumeEnabled = false
	config.PreConsumedQuota = 500
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.RedisEnabled = originalRedisEnabled
		config.LogConsumeEnabled = originalLogConsume
		config.PreConsumedQuota = originalPreConsumedQuota
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("group_ratio", 1.0)

	quota := NewQuota(ctx, "gpt-5", 100)
	if apiErr := quota.PreQuotaConsumptionRollbackable(); apiErr != nil {
		t.Fatalf("expected preconsume to succeed, got %+v", apiErr)
	}
	if !quota.HasPreConsumedSideEffect() {
		t.Fatal("expected preconsume side effect before settlement")
	}

	if err := quota.ConsumeWithIdentity(ctx, &types.Usage{PromptTokens: 800, TotalTokens: 800}, false, billing.SettlementRequestKindUnary, "", false); err != nil {
		t.Fatalf("expected settlement to succeed, got %v", err)
	}
	if quota.HasPreConsumedSideEffect() {
		t.Fatalf("expected successful settlement to clear rollback state, truth=%v cache=%v", quota.PreconsumeTruthApplied, quota.PreconsumeCacheApplied)
	}
	if err := quota.UndoSynchronously(ctx); err != nil {
		t.Fatalf("expected post-settlement undo to be a no-op, got %v", err)
	}

	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup to succeed, got %v", err)
	}
	var token model.Token
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup to succeed, got %v", err)
	}
	if user.Quota != 200 || user.UsedQuota != 800 || user.RequestCount != 1 {
		t.Fatalf("expected final quota to stay settled after undo, quota=%d used=%d requests=%d", user.Quota, user.UsedQuota, user.RequestCount)
	}
	if token.RemainQuota != 200 || token.UsedQuota != 800 {
		t.Fatalf("expected final token quota to stay settled after undo, remain=%d used=%d", token.RemainQuota, token.UsedQuota)
	}
}

func TestPreQuotaConsumptionRollbackableRollsBackTruthWhenCacheReserveFails(t *testing.T) {
	useQuotaReserveTestDB(t)
	insertQuotaReserveFixtures(t, 1000)

	server, err := fakeredis.Start()
	if err != nil {
		t.Fatalf("expected fake redis server to start, got %v", err)
	}
	defer server.Close()
	server.FailNext("DECRBY", "ERR cache unavailable")

	originalPricing := model.PricingInstance
	originalRedisEnabled := config.RedisEnabled
	originalRedisClient := commonredis.RDB
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"gpt-5": {Model: "gpt-5", Type: model.TokensPriceType, Input: 1, Output: 1},
		},
	}
	config.RedisEnabled = true
	commonredis.RDB = server.Client()
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.RedisEnabled = originalRedisEnabled
		commonredis.RDB = originalRedisClient
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("group_ratio", 1.0)

	quota := NewQuota(ctx, "gpt-5", 100)
	apiErr := quota.PreQuotaConsumptionRollbackable()
	if apiErr == nil || apiErr.Code != "decrease_user_quota_failed" {
		t.Fatalf("expected cache reserve failure to surface a quota cache error, got %+v", apiErr)
	}
	if quota.PreconsumeTruthApplied || quota.PreconsumeCacheApplied {
		t.Fatalf("expected failed cache reserve to roll back all preconsume state, got truth=%v cache=%v", quota.PreconsumeTruthApplied, quota.PreconsumeCacheApplied)
	}

	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup to succeed, got %v", err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup to succeed, got %v", err)
	}
	if user.Quota != 1000 || token.RemainQuota != 1000 || token.UsedQuota != 0 {
		t.Fatalf("expected failed cache reserve to restore truth state, user=%+v token=%+v", user, token)
	}
}

func TestPreQuotaConsumptionRollsBackTruthWhenCacheReserveFails(t *testing.T) {
	useQuotaReserveTestDB(t)
	insertQuotaReserveFixtures(t, 1000)

	server, err := fakeredis.Start()
	if err != nil {
		t.Fatalf("expected fake redis server to start, got %v", err)
	}
	defer server.Close()
	server.FailNext("DECRBY", "ERR cache unavailable")

	originalPricing := model.PricingInstance
	originalRedisEnabled := config.RedisEnabled
	originalRedisClient := commonredis.RDB
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"gpt-5": {Model: "gpt-5", Type: model.TokensPriceType, Input: 1, Output: 1},
		},
	}
	config.RedisEnabled = true
	commonredis.RDB = server.Client()
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.RedisEnabled = originalRedisEnabled
		commonredis.RDB = originalRedisClient
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("group_ratio", 1.0)

	quota := NewQuota(ctx, "gpt-5", 100)
	apiErr := quota.PreQuotaConsumption()
	if apiErr == nil || apiErr.Code != "decrease_user_quota_failed" {
		t.Fatalf("expected ordinary preconsume cache failure to surface a quota cache error, got %+v", apiErr)
	}
	if quota.PreconsumeTruthApplied || quota.PreconsumeCacheApplied {
		t.Fatalf("expected ordinary preconsume failure to roll back all state, got truth=%v cache=%v", quota.PreconsumeTruthApplied, quota.PreconsumeCacheApplied)
	}

	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup to succeed, got %v", err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup to succeed, got %v", err)
	}
	if user.Quota != 1000 || token.RemainQuota != 1000 || token.UsedQuota != 0 {
		t.Fatalf("expected ordinary preconsume failure to restore truth state, user=%+v token=%+v", user, token)
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

func TestConsumeUsageSettlementFailurePreservesPreConsumedTruth(t *testing.T) {
	logger.Logger = zap.NewNop()
	gin.SetMode(gin.TestMode)

	originalDB := model.DB
	originalPricing := model.PricingInstance
	originalBatchUpdate := config.BatchUpdateEnabled
	originalRedisEnabled := config.RedisEnabled
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.User{}, &model.Token{}, &model.Log{}); err != nil {
		t.Fatalf("expected schema migration to succeed, got %v", err)
	}
	model.DB = testDB
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"gpt-5": {Model: "gpt-5", Type: model.TokensPriceType, Input: 1, Output: 1},
		},
	}
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	t.Cleanup(func() {
		model.DB = originalDB
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatchUpdate
		config.RedisEnabled = originalRedisEnabled
	})

	if err := model.DB.Create(&model.User{Id: 1, Username: "alice", Quota: 1000, Status: config.UserStatusEnabled, CreatedTime: 1}).Error; err != nil {
		t.Fatalf("expected user fixture to persist, got %v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{Id: 1, UserId: 1, Key: "token-key", RemainQuota: 1000}).Error; err != nil {
		t.Fatalf("expected token fixture to persist, got %v", err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("group_ratio", 1.0)

	quota := NewQuota(ctx, "gpt-5", 100)
	if apiErr := quota.PreQuotaConsumptionRollbackable(); apiErr != nil {
		t.Fatalf("expected preconsume to succeed, got %v", apiErr)
	}
	if !quota.PreconsumeTruthApplied {
		t.Fatal("expected preconsume truth side effect before settlement")
	}
	var preUser model.User
	var preToken model.Token
	if err := model.DB.First(&preUser, 1).Error; err != nil {
		t.Fatalf("expected preconsume user lookup to succeed, got %v", err)
	}
	if err := model.DB.First(&preToken, 1).Error; err != nil {
		t.Fatalf("expected preconsume token lookup to succeed, got %v", err)
	}
	if err := model.DB.Exec(`CREATE TRIGGER fail_user_quota_decrease BEFORE UPDATE OF quota ON users WHEN NEW.quota < OLD.quota BEGIN SELECT RAISE(FAIL, 'blocked quota decrease'); END;`).Error; err != nil {
		t.Fatalf("expected failure trigger to install, got %v", err)
	}

	err = quota.ConsumeWithIdentity(ctx, &types.Usage{PromptTokens: 800, TotalTokens: 800}, false, billing.SettlementRequestKindRealtimeTurn, "settlement-fail", false)
	if err == nil {
		t.Fatal("expected settlement to fail")
	}
	if !quota.PreconsumeTruthApplied {
		t.Fatal("expected failed settlement to preserve preconsume truth")
	}

	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup to succeed, got %v", err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup to succeed, got %v", err)
	}
	if user.Quota != preUser.Quota || token.RemainQuota != preToken.RemainQuota || token.UsedQuota != preToken.UsedQuota {
		t.Fatalf("expected failed settlement to keep preconsume truth, before_user=%+v after_user=%+v before_token=%+v after_token=%+v", preUser, user, preToken, token)
	}
}
