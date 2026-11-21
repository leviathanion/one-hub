package relay_util

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"one-api/common/utils"
	"reflect"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/internal/billing"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/datatypes"
)

func TestUsageDiagnosticDoesNotCreateCancelledConsumption(t *testing.T) {
	for _, test := range []struct {
		name        string
		consumeLog  bool
		wantWarning bool
	}{
		{name: "missing", consumeLog: true, wantWarning: true},
		{name: "missing_logging_disabled", wantWarning: true},
		{name: "nil", consumeLog: true, wantWarning: true},
		{name: "conflicting", consumeLog: true, wantWarning: true},
		{name: "zero", consumeLog: true},
		{name: "normal", consumeLog: true},
		{name: "tokens_with_tool_conflict", wantWarning: true},
		{name: "tokens_with_informational_diagnostic"},
		{name: "operation_only", consumeLog: true},
		{name: "independent_tool", consumeLog: true, wantWarning: true},
		{name: "refund_unknown", consumeLog: true, wantWarning: true},
		{name: "confirm_unknown", consumeLog: true, wantWarning: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			oldLogger := logger.Logger
			useQuotaReserveTestDB(t)
			insertQuotaReserveFixtures(t, 1000)
			if err := model.DB.AutoMigrate(&model.Log{}); err != nil {
				t.Fatal(err)
			}
			core, logs := observer.New(zap.WarnLevel)
			logger.Logger = zap.New(core)
			oldPricing, oldBatch, oldLog, oldReserve := model.PricingInstance, config.BatchUpdateEnabled, config.LogConsumeEnabled, config.PreConsumedQuota
			oldConfirm, oldRefund := applyBillingSettlement, applyBillingRefund
			price := &model.Price{Model: "usage-model", Type: model.TokensPriceType, Input: 1, Output: 1}
			if test.name == "operation_only" {
				price.Type = model.TimesPriceType
			}
			model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{"usage-model": price}}
			config.BatchUpdateEnabled, config.LogConsumeEnabled, config.PreConsumedQuota = false, test.consumeLog, 50
			t.Cleanup(func() {
				logger.Logger = oldLogger
				model.PricingInstance, config.BatchUpdateEnabled, config.LogConsumeEnabled, config.PreConsumedQuota = oldPricing, oldBatch, oldLog, oldReserve
				applyBillingSettlement, applyBillingRefund = oldConfirm, oldRefund
			})
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			requestCtx := context.WithValue(context.Background(), logger.RequestIdKey, "usage-diagnostic-request")
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(requestCtx)
			ctx.Set("id", 1)
			ctx.Set("token_id", 1)
			setQuotaTestRoutingGroup(t, ctx, 1)
			attempt, err := NewAttemptQuota(ctx, "usage-model", 10, BillingAttemptSpec{})
			if err != nil {
				t.Fatal(err)
			}
			if err := attempt.ApplyReserve(requestCtx); err != nil {
				t.Fatal(err)
			}
			if err := attempt.ClaimSubmission(); err != nil {
				t.Fatal(err)
			}
			usage := &types.Usage{PromptTokens: 999, CompletionTokens: 999}
			switch test.name {
			case "nil":
				usage = nil
			case "zero":
				usage = &types.Usage{}
				usage.MarkProviderReported()
			case "normal", "confirm_unknown", "tokens_with_tool_conflict", "tokens_with_informational_diagnostic":
				usage = &types.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7}
				usage.MarkProviderReported()
				switch test.name {
				case "tokens_with_tool_conflict":
					usage.MarkExtraBillingConflict(types.APIToolTypeWebSearch)
				case "tokens_with_informational_diagnostic":
					usage.AddBillingDiagnostic("web_search_action_unknown")
				}
			case "conflicting":
				usage.MarkProviderReported()
				usage.ProviderTokenConflict = true
			case "operation_only":
				usage = &types.Usage{}
				usage.MarkProviderOperationUnits(1)
			case "independent_tool":
				usage.SetProviderExtraBilling(types.APIToolTypeWebSearch, "", 1)
			}
			calls := 0
			if test.name == "confirm_unknown" {
				applyBillingSettlement = func(context.Context, billing.SettlementCommand, *billing.SettlementOptions) (billing.SettlementResult, error) {
					calls++
					return billing.SettlementResult{BalanceOutcome: model.BillingBalanceCommitUnknown}, errors.New("commit unknown")
				}
			}
			if test.name == "refund_unknown" {
				applyBillingRefund = func(context.Context, int, int, bool, int64) (model.BillingBalanceResult, error) {
					calls++
					return model.BillingBalanceResult{Outcome: model.BillingBalanceCommitUnknown}, errors.New("refund unknown")
				}
			}
			first, firstErr := attempt.CloseFromProviderResult(requestCtx, usage, true)
			second, secondErr := attempt.CloseFromProviderResult(requestCtx, usage, true)
			if first != second || firstErr != secondErr {
				t.Fatalf("重复关闭改变结算结果：%+v/%+v %v/%v", first, second, firstErr, secondErr)
			}
			failed := strings.HasSuffix(test.name, "_unknown")
			if (firstErr != nil) != failed || first.Unsettled != failed {
				t.Fatalf("结算状态不符合预期：%+v %v", first, firstErr)
			}
			if failed && calls != 1 {
				t.Fatalf("不确定余额操作重放：%d", calls)
			}
			warnings := logs.FilterMessageSnippet("billing_usage_diagnostic").All()
			wantWarnings := 0
			if test.wantWarning {
				wantWarnings = 1
			}
			if len(warnings) != wantWarnings {
				t.Fatalf("系统诊断条数=%d，预期%d：%+v", len(warnings), wantWarnings, warnings)
			}
			if test.name == "tokens_with_tool_conflict" {
				if !first.Confirmed || first.ChargedQuota != 7 || !strings.Contains(warnings[0].Message, "unit_service_conflict:web_search") {
					t.Fatalf("工具冲突诊断或独立 token 结算异常：%+v，%s", first, warnings[0].Message)
				}
			}
			if test.wantWarning {
				message := warnings[0].Message
				if !strings.Contains(message, "usage-diagnostic-request") || !strings.Contains(message, `model="usage-model"`) || !strings.Contains(message, "stream=true") || !strings.Contains(message, "channel_id=0") {
					t.Fatalf("系统诊断缺少关联信息：%s", message)
				}
				if strings.Contains(message, "token-key-1") || strings.Contains(message, "access-token-1") || strings.Contains(message, "999") {
					t.Fatalf("系统诊断泄露凭据或本地估算：%s", message)
				}
				if failed && !strings.Contains(message, "settlement=unsettled final_quota=unknown") {
					t.Fatalf("不确定结算被显示为已收费或免费：%s", message)
				}
			}
			var count int64
			if err := model.DB.Model(&model.Log{}).Where("type = ?", model.LogTypeConsume).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			var user model.User
			if err := model.DB.First(&user, 1).Error; err != nil {
				t.Fatal(err)
			}
			wantCount := int64(0)
			if first.Confirmed && !failed {
				wantCount = 1
			}
			if user.RequestCount != int(wantCount) {
				t.Fatalf("诊断改变了请求统计：%d，预期%d", user.RequestCount, wantCount)
			}
			if !test.consumeLog {
				wantCount = 0
			}
			if count != wantCount {
				t.Fatalf("诊断新增消费记录：%d，预期%d", count, wantCount)
			}
			if !failed && user.Quota != 1000-int(first.ChargedQuota) {
				t.Fatalf("额度结算不一致：余额%d，费用%d", user.Quota, first.ChargedQuota)
			}
		})
	}
}

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
