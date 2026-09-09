package relay_util

import (
	"net/http"
	"net/http/httptest"
	"one-api/common/utils"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/internal/billing"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

func useQuotaTestGroup(t *testing.T) string {
	t.Helper()
	const symbol = "quota-test"
	model.GlobalUserGroupRatio.Lock()
	original := model.GlobalUserGroupRatio.UserGroup
	groups := make(map[string]*model.UserGroup, len(original)+1)
	for key, value := range original {
		groups[key] = value
	}
	groups[symbol] = &model.UserGroup{Symbol: symbol, Ratio: 1}
	model.GlobalUserGroupRatio.UserGroup = groups
	model.GlobalUserGroupRatio.Unlock()
	t.Cleanup(func() {
		model.GlobalUserGroupRatio.Lock()
		model.GlobalUserGroupRatio.UserGroup = original
		model.GlobalUserGroupRatio.Unlock()
	})
	return symbol
}

func mustEvaluateProviderQuota(t *testing.T, quota *Quota, usage *types.Usage) int {
	t.Helper()
	usage.MarkProviderReported()
	for key, billing := range usage.ExtraBilling {
		usage.MarkProviderExtraBilling(key, billing)
	}
	decision := quota.EvaluateProviderUsage(usage)
	if !decision.Confirm {
		t.Fatalf("provider usage was not priceable: %+v", decision)
	}
	return int(decision.FinalQuota)
}

func TestQuotaSeedTimingFreezesManagedTurnLatency(t *testing.T) {
	startedAt := time.Unix(1700000000, 0)
	firstResponseAt := startedAt.Add(375 * time.Millisecond)
	completedAt := startedAt.Add(3250 * time.Millisecond)

	quota := &Quota{}
	quota.SeedTiming(startedAt, firstResponseAt, completedAt)

	if got := quota.getRequestTime(); got != int(completedAt.Sub(startedAt).Milliseconds()) {
		t.Fatalf("expected frozen request time %d, got %d", completedAt.Sub(startedAt).Milliseconds(), got)
	}
	if got := quota.GetFirstResponseTime(); got != firstResponseAt.Sub(startedAt).Milliseconds() {
		t.Fatalf("expected frozen first response %d, got %d", firstResponseAt.Sub(startedAt).Milliseconds(), got)
	}
}

func TestQuotaSeedTimingRejectsInvalidBounds(t *testing.T) {
	startedAt := time.Unix(1700000000, 0)
	firstResponseAt := startedAt.Add(-250 * time.Millisecond)
	completedAt := startedAt.Add(-time.Second)

	quota := &Quota{}
	quota.SeedTiming(startedAt, firstResponseAt, completedAt)

	if got := quota.GetFirstResponseTime(); got != 0 {
		t.Fatalf("expected invalid first response timing to be ignored, got %d", got)
	}
	if got := quota.getRequestTime(); got != 0 {
		t.Fatalf("expected invalid request duration to clamp to zero, got %d", got)
	}
}

func TestNewQuotaCapturesRequestStartAtAttemptCreation(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	startedAt := time.Now().Add(-1500 * time.Millisecond)
	ctx.Set("requestStartTime", startedAt)

	quota := NewQuota(ctx, "gpt-5", 0)
	if !quota.startTime.Equal(startedAt) {
		t.Fatalf("expected middleware request start %v, got %v", startedAt, quota.startTime)
	}
	if got := quota.getRequestTime(); got < 1400 {
		t.Fatalf("request timing started too late: got %dms", got)
	}
}

func newTieredQuotaForTest(t *testing.T) *Quota {
	t.Helper()
	extraRatios := datatypes.NewJSONType(map[string]float64{
		config.UsageExtraCache: 0.1,
	})
	rateRules := datatypes.NewJSONType(model.PriceRateRules{Version: 2, ServiceTier: []model.PriceRateRule{{ID: "flex", When: model.PriceRuleCondition{ServiceTier: []string{"flex"}}, Multipliers: model.PriceRateMultiplier{Input: utils.GetPointer(float64(0.5)), Output: utils.GetPointer(float64(0.5))}}, {ID: "priority", When: model.PriceRuleCondition{ServiceTier: []string{"fast", "priority"}}, Multipliers: model.PriceRateMultiplier{Input: utils.GetPointer(float64(2)), Output: utils.GetPointer(float64(2))}}}, LongContext: []model.PriceRateRule{{ID: "long_context", When: model.PriceRuleCondition{InputTokens: &model.PriceTokenRange{GT: utils.GetPointer(272000)}}, Multipliers: model.PriceRateMultiplier{Input: utils.GetPointer(float64(2)), Output: utils.GetPointer(float64(1.5))}}}})
	price := model.Price{
		Model:       "tiered-model",
		Type:        model.TokensPriceType,
		Input:       2.5,
		Output:      15,
		ExtraRatios: &extraRatios,
		RateRules:   &rateRules,
	}
	originalPricing := model.PricingInstance
	prices := map[string]*model.Price{price.Model: &price}
	var match []string
	if originalPricing != nil {
		for name, current := range originalPricing.GetAllPrices() {
			prices[name] = current
		}
		match = append(match, originalPricing.Match...)
	}
	model.PricingInstance = &model.Pricing{Prices: prices, Match: match}
	t.Cleanup(func() { model.PricingInstance = originalPricing })
	return &Quota{
		modelName:   price.Model,
		price:       price,
		groupName:   useQuotaTestGroup(t),
		groupRatio:  1,
		inputRatio:  price.Input,
		outputRatio: price.Output,
	}
}

func TestQuotaUsesConfiguredTierAndLongContextBoundary(t *testing.T) {
	quota := newTieredQuotaForTest(t)
	defaultAtBoundary := mustEvaluateProviderQuota(t, quota, &types.Usage{
		PromptTokens: 272000,
		ServiceTier:  "default",
	})
	if want := 272000 * 25 / 10; defaultAtBoundary != want {
		t.Fatalf("expected 272000 tokens to stay at standard input price %d, got %d", want, defaultAtBoundary)
	}

	longDefault := mustEvaluateProviderQuota(t, quota, &types.Usage{
		PromptTokens:     272001,
		CompletionTokens: 100,
		ServiceTier:      "default",
	})
	if want := (272001 * 5) + (100 * 225 / 10); longDefault != want {
		t.Fatalf("expected long-context whole-request multipliers to produce %d, got %d", want, longDefault)
	}

	flex := mustEvaluateProviderQuota(t, quota, &types.Usage{
		PromptTokens:     100,
		CompletionTokens: 10,
		ServiceTier:      "flex",
	})
	if want := 200; flex != want {
		t.Fatalf("expected flex to halve input and output rates to %d, got %d", want, flex)
	}

	for _, serviceTier := range []string{"fast", "priority"} {
		got := mustEvaluateProviderQuota(t, quota, &types.Usage{
			PromptTokens:     100,
			CompletionTokens: 10,
			ServiceTier:      serviceTier,
		})
		if want := 800; got != want {
			t.Fatalf("expected %s to double input and output rates to %d, got %d", serviceTier, want, got)
		}
	}
}

func TestTimesQuotaIgnoresTierAndLongContextRateRules(t *testing.T) {
	groupName := useQuotaTestGroup(t)
	originalPricing := model.PricingInstance
	fixedPrice := &model.Price{Model: "fixed-price-model", Type: model.TimesPriceType, Input: 2.5, Output: 15}
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"fixed-price-model": fixedPrice,
		"provider-token-model": {
			Model: "provider-token-model", Type: model.TokensPriceType, Input: 2.5, Output: 15,
		},
	}}
	t.Cleanup(func() { model.PricingInstance = originalPricing })

	tests := []struct {
		name          string
		serviceTier   string
		promptTokens  int
		responseModel string
	}{
		{name: "flex", serviceTier: "flex", promptTokens: 100},
		{name: "priority", serviceTier: "priority", promptTokens: 100},
		{name: "long context", serviceTier: "default", promptTokens: 272001},
		{name: "actual model remains fixed price", serviceTier: "priority", promptTokens: 272001, responseModel: "provider-token-model"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			price := model.Price{
				Model:  "fixed-price-model",
				Type:   model.TimesPriceType,
				Input:  2.5,
				Output: 15,
			}
			quota := &Quota{
				modelName:   price.Model,
				price:       price,
				groupName:   groupName,
				groupRatio:  1,
				inputRatio:  price.Input,
				outputRatio: price.Output,
			}

			got := mustEvaluateProviderQuota(t, quota, &types.Usage{
				PromptTokens:  test.promptTokens,
				ServiceTier:   test.serviceTier,
				ResponseModel: test.responseModel,
			})
			if want := 2500; got != want {
				t.Fatalf("expected fixed times price to remain %d quota, got %d", want, got)
			}
		})
	}
}

func TestQuotaMissingAutoAndUnknownTierNeverInventAMultiplier(t *testing.T) {
	for _, serviceTier := range []string{"", "auto", "provider_future_tier"} {
		t.Run(serviceTier, func(t *testing.T) {
			quota := newTieredQuotaForTest(t)
			got := mustEvaluateProviderQuota(t, quota, &types.Usage{
				PromptTokens:     100,
				CompletionTokens: 10,
				ServiceTier:      serviceTier,
			})
			if want := 400; got != want {
				t.Fatalf("expected tier %q to use the configured base rate %d, got %d", serviceTier, want, got)
			}
			if serviceTier == "provider_future_tier" && !quota.billingDiagnostics["billing_tier_unknown"] {
				t.Fatalf("expected unknown tier diagnostic, got %+v", quota.billingDiagnostics)
			}
		})
	}
}

func TestQuotaActualModelCannotChangeBillingUnit(t *testing.T) {
	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"provider-times-model": {Model: "provider-times-model", Type: model.TimesPriceType, Input: 9},
	}}
	t.Cleanup(func() { model.PricingInstance = originalPricing })

	quota := newTieredQuotaForTest(t)
	got := mustEvaluateProviderQuota(t, quota, &types.Usage{
		PromptTokens: 100, ResponseModel: "provider-times-model", ServiceTier: "default",
	})
	if want := 250; got != want {
		t.Fatalf("expected request token pricing to remain authoritative, want %d got %d", want, got)
	}
	if !quota.billingDiagnostics["billing_model_price_type_mismatch"] {
		t.Fatalf("expected price-type mismatch diagnostic, got %+v", quota.billingDiagnostics)
	}
}

func TestQuotaUsesRegisteredActualModelAndCurrentRequestFallback(t *testing.T) {
	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"provider-cheap-model": {
			Model: "provider-cheap-model", Type: model.TokensPriceType, Input: 1, Output: 6,
		},
	}}
	t.Cleanup(func() { model.PricingInstance = originalPricing })

	quota := newTieredQuotaForTest(t)
	actualTerra := mustEvaluateProviderQuota(t, quota, &types.Usage{
		PromptTokens: 100, ResponseModel: "provider-cheap-model", ServiceTier: "default",
	})
	if actualTerra != 100 {
		t.Fatalf("expected registered actual Terra price, got %d", actualTerra)
	}

	unregisteredAlias := mustEvaluateProviderQuota(t, quota, &types.Usage{
		PromptTokens: 100, ResponseModel: "unregistered-provider-version", ServiceTier: "default",
	})
	if unregisteredAlias != 250 {
		t.Fatalf("expected unregistered provider alias to use current request price, got %d", unregisteredAlias)
	}
}

func TestQuotaOriginalModelBillingUsesCurrentCompleteRequestPricePolicy(t *testing.T) {
	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"provider-cheap-model": {
			Model: "provider-cheap-model", Type: model.TokensPriceType, Input: 1, Output: 6,
		},
	}}
	t.Cleanup(func() { model.PricingInstance = originalPricing })

	quota := newTieredQuotaForTest(t)
	quota.freezeRequestPricePolicy = true
	got := mustEvaluateProviderQuota(t, quota, &types.Usage{
		PromptTokens: 100,
		PromptTokensDetails: types.PromptTokensDetails{
			CachedTokens: 100,
		},
		ResponseModel: "provider-cheap-model",
		ServiceTier:   "flex",
	})
	if got != 13 {
		t.Fatalf("expected original model base, cache, and flex policy to remain authoritative, got %d", got)
	}
	if quota.settlementModel != "tiered-model" || quota.settlementPrice == nil || quota.settlementPrice.Model != "tiered-model" {
		t.Fatalf("expected settlement metadata to retain original model policy, model=%q price=%+v", quota.settlementModel, quota.settlementPrice)
	}
}

func TestQuotaUsesConfiguredWildcardCacheRatio(t *testing.T) {
	originalPricing := model.PricingInstance
	extraRatios := datatypes.NewJSONType(map[string]float64{config.UsageExtraCache: 0.1})
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"provider-model*": {
				Model: "provider-model*", Type: model.TokensPriceType, Input: 1, Output: 6, ExtraRatios: &extraRatios,
			},
		},
		Match: []string{"provider-model*"},
	}
	t.Cleanup(func() { model.PricingInstance = originalPricing })

	quota := newTieredQuotaForTest(t)
	got := mustEvaluateProviderQuota(t, quota, &types.Usage{
		PromptTokens: 100,
		PromptTokensDetails: types.PromptTokensDetails{
			CachedTokens: 100,
		},
		ResponseModel: "provider-model-v2",
		ServiceTier:   "default",
	})
	if got != 10 {
		t.Fatalf("expected wildcard price to bill cached input at 0.1x, got %d", got)
	}
}

func TestQuotaAppliesConfiguredCacheReadAndWriteOnce(t *testing.T) {
	quota := newTieredQuotaForTest(t)
	cacheRead := mustEvaluateProviderQuota(t, quota, &types.Usage{
		PromptTokens: 100,
		PromptTokensDetails: types.PromptTokensDetails{
			CachedTokens: 100,
		},
		ServiceTier: "default",
	})
	if cacheRead != 25 {
		t.Fatalf("expected cached input at 0.1x to cost 25 quota, got %d", cacheRead)
	}

	cacheWrite := mustEvaluateProviderQuota(t, quota, &types.Usage{
		PromptTokens: 100,
		PromptTokensDetails: types.PromptTokensDetails{
			CacheWriteTokens: 100,
		},
		ServiceTier: "default",
	})
	if cacheWrite != 313 {
		t.Fatalf("expected cache write at 1.25x to round to 313 quota, got %d", cacheWrite)
	}
}

func TestQuotaSettlementMetadataProtocolDefaultsAndOverrides(t *testing.T) {
	quota := &Quota{}

	httpEnvelope := quota.buildSettlementEnvelope(0, &types.Usage{}, false, billing.SettlementRequestKindUnary)
	if got := httpEnvelope.Options.Projection.Metadata["protocol"]; got != LogProtocolHTTP {
		t.Fatalf("expected non-stream settlement protocol %q, got %#v", LogProtocolHTTP, got)
	}

	streamEnvelope := quota.buildSettlementEnvelope(0, &types.Usage{}, true, billing.SettlementRequestKindUnary)
	if got := streamEnvelope.Options.Projection.Metadata["protocol"]; got != LogProtocolHTTPStream {
		t.Fatalf("expected stream settlement protocol %q, got %#v", LogProtocolHTTPStream, got)
	}

	quota.SetLogProtocol(LogProtocolRealtimeWS)
	realtimeEnvelope := quota.buildSettlementEnvelope(0, &types.Usage{}, true, billing.SettlementRequestKindRealtimeTurn)
	if got := realtimeEnvelope.Options.Projection.Metadata["protocol"]; got != LogProtocolRealtimeWS {
		t.Fatalf("expected explicit realtime settlement protocol %q, got %#v", LogProtocolRealtimeWS, got)
	}
	if !realtimeEnvelope.Options.Projection.IsStream {
		t.Fatal("expected realtime settlement projection to preserve is_stream=true")
	}
}

func TestQuotaEvaluateProviderUsageSeparatesImageGenerationVariantPricing(t *testing.T) {
	quota := &Quota{modelName: "gpt-image-1"}
	useCurrentQuotaPrice(t, quota, model.Price{Type: model.TokensPriceType, Input: 1, Output: 1})
	lowKey := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "low-1024x1024")
	highKey := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "high-1536x1024")
	usage := &types.Usage{ExtraBilling: map[string]types.ExtraBilling{
		lowKey: {
			ServiceType: types.APIToolTypeImageGeneration,
			Type:        "low-1024x1024",
			CallCount:   1,
		},
		highKey: {
			ServiceType: types.APIToolTypeImageGeneration,
			Type:        "high-1536x1024",
			CallCount:   1,
		},
	}}
	if got := mustEvaluateProviderQuota(t, quota, usage); got <= 0 {
		t.Fatalf("expected provider image units to be charged, got %d", got)
	}

	if len(quota.extraBillingData) != 2 {
		t.Fatalf("expected distinct image generation variants to produce separate priced entries, got %+v", quota.extraBillingData)
	}
	if got := quota.extraBillingData[lowKey]; got.ServiceType != types.APIToolTypeImageGeneration || got.Price != 0.011 {
		t.Fatalf("expected low variant to keep its own price, got %+v", got)
	}
	if got := quota.extraBillingData[highKey]; got.ServiceType != types.APIToolTypeImageGeneration || got.Price != 0.25 {
		t.Fatalf("expected high variant to keep its own price, got %+v", got)
	}
}

func TestQuotaRecordsConservativeImagePriceDiagnostic(t *testing.T) {
	quota := &Quota{modelName: "future-model"}
	useCurrentQuotaPrice(t, quota, model.Price{Type: model.TokensPriceType, Input: 1, Output: 1})
	variant := "future-image|high|1024x1024|0"
	key := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, variant)
	usage := &types.Usage{ExtraBilling: map[string]types.ExtraBilling{
		key: {
			ServiceType: types.APIToolTypeImageGeneration,
			Type:        variant,
			CallCount:   1,
		},
	}}
	if got := mustEvaluateProviderQuota(t, quota, usage); got <= 0 {
		t.Fatalf("expected conservative provider image unit charge, got %d", got)
	}

	if data := quota.extraBillingData[key]; data.Price <= 0 {
		t.Fatalf("expected conservative image price to remain nonzero, got %+v", data)
	}
	if !quota.billingDiagnostics[imageGenerationConservativePriceDiagnostic] {
		t.Fatalf("expected conservative image price diagnostic, got %+v", quota.billingDiagnostics)
	}
}

func TestImageGenerationUnknownEvidenceUsesConservativeNonZeroPrice(t *testing.T) {
	for _, variant := range []string{"", "auto-auto", "future-shape", "high-future"} {
		if got := getDefaultExtraServicePrice(types.APIToolTypeImageGeneration, "gpt-5.6", variant); got != 0.71157 {
			t.Fatalf("expected variant %q to use highest compatible catalogue price 0.71157, got %v", variant, got)
		}
	}
}

func testRuleMultiplier(rules model.PriceRateRules, id string) *model.PriceRateMultiplier {
	for _, rule := range rules.ServiceTier {
		if rule.ID == id {
			return &rule.Multipliers
		}
	}
	return nil
}
