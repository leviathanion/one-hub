package relay_util

import (
	"testing"

	"one-api/common/config"
	"one-api/model"
	"one-api/types"

	"gorm.io/datatypes"
)

func useComponentTestGroup(t *testing.T) string {
	t.Helper()
	const symbol = "component-test"
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

func componentTestQuota(t *testing.T) *Quota {
	t.Helper()
	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"gpt-5": {Model: "gpt-5", Type: model.TokensPriceType, Input: 1, Output: 1},
	}}
	t.Cleanup(func() { model.PricingInstance = originalPricing })
	return &Quota{
		modelName:   "gpt-5",
		groupName:   useComponentTestGroup(t),
		price:       model.Price{Model: "gpt-5", Type: model.TokensPriceType, Input: 1, Output: 1},
		groupRatio:  1,
		inputRatio:  1,
		outputRatio: 1,
	}
}

func TestUsageComponentsKeepIndependentProviderUnitWhenTokensAreMissing(t *testing.T) {
	quota := componentTestQuota(t)
	usage := &types.Usage{ExtraBilling: map[string]types.ExtraBilling{
		types.APIToolTypeWebSearch: {ServiceType: types.APIToolTypeWebSearch, Type: "medium", CallCount: 1},
	}}
	usage.MarkProviderExtraBilling(types.APIToolTypeWebSearch, usage.ExtraBilling[types.APIToolTypeWebSearch])
	decision := quota.EvaluateProviderUsage(usage)
	want := int64(defaultExtraServicePrices.WebSearchGA * float64(config.QuotaPerUnit))
	if !decision.Confirm || decision.FinalQuota != want {
		t.Fatalf("independent search component was erased by missing tokens: %+v want=%d", decision, want)
	}
	if decision.Components[0].Status != PriceComponentMissingEvidence || decision.Components[1].Status != PriceComponentPriceable {
		t.Fatalf("unexpected component reduction: %+v", decision.Components)
	}
}

func TestUsageComponentsDoNotPartiallyChargeIncompleteTokenPartition(t *testing.T) {
	quota := componentTestQuota(t)
	usage := &types.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110}
	usage.MarkProviderReported()
	usage.RequireTokenExtraEvidence(config.UsageExtraCache, config.UsageExtraCacheWrite)
	decision := quota.EvaluateProviderUsage(usage)
	if decision.Confirm || decision.FinalQuota != 0 || decision.Components[0].Status != PriceComponentMissingEvidence {
		t.Fatalf("incomplete cache partition was partially charged: %+v", decision)
	}
}

func TestUsageComponentsConfirmLegitimateZeroTokenEvidence(t *testing.T) {
	quota := componentTestQuota(t)
	usage := &types.Usage{}
	usage.MarkProviderReported()
	decision := quota.EvaluateProviderUsage(usage)
	if !decision.Confirm || decision.FinalQuota != 0 || decision.Components[0].Status != PriceComponentPriceable {
		t.Fatalf("legitimate zero token evidence became Cancel: %+v", decision)
	}
}

func TestUsageComponentsPriceProviderOperationCountForTimesPrice(t *testing.T) {
	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"imagen": {Model: "imagen", Type: model.TimesPriceType, Input: 2},
	}}
	t.Cleanup(func() { model.PricingInstance = originalPricing })
	quota := &Quota{
		modelName:  "imagen",
		groupName:  useComponentTestGroup(t),
		price:      model.Price{Model: "imagen", Type: model.TimesPriceType, Input: 2},
		groupRatio: 1,
		inputRatio: 2,
	}
	usage := &types.Usage{}
	usage.MarkProviderOperationUnits(3)
	decision := quota.EvaluateProviderUsage(usage)
	if !decision.Confirm || decision.FinalQuota != 6000 {
		t.Fatalf("provider operation count was not priced atomically: %+v", decision)
	}
}

func TestUsageComponentsRejectNegativeProviderOperationCount(t *testing.T) {
	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"imagen": {Model: "imagen", Type: model.TimesPriceType, Input: 2},
	}}
	t.Cleanup(func() { model.PricingInstance = originalPricing })
	quota := &Quota{modelName: "imagen", groupName: useComponentTestGroup(t), groupRatio: 1}
	negative := -1
	decision := quota.EvaluateProviderUsage(&types.Usage{ProviderOperationUnits: &negative})
	if decision.Confirm || len(decision.Components) == 0 || decision.Components[0].Status != PriceComponentConflictingEvidence {
		t.Fatalf("negative provider operation count was authorized: %+v", decision)
	}
}

func TestUsageComponentsCancelUnpricedIndependentTranscription(t *testing.T) {
	quota := componentTestQuota(t)
	usage := &types.Usage{}
	usage.SetProviderIndependentUsageUnit(config.UsageExtraInputAudioTranscription, 2.5)
	decision := quota.EvaluateProviderUsage(usage)
	if decision.Confirm || decision.FinalQuota != 0 {
		t.Fatalf("unpriced independent transcription authorized settlement: %+v", decision)
	}
}

func TestUsageComponentsRejectAttributionOnlyAndUntrustedRequestUnits(t *testing.T) {
	quota := componentTestQuota(t)
	usage := &types.Usage{
		ResponseModel: "gpt-5-provider",
		ExtraBilling: map[string]types.ExtraBilling{
			types.APIToolTypeWebSearch: {ServiceType: types.APIToolTypeWebSearch, CallCount: 1},
		},
	}
	decision := quota.EvaluateProviderUsage(usage)
	if decision.Confirm || decision.FinalQuota != 0 {
		t.Fatalf("attribution/request inference authorized settlement: %+v", decision)
	}
}

func TestUsageComponentsUseCurrentPriceAtSettlement(t *testing.T) {
	originalPricing := model.PricingInstance
	t.Cleanup(func() { model.PricingInstance = originalPricing })
	pricing := &model.Pricing{Prices: map[string]*model.Price{
		"request-model": {Model: "request-model", Type: model.TokensPriceType, Input: 1, Output: 1},
		"actual-model":  {Model: "actual-model", Type: model.TokensPriceType, Input: 2, Output: 3},
	}}
	model.PricingInstance = pricing
	quota := &Quota{
		modelName: "request-model", price: model.Price{Model: "request-model", Type: model.TokensPriceType, Input: 1, Output: 1},
		groupName:  useComponentTestGroup(t),
		groupRatio: 1, inputRatio: 1, outputRatio: 1,
	}
	pricing.Prices["actual-model"].Input = 99
	usage := &types.Usage{PromptTokens: 10, TotalTokens: 10, ResponseModel: "actual-model"}
	usage.MarkProviderReported()
	decision := quota.EvaluateProviderUsage(usage)
	if !decision.Confirm || decision.FinalQuota != 990 {
		t.Fatalf("settlement did not use the current price: %+v", decision)
	}
}

func TestUsageComponentsDoNotUseConflictingAttributionForDependentPrices(t *testing.T) {
	originalPricing := model.PricingInstance
	extraRatios := datatypes.NewJSONType(map[string]float64{config.UsageExtraInputAudioTranscription: 4})
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"request-model": {Model: "request-model", Type: model.TokensPriceType, Input: 1, Output: 1, ExtraRatios: &extraRatios},
		"gpt-4o":        {Model: "gpt-4o", Type: model.TokensPriceType, Input: 99, Output: 99, ExtraRatios: &extraRatios},
	}}
	t.Cleanup(func() { model.PricingInstance = originalPricing })
	quota := &Quota{modelName: "request-model", groupName: useComponentTestGroup(t), groupRatio: 1}

	previewKey := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	gaKey := types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "medium")
	imageKey := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-1-mini|low|1024x1024|0")
	incompleteImageKey := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "low-1024x1024")
	usage := &types.Usage{
		PromptTokens:        10,
		TotalTokens:         10,
		ResponseModel:       "gpt-4o",
		AttributionConflict: true,
		ExtraBilling: map[string]types.ExtraBilling{
			previewKey:         {ServiceType: types.APIToolTypeWebSearchPreview, Type: "medium", CallCount: 1},
			gaKey:              {ServiceType: types.APIToolTypeWebSearch, Type: "medium", CallCount: 1},
			imageKey:           {ServiceType: types.APIToolTypeImageGeneration, Type: "gpt-image-1-mini|low|1024x1024|0", CallCount: 1},
			incompleteImageKey: {ServiceType: types.APIToolTypeImageGeneration, Type: "low-1024x1024", CallCount: 1},
		},
	}
	usage.MarkProviderReported()
	usage.SetProviderIndependentUsageUnit(config.UsageExtraInputAudioTranscription, 2)
	for key, billing := range usage.ExtraBilling {
		usage.MarkProviderExtraBilling(key, billing)
	}

	decision := quota.EvaluateProviderUsage(usage)
	want := int64(saturatingCeilToInt(defaultExtraServicePrices.WebSearchGA*quota.effectiveQuotaPerUnit()) +
		saturatingCeilToInt(0.005*quota.effectiveQuotaPerUnit()) +
		saturatingCeilToInt(getDefaultExtraServicePrice(types.APIToolTypeImageGeneration, "", "low-1024x1024")*quota.effectiveQuotaPerUnit()))
	if !decision.Confirm || decision.FinalQuota != want {
		t.Fatalf("conflicting attribution charged a dependent component: decision=%+v want=%d", decision, want)
	}
	statuses := componentStatuses(decision.Components)
	for _, name := range []string{"tokens", "independent_unit:" + config.UsageExtraInputAudioTranscription, "unit:" + previewKey} {
		if statuses[name] != PriceComponentConflictingEvidence {
			t.Fatalf("dependent component %q was not conflicting: %+v", name, decision.Components)
		}
	}
	for _, name := range []string{"unit:" + gaKey, "unit:" + imageKey, "unit:" + incompleteImageKey} {
		if statuses[name] != PriceComponentPriceable {
			t.Fatalf("independent component %q was not retained: %+v", name, decision.Components)
		}
	}
	if quota.settlementModel != "request-model" || quota.settlementPrice == nil || quota.settlementPrice.Input != 1 {
		t.Fatalf("conflicting actual model reached price policy: model=%q price=%+v", quota.settlementModel, quota.settlementPrice)
	}
	if !quota.billingDiagnostics[imageGenerationConservativePriceDiagnostic] {
		t.Fatalf("conservative image pricing diagnostic was lost: %+v", quota.billingDiagnostics)
	}
}

func TestUsageComponentsRejectConflictingActualModelForOperationUnits(t *testing.T) {
	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"request-model": {Model: "request-model", Type: model.TokensPriceType, Input: 1, Output: 1},
		"actual-model":  {Model: "actual-model", Type: model.TimesPriceType, Input: 9},
	}}
	t.Cleanup(func() { model.PricingInstance = originalPricing })
	quota := &Quota{modelName: "request-model", groupName: useComponentTestGroup(t), groupRatio: 1}
	usage := &types.Usage{
		ResponseModel:       "actual-model",
		AttributionConflict: true,
		BillingDiagnostics:  map[string]bool{"billing_model_conflict": true},
	}
	usage.MarkProviderOperationUnits(3)
	usage.SetProviderExtraBilling(types.APIToolTypeWebSearch, "", 1)

	decision := quota.EvaluateProviderUsage(usage)
	want := int64(saturatingCeilToInt(defaultExtraServicePrices.WebSearchGA * quota.effectiveQuotaPerUnit()))
	if !decision.Confirm || decision.FinalQuota != want {
		t.Fatalf("conflicting operation model was charged: decision=%+v want=%d", decision, want)
	}
	if got := componentStatuses(decision.Components)["operation_units"]; got != PriceComponentConflictingEvidence {
		t.Fatalf("operation component status=%q, components=%+v", got, decision.Components)
	}
}

func TestUsageComponentsOriginalModelPolicyStillRejectsAttributionConflict(t *testing.T) {
	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"request-model": {Model: "request-model", Type: model.TokensPriceType, Input: 2, Output: 3},
		"actual-model":  {Model: "actual-model", Type: model.TokensPriceType, Input: 99, Output: 99},
	}}
	t.Cleanup(func() { model.PricingInstance = originalPricing })
	quota := &Quota{
		modelName: "request-model", groupName: useComponentTestGroup(t), groupRatio: 1,
		freezeRequestPricePolicy: true,
	}
	usage := &types.Usage{
		PromptTokens:        10,
		CompletionTokens:    2,
		TotalTokens:         12,
		ResponseModel:       "actual-model",
		AttributionConflict: true,
		BillingDiagnostics:  map[string]bool{"billing_model_conflict": true},
	}
	usage.MarkProviderReported()

	decision := quota.EvaluateProviderUsage(usage)
	if decision.Confirm || decision.FinalQuota != 0 {
		t.Fatalf("original-model policy authorized conflicting attribution: %+v", decision)
	}
	if got := componentStatuses(decision.Components)["tokens"]; got != PriceComponentConflictingEvidence {
		t.Fatalf("original-model token component status=%q, components=%+v", got, decision.Components)
	}
}

func TestUsageComponentsConflictDiagnosticsDoNotAuthorizeDependentComponents(t *testing.T) {
	originalPricing := model.PricingInstance
	extraRatios := datatypes.NewJSONType(map[string]float64{config.UsageExtraInputAudioTranscription: 4})
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"request-model": {Model: "request-model", Type: model.TokensPriceType, Input: 1, Output: 1},
		"gpt-4o":        {Model: "gpt-4o", Type: model.TokensPriceType, Input: 2, Output: 3, ExtraRatios: &extraRatios},
	}}
	t.Cleanup(func() { model.PricingInstance = originalPricing })
	quota := &Quota{modelName: "request-model", groupName: useComponentTestGroup(t), groupRatio: 1}
	previewKey := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	usage := &types.Usage{
		PromptTokens:        10,
		TotalTokens:         10,
		ResponseModel:       "gpt-4o",
		ServiceTier:         "flex",
		AttributionConflict: true,
		BillingDiagnostics:  map[string]bool{"billing_tier_conflict": true},
	}
	usage.MarkProviderReported()
	usage.SetProviderIndependentUsageUnit(config.UsageExtraInputAudioTranscription, 2)
	usage.SetProviderExtraBilling(types.APIToolTypeWebSearchPreview, "medium", 1)

	decision := quota.EvaluateProviderUsage(usage)
	if decision.Confirm || decision.FinalQuota != 0 {
		t.Fatalf("conflict diagnostic narrowed the fail-safe boundary: %+v", decision)
	}
	statuses := componentStatuses(decision.Components)
	if statuses["tokens"] != PriceComponentConflictingEvidence ||
		statuses["independent_unit:"+config.UsageExtraInputAudioTranscription] != PriceComponentConflictingEvidence ||
		statuses["unit:"+previewKey] != PriceComponentConflictingEvidence {
		t.Fatalf("component dependencies were not isolated: %+v", decision.Components)
	}
	if quota.settlementTier != "default" {
		t.Fatalf("conflicting tier reached price policy: %q", quota.settlementTier)
	}
}

func componentStatuses(components []PriceComponentDecision) map[string]PriceComponentStatus {
	statuses := make(map[string]PriceComponentStatus, len(components))
	for _, component := range components {
		statuses[component.Name] = component.Status
	}
	return statuses
}

func TestPolicyDiagnosticsDescribeTheLatestSettlementRead(t *testing.T) {
	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{}}
	t.Cleanup(func() { model.PricingInstance = originalPricing })
	quota := &Quota{modelName: "restored-model", groupName: useComponentTestGroup(t), groupRatio: 1}
	usage := &types.Usage{PromptTokens: 1, TotalTokens: 1}
	usage.MarkProviderReported()
	if first := quota.EvaluateProviderUsage(usage); first.Confirm || !quota.billingDiagnostics["price_policy_missing_at_settlement"] {
		t.Fatalf("missing policy was not diagnosed: decision=%+v diagnostics=%+v", first, quota.billingDiagnostics)
	}
	model.PricingInstance.Prices["restored-model"] = &model.Price{Model: "restored-model", Type: model.TokensPriceType, Input: 1, Output: 1}
	second := quota.EvaluateProviderUsage(usage)
	if !second.Confirm || quota.billingDiagnostics["price_policy_missing_at_settlement"] {
		t.Fatalf("restored policy kept stale diagnostic: decision=%+v diagnostics=%+v", second, quota.billingDiagnostics)
	}
}
