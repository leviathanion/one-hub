package relay_util

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"one-api/internal/billing"
	"one-api/model"
	"one-api/types"
)

func TestAttemptQuotaRepeatedCloseDoesNotReevaluateUsageOrPrice(t *testing.T) {
	originalPricing := model.PricingInstance
	pricing := &model.Pricing{Prices: map[string]*model.Price{
		"request-model": {Model: "request-model", Type: model.TokensPriceType, Input: 1, Output: 1},
		"actual-model":  {Model: "actual-model", Type: model.TokensPriceType, Input: 2, Output: 3},
	}}
	model.PricingInstance = pricing
	t.Cleanup(func() { model.PricingInstance = originalPricing })

	originalApply := applyBillingSettlement
	var applyCalls atomic.Int32
	applyBillingSettlement = func(context.Context, billing.SettlementCommand, *billing.SettlementOptions) (billing.SettlementResult, error) {
		applyCalls.Add(1)
		return billing.SettlementResult{TruthApplied: true, BalanceOutcome: model.BillingBalanceCommitted}, nil
	}
	t.Cleanup(func() { applyBillingSettlement = originalApply })

	quota := &Quota{
		modelName:  "request-model",
		groupName:  useComponentTestGroup(t),
		groupRatio: 1,
	}
	attempt := &AttemptQuota{quota: quota}
	firstUsage := &types.Usage{PromptTokens: 10, TotalTokens: 10, ResponseModel: "actual-model"}
	firstUsage.MarkProviderReported()
	first, err := attempt.CloseFromProviderResult(context.Background(), firstUsage, false)
	if err != nil || !first.Confirmed || first.ChargedQuota != 20 {
		t.Fatalf("first close result=%+v err=%v", first, err)
	}
	firstModel := quota.settlementModel
	firstPrice := cloneQuotaPrice(*quota.settlementPrice)
	firstDiagnostics := make(map[string]bool, len(quota.billingDiagnostics))
	for key, value := range quota.billingDiagnostics {
		firstDiagnostics[key] = value
	}

	pricing.Prices["actual-model"].Input = 99
	secondUsage := &types.Usage{
		PromptTokens:        100,
		TotalTokens:         100,
		ResponseModel:       "actual-model",
		AttributionConflict: true,
		BillingDiagnostics:  map[string]bool{"must_not_be_added_after_finalize": true},
	}
	secondUsage.MarkProviderReported()
	second, err := attempt.CloseFromProviderResult(context.Background(), secondUsage, false)
	if err != nil || second != first {
		t.Fatalf("repeated close result=%+v err=%v, want cached %+v", second, err, first)
	}
	if applyCalls.Load() != 1 {
		t.Fatalf("settlement applied %d times", applyCalls.Load())
	}
	if quota.settlementModel != firstModel || quota.settlementPrice == nil || quota.settlementPrice.Input != firstPrice.Input || quota.settlementPrice.Output != firstPrice.Output {
		t.Fatalf("repeated close changed settlement price metadata: model=%q price=%+v", quota.settlementModel, quota.settlementPrice)
	}
	if len(quota.billingDiagnostics) != len(firstDiagnostics) {
		t.Fatalf("repeated close changed diagnostics: before=%v after=%v", firstDiagnostics, quota.billingDiagnostics)
	}
	for key, value := range firstDiagnostics {
		if quota.billingDiagnostics[key] != value {
			t.Fatalf("repeated close changed diagnostic %q: before=%v after=%v", key, value, quota.billingDiagnostics[key])
		}
	}
}

func TestAttemptQuotaConcurrentCloseEvaluatesAndSettlesOnce(t *testing.T) {
	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"request-model": {Model: "request-model", Type: model.TokensPriceType, Input: 1, Output: 1},
		"actual-model":  {Model: "actual-model", Type: model.TokensPriceType, Input: 3, Output: 1},
	}}
	t.Cleanup(func() { model.PricingInstance = originalPricing })

	originalApply := applyBillingSettlement
	var applyCalls atomic.Int32
	applyBillingSettlement = func(context.Context, billing.SettlementCommand, *billing.SettlementOptions) (billing.SettlementResult, error) {
		applyCalls.Add(1)
		return billing.SettlementResult{TruthApplied: true, BalanceOutcome: model.BillingBalanceCommitted}, nil
	}
	t.Cleanup(func() { applyBillingSettlement = originalApply })

	quota := &Quota{modelName: "request-model", groupName: useComponentTestGroup(t), groupRatio: 1}
	attempt := &AttemptQuota{quota: quota}
	requestUsage := &types.Usage{PromptTokens: 1, TotalTokens: 1}
	requestUsage.MarkProviderReported()
	actualUsage := &types.Usage{PromptTokens: 2, TotalTokens: 2, ResponseModel: "actual-model"}
	actualUsage.MarkProviderReported()

	start := make(chan struct{})
	results := make(chan AttemptResult, 2)
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for _, usage := range []*types.Usage{requestUsage, actualUsage} {
		usage := usage
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			result, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
			results <- result
			errors <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	close(errors)

	var first *AttemptResult
	for result := range results {
		result := result
		if first == nil {
			first = &result
			continue
		}
		if result != *first {
			t.Fatalf("concurrent close returned different results: first=%+v second=%+v", *first, result)
		}
	}
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent close failed: %v", err)
		}
	}
	if applyCalls.Load() != 1 {
		t.Fatalf("concurrent close applied settlement %d times", applyCalls.Load())
	}
	if first == nil || !first.Confirmed || (first.ChargedQuota != 1 && first.ChargedQuota != 6) {
		t.Fatalf("unexpected winning close result: %+v", first)
	}
	if first.ChargedQuota == 1 && quota.settlementModel != "request-model" {
		t.Fatalf("request-model result retained mismatched metadata: %+v", quota)
	}
	if first.ChargedQuota == 6 && quota.settlementModel != "actual-model" {
		t.Fatalf("actual-model result retained mismatched metadata: %+v", quota)
	}
}
