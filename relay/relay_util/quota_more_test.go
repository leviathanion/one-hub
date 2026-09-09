package relay_util

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/logger"
	"one-api/internal/billing"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type quotaMoreContextKey string

func useCurrentQuotaPrice(t *testing.T, quota *Quota, price model.Price) {
	t.Helper()
	modelName := quota.modelName
	if quota.groupName == "" {
		quota.groupName = "quota-test"
	}
	if quota.groupRatio <= 0 {
		quota.groupRatio = 1
	}
	useCurrentQuotaGroup(t, quota.groupName, quota.groupRatio)
	originalPricing := model.PricingInstance
	price.Model = modelName
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{modelName: &price}}
	t.Cleanup(func() { model.PricingInstance = originalPricing })
}

func useCurrentQuotaGroup(t *testing.T, symbol string, ratio float64) {
	t.Helper()
	model.GlobalUserGroupRatio.Lock()
	original := model.GlobalUserGroupRatio.UserGroup
	groups := make(map[string]*model.UserGroup, len(original)+1)
	for key, value := range original {
		groups[key] = value
	}
	groups[symbol] = &model.UserGroup{Symbol: symbol, Ratio: ratio}
	model.GlobalUserGroupRatio.UserGroup = groups
	model.GlobalUserGroupRatio.Unlock()
	t.Cleanup(func() {
		model.GlobalUserGroupRatio.Lock()
		model.GlobalUserGroupRatio.UserGroup = original
		model.GlobalUserGroupRatio.Unlock()
	})
}

func setQuotaTestRoutingGroup(t *testing.T, ctx *gin.Context, ratio float64) {
	t.Helper()
	const symbol = "quota-test"
	groupctx.SetRoutingGroup(ctx, symbol, groupctx.RoutingGroupSourceUserGroup)
	ctx.Set("group_ratio", ratio)
	useCurrentQuotaGroup(t, symbol, ratio)
}

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
	ctx.Set("billing_original_model", true)
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
	if quota.userId != 11 || quota.channelId != 22 || quota.tokenId != 33 || quota.callerNS != "token:33" {
		t.Fatalf("expected quota identity fields to be copied from gin context, got %+v", quota)
	}
	if !quota.isBackupGroup || quota.tokenName != "token-alpha" || quota.groupName != "team-b" || quota.tokenGroupName != "team-a" || quota.backupGroupName != "team-b" {
		t.Fatalf("expected quota group metadata to be copied, got %+v", quota)
	}
	if !quota.freezeRequestPricePolicy {
		t.Fatal("expected quota to preserve original-model billing policy")
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
	useCurrentQuotaPrice(t, quota, quota.price)

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
	if total := mustEvaluateProviderQuota(t, quota, &types.Usage{ExtraBilling: map[string]types.ExtraBilling{
		types.APIToolTypeWebSearchPreview: {
			ServiceType: types.APIToolTypeWebSearchPreview,
			Type:        "medium",
			CallCount:   1,
		},
	}}); total <= 0 {
		t.Fatalf("expected priced extra billing to survive zero-token usage, got %d", total)
	}
	if total := mustEvaluateProviderQuota(t, quota, usage); total <= 0 {
		t.Fatalf("expected token usage to produce positive quota, got %d", total)
	}

	startedAt := time.Unix(1700000000, 0)
	firstResponseAt := startedAt.Add(150 * time.Millisecond)
	quota.SeedTiming(startedAt, firstResponseAt, startedAt.Add(time.Second))
	mustEvaluateProviderQuota(t, quota, &types.Usage{ExtraBilling: map[string]types.ExtraBilling{
		types.APIToolTypeWebSearchPreview: {
			ServiceType: types.APIToolTypeWebSearchPreview,
			CallCount:   1,
		},
	}})
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
	mustEvaluateProviderQuota(t, quota, usage)
	if quota.extraBillingData != nil {
		t.Fatalf("expected empty extra billing to clear metadata, got %+v", quota.extraBillingData)
	}

	timesQuota := &Quota{modelName: "times-model", price: model.Price{Model: "times-model", Type: model.TimesPriceType, Input: 1.25}, groupRatio: 1, inputRatio: 1.25}
	useCurrentQuotaPrice(t, timesQuota, timesQuota.price)
	timesUsage := &types.Usage{}
	timesUsage.MarkProviderOperationUnits(1)
	if total := mustEvaluateProviderQuota(t, timesQuota, timesUsage); total != 1250 {
		t.Fatalf("expected times pricing to ignore tokens and scale on input ratio, got %d", total)
	}
}

func TestQuotaBillsExtraBillingWithoutTokenUsage(t *testing.T) {
	quota := &Quota{
		modelName:   "gpt-5",
		price:       model.Price{Type: model.TokensPriceType, Input: 1, Output: 1},
		groupRatio:  1,
		inputRatio:  1,
		outputRatio: 1,
	}
	useCurrentQuotaPrice(t, quota, quota.price)
	usage := &types.Usage{ExtraBilling: map[string]types.ExtraBilling{
		types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium"): {
			ServiceType: types.APIToolTypeWebSearchPreview,
			Type:        "medium",
			CallCount:   1,
		},
	}}
	if got := mustEvaluateProviderQuota(t, quota, usage); got != 5000 {
		t.Fatalf("extra-billing-only quota=%d, want 5000", got)
	}
	if got := mustEvaluateProviderQuota(t, quota, &types.Usage{}); got != 0 {
		t.Fatalf("empty usage quota=%d, want 0", got)
	}
	unknown := &types.Usage{ExtraBilling: map[string]types.ExtraBilling{
		"future_tool": {ServiceType: "future_tool", CallCount: 1},
	}}
	if got := mustEvaluateProviderQuota(t, quota, unknown); got != 0 {
		t.Fatalf("unpriced extra billing quota=%d, want 0", got)
	}
}

func TestQuotaTokenAndPriceArithmeticSaturatesInsteadOfWrapping(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	extraRatios := datatypes.NewJSONType(map[string]float64{config.UsageExtraInputAudio: 2})
	quota := &Quota{
		modelName:   "overflow-test",
		price:       model.Price{Type: model.TokensPriceType, Input: 1, Output: 1, ExtraRatios: &extraRatios},
		groupRatio:  1,
		inputRatio:  1,
		outputRatio: 1,
	}
	useCurrentQuotaPrice(t, quota, quota.price)
	usage := &types.Usage{
		PromptTokens: maxInt - 5,
		ExtraTokens:  map[string]int{config.UsageExtraInputAudio: 10},
	}
	if got := mustEvaluateProviderQuota(t, quota, usage); got != maxInt {
		t.Fatalf("quota conversion wrapped to %d, want saturation at %d", got, maxInt)
	}
}

func TestQuotaReservationArithmeticSaturatesInsteadOfBypassingAdmission(t *testing.T) {
	quota := &Quota{modelName: "overflow-reservation", groupRatio: 1}
	useCurrentQuotaPrice(t, quota, model.Price{Type: model.TimesPriceType, Input: math.MaxFloat64})
	got, err := quota.ReservationQuota()
	if err != nil {
		t.Fatal(err)
	}
	if want := int(^uint(0) >> 1); got != want {
		t.Fatalf("reservation overflow=%d want saturation=%d", got, want)
	}
}

func TestQuotaKeepsFractionalExtraTokenMultiplierUntilFinalRounding(t *testing.T) {
	extraRatios := datatypes.NewJSONType(map[string]float64{config.UsageExtraCacheWrite: 1.5})
	quota := &Quota{
		modelName:   "fractional-extra-token-test",
		price:       model.Price{Type: model.TokensPriceType, Input: 1, Output: 1, ExtraRatios: &extraRatios},
		groupRatio:  1,
		inputRatio:  1,
		outputRatio: 1,
	}
	useCurrentQuotaPrice(t, quota, quota.price)
	usage := &types.Usage{
		PromptTokens: 1,
		ExtraTokens:  map[string]int{config.UsageExtraCacheWrite: 1},
	}
	if got := mustEvaluateProviderQuota(t, quota, usage); got != 2 {
		t.Fatalf("fractional token multiplier quota=%d, want ceil(1.5)=2", got)
	}
}

func TestQuotaPricesTranscriptionTokenAndDurationDimensions(t *testing.T) {
	extraRatios := datatypes.NewJSONType(map[string]float64{config.UsageExtraInputAudioTranscription: 4})
	quota := &Quota{
		modelName:   "transcription-test",
		price:       model.Price{Type: model.TokensPriceType, Input: 1, Output: 1, ExtraRatios: &extraRatios},
		groupRatio:  1,
		inputRatio:  1,
		outputRatio: 1,
	}
	useCurrentQuotaPrice(t, quota, quota.price)
	tokenUsage := &types.UsageEvent{Source: types.UsageSourceInputAudioTranscription, BillingBasis: types.UsageBillingBasisTokens}
	tokenUsage.SetProviderIndependentUsageUnit(config.UsageExtraInputAudioTranscription, 7)
	if decision := quota.EvaluateProviderUsage(tokenUsage.ToChatUsage()); !decision.Confirm || decision.FinalQuota != 28 {
		t.Fatalf("transcription token decision=%+v, want 28", decision)
	}
	mixedUsage := &types.UsageEvent{InputTokens: 10, TotalTokens: 10, Source: types.UsageSourceRealtimeResponse, ProviderTokenEvidence: true}
	mixedUsage.Merge(tokenUsage)
	if decision := quota.EvaluateProviderUsage(mixedUsage.ToChatUsage()); !decision.Confirm || decision.FinalQuota != 38 {
		t.Fatalf("main-model plus transcription decision=%+v, want 38", decision)
	}
	durationUsage := &types.UsageEvent{Source: types.UsageSourceInputAudioTranscription, BillingBasis: types.UsageBillingBasisDuration}
	durationUsage.SetProviderIndependentUsageUnit(config.UsageExtraInputAudioTranscription, 2.5)
	if decision := quota.EvaluateProviderUsage(durationUsage.ToChatUsage()); !decision.Confirm || decision.FinalQuota != 10 {
		t.Fatalf("transcription duration decision=%+v, want 10", decision)
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
	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.User{}, &model.Token{}, &model.UserGroup{}); err != nil {
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

func TestAttemptQuotaApplyReserveIsIdempotent(t *testing.T) {
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
	setQuotaTestRoutingGroup(t, ctx, 1)

	attempt, err := NewAttemptQuota(ctx, "gpt-5", 100, BillingAttemptSpec{})
	if err != nil {
		t.Fatalf("create billing attempt: %v", err)
	}
	if err := attempt.ApplyReserve(ctx.Request.Context()); err != nil {
		t.Fatalf("expected first reserve to succeed, got %+v", err)
	}
	if err := attempt.ApplyReserve(ctx.Request.Context()); err != nil {
		t.Fatalf("expected repeated reserve to be a no-op, got %+v", err)
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

func TestAttemptQuotaCloseWithoutSubmissionReturnsAfterRefund(t *testing.T) {
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
	setQuotaTestRoutingGroup(t, ctx, 1)

	attempt, err := NewAttemptQuota(ctx, "gpt-5", 100, BillingAttemptSpec{})
	if err != nil {
		t.Fatalf("create billing attempt: %v", err)
	}
	if err := attempt.ApplyReserve(ctx.Request.Context()); err != nil {
		t.Fatalf("expected reserve to succeed, got %+v", err)
	}
	if _, err := attempt.CloseWithoutSubmission(ctx.Request.Context()); err != nil {
		t.Fatalf("expected unused attempt refund to succeed, got %v", err)
	}

	if attempt.quota.HasPreConsumedSideEffect() {
		t.Fatalf("expected close to clear reservation state before returning, truth=%v", attempt.quota.PreconsumeTruthApplied)
	}
	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup to succeed, got %v", err)
	}
	var token model.Token
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup to succeed, got %v", err)
	}
	if user.Quota != 1000 || token.RemainQuota != 1000 || token.UsedQuota != 0 {
		t.Fatalf("expected close to restore quota before returning, user=%d token_remain=%d token_used=%d", user.Quota, token.RemainQuota, token.UsedQuota)
	}
}

func TestAttemptQuotaCloseDoesNotReplayIndeterminateRefund(t *testing.T) {
	originalApply := applyBillingRefund
	calls := 0
	applyBillingRefund = func(context.Context, int, int, bool, int64) (model.BillingBalanceResult, error) {
		calls++
		return model.BillingBalanceResult{Outcome: model.BillingBalanceCommitUnknown, CommitAttempted: true}, errors.New("commit acknowledgement lost")
	}
	t.Cleanup(func() { applyBillingRefund = originalApply })

	quota := &Quota{userId: 1, tokenId: 1, preConsumedQuota: 100, PreconsumeTruthApplied: true}
	attempt := &AttemptQuota{quota: quota, reserveApplied: true}
	if _, err := attempt.CloseWithoutSubmission(context.Background()); err == nil {
		t.Fatal("expected indeterminate refund commit to remain observable")
	}
	if quota.PreconsumeTruthApplied {
		t.Fatal("indeterminate commit must clear the in-process truth replay bit")
	}
	if _, err := attempt.CloseWithoutSubmission(context.Background()); err == nil {
		t.Fatal("second cleanup must preserve the unknown outcome diagnostic")
	}
	if calls != 1 {
		t.Fatalf("indeterminate refund was replayed %d times", calls)
	}
}

func TestAttemptQuotaSettlementSuccessClearsReservationState(t *testing.T) {
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
	setQuotaTestRoutingGroup(t, ctx, 1)

	attempt, err := NewAttemptQuota(ctx, "gpt-5", 100, BillingAttemptSpec{})
	if err != nil {
		t.Fatalf("create billing attempt: %v", err)
	}
	if err := attempt.ApplyReserve(ctx.Request.Context()); err != nil {
		t.Fatalf("expected reserve to succeed, got %+v", err)
	}
	if !attempt.quota.HasPreConsumedSideEffect() {
		t.Fatal("expected reservation side effect before settlement")
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatalf("claim submission: %v", err)
	}
	usage := &types.Usage{PromptTokens: 800, TotalTokens: 800}
	usage.MarkProviderReported()
	if result, err := attempt.CloseFromProviderResult(ctx.Request.Context(), usage, false); err != nil || !result.Confirmed || result.ChargedQuota != 800 {
		t.Fatalf("expected settlement to succeed, got %v", err)
	}
	if attempt.quota.HasPreConsumedSideEffect() {
		t.Fatalf("expected successful settlement to clear reservation state, truth=%v", attempt.quota.PreconsumeTruthApplied)
	}
	if result, err := attempt.CloseWithoutSubmission(ctx.Request.Context()); err != nil || !result.Confirmed || result.ChargedQuota != 800 {
		t.Fatalf("expected repeated close to preserve settlement result, result=%+v err=%v", result, err)
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

func TestAttemptQuotaReservePreservesIndeterminateCommitOutcome(t *testing.T) {
	useQuotaReserveTestDB(t)
	insertQuotaReserveFixtures(t, 1000)
	originalPricing := model.PricingInstance
	originalRedisEnabled := config.RedisEnabled
	originalApply := applyBillingReserve
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"gpt-5": {Model: "gpt-5", Type: model.TokensPriceType, Input: 1, Output: 1},
	}}
	config.RedisEnabled = false
	applyCalls := 0
	applyBillingReserve = func(context.Context, int, int, int64) (model.BillingBalanceResult, error) {
		applyCalls++
		return model.BillingBalanceResult{Outcome: model.BillingBalanceCommitUnknown, CommitAttempted: true}, errors.New("commit acknowledgement lost")
	}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.RedisEnabled = originalRedisEnabled
		applyBillingReserve = originalApply
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	setQuotaTestRoutingGroup(t, ctx, 1)
	attempt, err := NewAttemptQuota(ctx, "gpt-5", 100, BillingAttemptSpec{})
	if err != nil {
		t.Fatalf("create billing attempt: %v", err)
	}
	reserveErr := attempt.ApplyReserve(ctx.Request.Context())
	apiErr := BillingAPIError(reserveErr, "reserve_failed", http.StatusInternalServerError)
	if apiErr == nil || apiErr.Code != "pre_consume_commit_indeterminate" || !attempt.quota.PreconsumeTruthIndeterminate || !attempt.quota.HasPreConsumedSideEffect() {
		t.Fatalf("expected explicit indeterminate reserve state, err=%+v quota=%+v", apiErr, attempt.quota)
	}
	if _, err := attempt.CloseWithoutSubmission(ctx.Request.Context()); err == nil || applyCalls != 1 {
		t.Fatalf("indeterminate reserve must not be replayed as a refund, err=%v calls=%d", err, applyCalls)
	}
}

func TestEveryBillingAttemptUsesSQLReservation(t *testing.T) {
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
	setQuotaTestRoutingGroup(t, ctx, 1)

	trustedAttempt, err := NewAttemptQuota(ctx, "task-model", 1000, BillingAttemptSpec{})
	if err != nil {
		t.Fatalf("create first billing attempt: %v", err)
	}
	if err := trustedAttempt.ApplyReserve(ctx.Request.Context()); err != nil {
		t.Fatalf("expected trusted reserve to succeed, got %+v", err)
	}

	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected trusted user lookup to succeed, got %v", err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected trusted token lookup to succeed, got %v", err)
	}
	if user.Quota != 199000 || token.RemainQuota != 199000 || token.UsedQuota != 1000 {
		t.Fatalf("ordinary attempt did not reserve in SQL, user=%d token_remain=%d token_used=%d", user.Quota, token.RemainQuota, token.UsedQuota)
	}

	secondAttempt, err := NewAttemptQuota(ctx, "task-model", 1000, BillingAttemptSpec{})
	if err != nil {
		t.Fatalf("create second billing attempt: %v", err)
	}
	if err := secondAttempt.ApplyReserve(ctx.Request.Context()); err != nil {
		t.Fatalf("expected second reserve to succeed, got %+v", err)
	}

	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected second user lookup to succeed, got %v", err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected second token lookup to succeed, got %v", err)
	}
	if user.Quota != 198000 {
		t.Fatalf("expected every attempt to debit user quota immediately, got %d", user.Quota)
	}
	if token.RemainQuota != 198000 || token.UsedQuota != 2000 {
		t.Fatalf("expected every attempt to debit token quota immediately, got remain=%d used=%d", token.RemainQuota, token.UsedQuota)
	}
}

func TestQuotaReadsCurrentRuntimeBillingOptionsAtEachStage(t *testing.T) {
	originalOptions := config.GlobalOption
	manager := config.NewOptionManager()
	preConsumed := 0
	quotaPerUnit := 1.0
	manager.RegisterIntOption("PreConsumedQuota", &preConsumed, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	manager.RegisterFloatOption("QuotaPerUnit", &quotaPerUnit, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{"PreConsumedQuota": "7", "QuotaPerUnit": "100"}); err != nil {
		t.Fatal(err)
	}
	config.GlobalOption = manager
	t.Cleanup(func() { config.GlobalOption = originalOptions })

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	setQuotaTestRoutingGroup(t, ctx, 1)
	quota := NewQuota(ctx, "model", 1)
	if _, err := manager.PublishRuntimeOverrides(2, map[string]string{"PreConsumedQuota": "70", "QuotaPerUnit": "1000"}); err != nil {
		t.Fatal(err)
	}
	if quota.effectivePreConsumedQuotaBase() != 70 || quota.effectiveQuotaPerUnit() != 1000 {
		t.Fatalf("quota did not observe current options: pre=%d unit=%v", quota.effectivePreConsumedQuotaBase(), quota.effectiveQuotaPerUnit())
	}
	if config.GlobalOption.RuntimeSnapshot().Version() != 2 {
		t.Fatalf("request did not observe current options: version=%d", config.GlobalOption.RuntimeSnapshot().Version())
	}
}

func TestQuotaUsesCurrentLocalPriceWhenReserveStarts(t *testing.T) {
	useQuotaReserveTestDB(t)
	insertQuotaReserveFixtures(t, 200000)
	if err := model.DB.AutoMigrate(&model.Price{}, &model.ModelInfo{}, &model.PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := model.EnsurePublicationVersionRows(model.DB); err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Create(&model.Price{Model: "priced", Type: model.TokensPriceType, Input: 1, Output: 1}).Error; err != nil {
		t.Fatal(err)
	}

	originalPricing := model.PricingInstance
	originalOptions := config.GlobalOption
	manager := config.NewOptionManager()
	preConsumed := 0
	quotaPerUnit := 1.0
	manager.RegisterIntOption("PreConsumedQuota", &preConsumed, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	manager.RegisterFloatOption("QuotaPerUnit", &quotaPerUnit, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	if _, err := manager.PublishRuntimeOverrides(1, nil); err != nil {
		t.Fatal(err)
	}
	config.GlobalOption = manager
	model.PricingInstance = &model.Pricing{Prices: make(map[string]*model.Price)}
	if err := model.PricingInstance.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.GlobalOption = originalOptions
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	setQuotaTestRoutingGroup(t, ctx, 1)
	attempt, err := NewAttemptQuota(ctx, "priced", 100, BillingAttemptSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Model(&model.Price{}).Where("model = ?", "priced").Update("input", 2).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := model.BumpPublicationVersionCAS(context.Background(), model.DB, model.PublicationOwnerPrice, 1); err != nil {
		t.Fatal(err)
	}
	if err := model.PricingInstance.Init(); err != nil {
		t.Fatal(err)
	}
	if err := attempt.ApplyReserve(ctx.Request.Context()); err != nil {
		t.Fatalf("reserve with current price failed: %+v", err)
	}
	quota := attempt.Quota()
	if quota.PreConsumedQuota() != 200 {
		t.Fatalf("quota did not use current local price: reserved=%d", quota.PreConsumedQuota())
	}
	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil || user.Quota != 199800 {
		t.Fatalf("reserve used wrong price: quota=%d err=%v", user.Quota, err)
	}
}

func TestQuotaAdditionalNilAndRequestTimeBranches(t *testing.T) {
	var nilQuota *Quota
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

func TestConsumeFixedFinalQuotaSettlementReturnsZeroAppliedOnError(t *testing.T) {
	logger.Logger = zap.NewNop()

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
	})

	quota := &Quota{
		modelName:      "gpt-5",
		userId:         1,
		tokenId:        1,
		channelId:      1,
		requestContext: context.Background(),
	}
	applied, err := quota.consumeFinalQuota(context.Background(), 100, nil, true, billing.SettlementRequestKindRealtimeTurn)
	if err == nil {
		t.Fatal("expected fixed final quota settlement to fail against an unmigrated database")
	}
	if applied != 0 {
		t.Fatalf("expected failed fixed settlement to report zero applied quota, got %d", applied)
	}
}

func TestSettlementRetriesOnlyDefinitelyNotAppliedOutcome(t *testing.T) {
	originalApply := applyBillingSettlement
	t.Cleanup(func() { applyBillingSettlement = originalApply })

	calls := 0
	applyBillingSettlement = func(context.Context, billing.SettlementCommand, *billing.SettlementOptions) (billing.SettlementResult, error) {
		calls++
		if calls == 1 {
			return billing.SettlementResult{BalanceOutcome: model.BillingBalanceDefinitelyRolledBack}, errors.New("transaction rolled back")
		}
		return billing.SettlementResult{TruthApplied: true, BalanceOutcome: model.BillingBalanceCommitted}, nil
	}
	quota := &Quota{userId: 1, tokenId: 1, price: model.Price{Type: model.TokensPriceType}, requestContext: context.Background(), startTime: time.Now()}
	if applied, err := quota.consumeFinalQuota(context.Background(), 7, &types.Usage{}, false, billing.SettlementRequestKindUnary); err != nil || applied != 7 || calls != 2 {
		t.Fatalf("definitely-not-applied settlement was not retried once: applied=%d err=%v calls=%d", applied, err, calls)
	}

	calls = 0
	applyBillingSettlement = func(context.Context, billing.SettlementCommand, *billing.SettlementOptions) (billing.SettlementResult, error) {
		calls++
		return billing.SettlementResult{BalanceOutcome: model.BillingBalanceCommitUnknown}, errors.New("commit acknowledgement lost")
	}
	if _, err := quota.consumeFinalQuota(context.Background(), 7, &types.Usage{}, false, billing.SettlementRequestKindUnary); err == nil || calls != 1 {
		t.Fatalf("commit-unknown settlement was replayed: err=%v calls=%d", err, calls)
	}
}

func TestConsumeUsageSettlementFailurePreservesPreConsumedTruth(t *testing.T) {
	logger.Logger = zap.NewNop()
	gin.SetMode(gin.TestMode)

	originalDB := model.DB
	originalPricing := model.PricingInstance
	originalBatchUpdate := config.BatchUpdateEnabled
	originalRedisEnabled := config.RedisEnabled
	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.User{}, &model.Token{}, &model.Log{}, &model.UserGroup{}); err != nil {
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
	setQuotaTestRoutingGroup(t, ctx, 1)

	attempt, err := NewAttemptQuota(ctx, "gpt-5", 100, BillingAttemptSpec{RequestKind: billing.SettlementRequestKindRealtimeTurn})
	if err != nil {
		t.Fatalf("create billing attempt: %v", err)
	}
	if err := attempt.ApplyReserve(ctx.Request.Context()); err != nil {
		t.Fatalf("expected reserve to succeed, got %v", err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatalf("claim submission: %v", err)
	}
	if !attempt.quota.PreconsumeTruthApplied {
		t.Fatal("expected reservation truth side effect before settlement")
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

	usage := &types.Usage{PromptTokens: 800, TotalTokens: 800}
	usage.MarkProviderReported()
	_, err = attempt.CloseFromProviderResult(ctx.Request.Context(), usage, false)
	if err == nil {
		t.Fatal("expected settlement to fail")
	}
	if !attempt.quota.PreconsumeTruthApplied {
		t.Fatal("expected failed settlement to preserve reservation truth")
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
