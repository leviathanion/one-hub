package relay_util

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/logger"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/datatypes"
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
	if quota.userId != 11 || quota.channelId != 22 || quota.tokenId != 33 || !quota.unlimitedQuota {
		t.Fatalf("expected quota identity fields to be copied from gin context, got %+v", quota)
	}
	if !quota.isBackupGroup || quota.tokenName != "token-alpha" || quota.groupName != "team-b" || quota.tokenGroupName != "team-a" || quota.backupGroupName != "team-b" {
		t.Fatalf("expected quota group metadata to be copied, got %+v", quota)
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
		ExtraTokens: map[string]int{
			config.UsageExtraInputAudio: 4,
			config.UsageExtraReasoning:  2,
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
