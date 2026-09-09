package relay_util

import (
	"context"
	"encoding/json"
	"math"
	"one-api/common/utils"
	"reflect"
	"testing"

	"one-api/common/config"
	"one-api/model"
	"one-api/types"

	"gorm.io/datatypes"
)

func loggedTokenBilling(t *testing.T, meta map[string]any) tokenBillingDetails {
	t.Helper()
	raw, err := json.Marshal(meta["token_billing"])
	if err != nil {
		t.Fatal(err)
	}
	var details tokenBillingDetails
	if err := json.Unmarshal(raw, &details); err != nil {
		t.Fatal(err)
	}
	return details
}

func TestBillingLogRecordsOnlyAppliedRateRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		input  int
		tier   string
		kinds  []string
		inMul  float64
		outMul float64
	}{
		{"boundary", 272000, "default", nil, 1, 1},
		{"long", 272001, "default", []string{"long_context"}, 2, 1.5},
		{"fast", 100, "fast", []string{"service_tier"}, 2, 2},
		{"priority", 100, "priority", []string{"service_tier"}, 2, 2},
		{"flex", 100, "flex", []string{"service_tier"}, 0.5, 0.5},
		{"stacked", 272001, "fast", []string{"service_tier", "long_context"}, 4, 3},
		{"long-flex", 272001, "flex", []string{"service_tier", "long_context"}, 1, 0.75},
		{"unknown-tier", 100, "future", nil, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := newTieredQuotaForTest(t)
			usage := &types.Usage{PromptTokens: tc.input, CompletionTokens: 10, ServiceTier: tc.tier}
			charge := mustEvaluateProviderQuota(t, q, usage)
			meta := q.GetLogMeta(usage)
			details := loggedTokenBilling(t, meta)
			if details.Status != PriceComponentPriceable || details.Charge != int64(charge) || details.InputUnits != float64(tc.input)*tc.inMul || details.OutputUnits != 10*tc.outMul {
				t.Fatalf("unexpected token settlement facts: %+v, charge=%d", details, charge)
			}
			if details.BaseInputRatio != 2.5 || details.BaseOutputRatio != 15 || meta["input_ratio"] != 2.5*tc.inMul || meta["output_ratio"] != 15*tc.outMul {
				t.Fatalf("base/effective prices disagree: details=%+v metadata=%+v", details, meta)
			}
			var kinds []string
			for _, rule := range details.Rules {
				kinds = append(kinds, rule.Kind)
				if rule.Kind == "long_context" && (rule.When.InputTokens.GT == nil || *rule.When.InputTokens.GT != 272000) {
					t.Fatalf("missing long-context threshold: %+v", rule)
				}
			}
			if !reflect.DeepEqual(kinds, tc.kinds) || details.Rules == nil {
				t.Fatalf("applied rules=%v want=%v; new logs must retain an explicit empty rules array", kinds, tc.kinds)
			}
		})
	}
}

func TestBillingLogKeepsSettlementFactsAfterConfigurationChanges(t *testing.T) {
	q := newTieredQuotaForTest(t)
	usage := &types.Usage{PromptTokens: 272001, CompletionTokens: 10, ServiceTier: "fast"}
	mustEvaluateProviderQuota(t, q, usage)
	before := loggedTokenBilling(t, q.GetLogMeta(usage))
	model.PricingInstance.Prices[q.modelName].Input = 999
	model.PricingInstance.Prices[q.modelName].RateRules = nil
	after := loggedTokenBilling(t, q.GetLogMeta(usage))
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("logging reread current configuration: before=%+v after=%+v", before, after)
	}

	// 下一次评估仍应读取当前配置，不能把日志事实当成配置快照复用。
	mustEvaluateProviderQuota(t, q, &types.Usage{PromptTokens: 10, CompletionTokens: 1, ServiceTier: "default"})
	next := loggedTokenBilling(t, q.GetLogMeta(nil))
	if next.BaseInputRatio != 999 || len(next.Rules) != 0 {
		t.Fatalf("new evaluation retained old rules: %+v", next)
	}
}

func TestBillingLogDoesNotClaimRulesChargedForMissingOrConflictingTokens(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		q := newTieredQuotaForTest(t)
		usage := &types.Usage{PromptTokens: 272001, ServiceTier: "fast"}
		if conflict {
			usage.MarkProviderReported()
			usage.AttributionConflict = true
		}
		usage.SetProviderExtraBilling(types.APIToolTypeWebSearch, "", 1)
		decision := q.EvaluateProviderUsage(usage)
		details := loggedTokenBilling(t, q.GetLogMeta(usage))
		if !decision.Confirm || details.Status == PriceComponentPriceable || details.Charge != 0 || len(details.Rules) != 0 || details.InputUnits != 0 {
			t.Fatalf("tool-only settlement claimed token charges: conflict=%v details=%+v decision=%+v", conflict, details, decision)
		}
	}
}

func TestBillingLogRecordsFractionalTokenUnits(t *testing.T) {
	q := newTieredQuotaForTest(t)
	usage := &types.Usage{PromptTokens: 10, CompletionTokens: 3, ServiceTier: "fast"}
	usage.PromptTokensDetails = types.PromptTokensDetails{CachedTokens: 9}
	charge := mustEvaluateProviderQuota(t, q, usage)
	details := loggedTokenBilling(t, q.GetLogMeta(usage))
	if math.Abs(details.InputUnits-3.8) > 1e-12 || details.OutputUnits != 6 || charge != 100 || details.Charge != 100 {
		t.Fatalf("fractional charge facts were rounded or changed: %+v charge=%d", details, charge)
	}
}

func TestBillingLogRecordsZeroThresholdAndFreeTokenCharge(t *testing.T) {
	q := newTieredQuotaForTest(t)
	price := model.PricingInstance.Prices[q.modelName]
	price.Input, price.Output = 0, 0
	rules := datatypes.NewJSONType(model.PriceRateRules{Version: 2, LongContext: []model.PriceRateRule{{ID: "long_context", When: model.PriceRuleCondition{InputTokens: &model.PriceTokenRange{GT: utils.GetPointer(0)}}, Multipliers: model.PriceRateMultiplier{Input: utils.GetPointer(float64(2)), Output: utils.GetPointer(float64(1.5))}}}})
	price.RateRules = &rules
	usage := &types.Usage{PromptTokens: 1}
	if charge := mustEvaluateProviderQuota(t, q, usage); charge != 0 {
		t.Fatalf("free price charged %d", charge)
	}
	meta := q.GetLogMeta(usage)
	details := loggedTokenBilling(t, meta)
	if details.Charge != 0 || details.Status != PriceComponentPriceable || len(details.Rules) != 1 || details.Rules[0].When.InputTokens.GT == nil || *details.Rules[0].When.InputTokens.GT != 0 {
		t.Fatalf("zero threshold/free charge facts lost: %+v", details)
	}
	if meta["input_ratio"] != float64(0) || meta["output_ratio"] != float64(0) {
		t.Fatalf("zero effective prices replaced with estimates: %+v", meta)
	}
}

func TestAttemptPersistsBillingRulesWithTheChargedAmount(t *testing.T) {
	useQuotaReserveTestDB(t)
	insertQuotaReserveFixtures(t, 10000000)
	if err := model.DB.AutoMigrate(&model.Log{}, &model.Statistics{}); err != nil {
		t.Fatal(err)
	}
	originalBatch := config.BatchUpdateEnabled
	config.BatchUpdateEnabled = false
	t.Cleanup(func() { config.BatchUpdateEnabled = originalBatch })
	q := newTieredQuotaForTest(t)
	q.userId, q.tokenId = 1, 1
	attempt := &AttemptQuota{quota: q}
	if err := attempt.ApplyReserve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatal(err)
	}
	usage := &types.Usage{PromptTokens: 272001, CompletionTokens: 10, ServiceTier: "fast"}
	usage.MarkProviderReported()
	result, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
	if err != nil {
		t.Fatal(err)
	}
	var log model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Take(&log).Error; err != nil {
		t.Fatal(err)
	}
	details := loggedTokenBilling(t, log.Metadata.Data())
	if log.Quota != int(result.ChargedQuota) || details.Charge != result.ChargedQuota || len(details.Rules) != 2 {
		t.Fatalf("persisted rules disagree with settlement: log=%d result=%+v details=%+v", log.Quota, result, details)
	}
	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil || user.Quota != 10000000-log.Quota {
		t.Fatalf("balance/log mismatch: quota=%d log=%d err=%v", user.Quota, log.Quota, err)
	}
}

func TestBillingLogCannotLoseTheWholeLogToOverflowedAuditUnits(t *testing.T) {
	q := newTieredQuotaForTest(t)
	price := model.PricingInstance.Prices[q.modelName]
	extra := datatypes.NewJSONType(map[string]float64{config.UsageExtraInputAudio: math.MaxFloat64})
	price.ExtraRatios = &extra
	usage := &types.Usage{PromptTokens: 100, PromptTokensDetails: types.PromptTokensDetails{AudioTokens: 100}}
	mustEvaluateProviderQuota(t, q, usage)
	meta := q.GetLogMeta(usage)
	if _, ok := meta["token_billing"]; ok || !q.billingDiagnostics["token_billing_units_unrepresentable"] {
		t.Fatalf("unrepresentable units must be omitted and diagnosed: %+v", meta)
	}
	if _, err := json.Marshal(meta); err != nil {
		t.Fatalf("audit units prevented log serialization: %v", err)
	}
}
