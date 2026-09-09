package relay_util

import (
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"math"
	"one-api/common/config"
	"one-api/model"
	"one-api/types"
	"testing"
	"time"
)

func TestRateRuleOverflowDoesNotDropCachePriceOrCreditBalance(t *testing.T) {
	q := newTieredQuotaForTest(t)
	price := model.PricingInstance.Prices[q.modelName]
	extra := datatypes.NewJSONType(map[string]float64{config.UsageExtraCache: 1e308})
	price.ExtraRatios = &extra
	usage := &types.Usage{PromptTokens: 100, CompletionTokens: 10, ServiceTier: "fast", PromptTokensDetails: types.PromptTokensDetails{CachedTokens: 80}}
	usage.MarkProviderReported()
	got := q.EvaluateProviderUsage(usage)
	assert.True(t, got.Confirm)
	assert.Equal(t, int64(math.MaxInt), got.FinalQuota)
	_, err := json.Marshal(q.GetLogMeta(usage))
	require.NoError(t, err)
	// 无法成立的输入分区不能产生负费用，再抵扣输出费用。
	extra = datatypes.NewJSONType(map[string]float64{config.UsageExtraCache: 0})
	price.ExtraRatios = &extra
	usage = &types.Usage{PromptTokens: 100, CompletionTokens: 10, ServiceTier: "fast", PromptTokensDetails: types.PromptTokensDetails{CachedTokens: 200}}
	usage.MarkProviderReported()
	got = q.EvaluateProviderUsage(usage)
	assert.False(t, got.Confirm)
	assert.Zero(t, got.FinalQuota)
	assert.Equal(t, PriceComponentConflictingEvidence, got.Components[0].Status)
}

func TestRateRuleCacheOverrideCanChargeWhenInputIsFree(t *testing.T) {
	for _, tc := range []struct {
		rules string
		want  int64
	}{
		{`{"version":2,"schedule":{"rules":[{"id":"free","when":{},"multipliers":{"all":0,"extra_multipliers":{"cached_tokens":1}}}]}}`, 20},
		{`{"version":2,"schedule":{"rules":[{"id":"half","when":{},"multipliers":{"all":0.5}}]}}`, 110},
	} {
		t.Run(tc.rules, func(t *testing.T) {
			q := newTieredQuotaForTest(t)
			var rules model.PriceRateRules
			require.NoError(t, json.Unmarshal([]byte(tc.rules), &rules))
			encoded := datatypes.NewJSONType(rules)
			model.PricingInstance.Prices[q.modelName].RateRules = &encoded
			usage := &types.Usage{PromptTokens: 100, CompletionTokens: 10, PromptTokensDetails: types.PromptTokensDetails{CachedTokens: 80}}
			usage.MarkProviderReported()
			decision := q.EvaluateProviderUsage(usage)
			assert.True(t, decision.Confirm)
			assert.Equal(t, tc.want, decision.FinalQuota)
			meta := q.GetLogMeta(usage)
			extras := meta["effective_extra_ratios"].(map[string]float64)
			assert.InDelta(t, 0.25*q.settlementRuleResult.Extra[config.UsageExtraCache], extras[config.UsageExtraCache], 1e-12)
			preview, _, err := PreviewTokenPrice(*model.PricingInstance.Prices[q.modelName], model.PriceRuleFacts{}, usage, 1)
			require.NoError(t, err)
			assert.Equal(t, decision.FinalQuota, preview.FinalQuota)
		})
	}
}

func TestRateRuleUnknownSpeedDoesNotEraseIndependentTool(t *testing.T) {
	q := newTieredQuotaForTest(t)
	var rules model.PriceRateRules
	require.NoError(t, json.Unmarshal([]byte(`{"version":2,"speed":[{"id":"fast","when":{"speed":["fast"]},"multipliers":{"all":2}}]}`), &rules))
	encoded := datatypes.NewJSONType(rules)
	model.PricingInstance.Prices[q.modelName].RateRules = &encoded
	usage := &types.Usage{PromptTokens: 100, CompletionTokens: 10}
	usage.MarkProviderReported()
	usage.SetProviderExtraBilling(types.APIToolTypeWebSearch, "", 1)
	got := q.EvaluateProviderUsage(usage)
	require.True(t, got.Confirm)
	assert.Equal(t, PriceComponentMissingEvidence, got.Components[0].Status)
	assert.Greater(t, got.FinalQuota, int64(0))
	assert.Empty(t, q.settlementTokenBilling.Rules)
	usage.Speed = "standard"
	got = q.EvaluateProviderUsage(usage)
	assert.Equal(t, PriceComponentPriceable, got.Components[0].Status)
}

func TestRateRuleCacheEvidenceUsesEffectiveRatherThanBaseRatio(t *testing.T) {
	q := newTieredQuotaForTest(t)
	price := model.PricingInstance.Prices[q.modelName]
	extra := datatypes.NewJSONType(map[string]float64{config.UsageExtraCache: 1})
	price.ExtraRatios = &extra
	var rules model.PriceRateRules
	require.NoError(t, json.Unmarshal([]byte(`{"version":2,"schedule":{"rules":[{"id":"discount","when":{},"multipliers":{"all":0,"extra_multipliers":{"cached_tokens":1}}}]}}`), &rules))
	encoded := datatypes.NewJSONType(rules)
	price.RateRules = &encoded
	usage := &types.Usage{PromptTokens: 100, CompletionTokens: 10}
	usage.MarkProviderReported()
	usage.RequireTokenExtraEvidence(config.UsageExtraCache)
	assert.False(t, q.EvaluateProviderUsage(usage).Confirm)
	usage.PromptTokensDetails.CachedTokens = 80
	usage.ProviderTokenFields[config.UsageExtraCache] = true
	got := q.EvaluateProviderUsage(usage)
	assert.True(t, got.Confirm)
	assert.Equal(t, int64(200), got.FinalQuota)
}

func TestRateRuleScheduleUsesEachAttemptStart(t *testing.T) {
	q := newTieredQuotaForTest(t)
	var rules model.PriceRateRules
	require.NoError(t, json.Unmarshal([]byte(`{"version":2,"schedule":{"rules":[{"id":"night","when":{"time":{"start":"23:00","end":"07:00"}},"multipliers":{"all":0.5}}],"timezone":"Asia/Shanghai"}}`), &rules))
	encoded := datatypes.NewJSONType(rules)
	model.PricingInstance.Prices[q.modelName].RateRules = &encoded
	usage := &types.Usage{PromptTokens: 100, CompletionTokens: 10}
	usage.MarkProviderReported()
	start, err := time.Parse(time.RFC3339, "2026-09-12T06:59:00+08:00")
	require.NoError(t, err)
	q.SeedTiming(start, time.Time{}, start.Add(2*time.Minute))
	assert.Equal(t, int64(200), q.EvaluateProviderUsage(usage).FinalQuota)
	q.SeedTiming(start.Add(time.Minute), time.Time{}, start.Add(3*time.Minute))
	assert.Equal(t, int64(400), q.EvaluateProviderUsage(usage).FinalQuota)
}
