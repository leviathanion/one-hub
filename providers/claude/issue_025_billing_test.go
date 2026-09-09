package claude_test

import (
 "one-api/common/utils"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/groupctx"
	"one-api/model"
	"one-api/providers/claude"
	"one-api/relay/relay_util"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

func TestI025ClaudeProviderTierReachesConfiguredChargeAndLog(t *testing.T) {
	var providerUsage claude.Usage
	if err := json.Unmarshal([]byte(`{"input_tokens":100,"output_tokens":20,"service_tier":"priority"}`), &providerUsage); err != nil {
		t.Fatalf("解码 Claude usage 失败: %v", err)
	}
	usage := &types.Usage{}
	if !claude.ClaudeUsageToOpenaiUsage(&providerUsage, usage) {
		t.Fatalf("Claude usage 转换失败: %+v", providerUsage)
	}
	usage.MergeProviderAttribution("claude-test", "")

	const groupName = "i025-tier"
	model.GlobalUserGroupRatio.Lock()
	originalGroups := model.GlobalUserGroupRatio.UserGroup
	groups := make(map[string]*model.UserGroup, len(originalGroups)+1)
	for name, group := range originalGroups {
		groups[name] = group
	}
	groups[groupName] = &model.UserGroup{Symbol: groupName, Ratio: 1}
	model.GlobalUserGroupRatio.UserGroup = groups
	model.GlobalUserGroupRatio.Unlock()
	t.Cleanup(func() {
		model.GlobalUserGroupRatio.Lock()
		model.GlobalUserGroupRatio.UserGroup = originalGroups
		model.GlobalUserGroupRatio.Unlock()
	})

	rules := datatypes.NewJSONType(model.PriceRateRules{Version: 2, ServiceTier: []model.PriceRateRule{{ID: "priority", When: model.PriceRuleCondition{ServiceTier: []string{"fast", "priority"}}, Multipliers: model.PriceRateMultiplier{Input: utils.GetPointer(float64(2)), Output: utils.GetPointer(float64(2))}}}})
	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"claude-test": {Model: "claude-test", Type: model.TokensPriceType, Input: 1, Output: 1, RateRules: &rules},
	}}
	t.Cleanup(func() { model.PricingInstance = originalPricing })

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	groupctx.SetRoutingGroup(ctx, groupName, groupctx.RoutingGroupSourceUserGroup)
	ctx.Set("group_ratio", 1.0)
	quota := relay_util.NewQuota(ctx, "claude-test", 0)
	decision := quota.EvaluateProviderUsage(usage)
	if !decision.Confirm || decision.FinalQuota != 240 {
		t.Fatalf("Claude priority 100+20 未按 fast_priority=2 结算 240: usage=%+v decision=%+v", usage, decision)
	}
	meta := quota.GetLogMeta(usage)
	if meta["effective_service_tier"] != "priority" || meta["billing_input_multiplier"] != float64(2) || meta["billing_output_multiplier"] != float64(2) {
		t.Fatalf("Claude priority 计费日志未记录实际 tier/倍率: %+v", meta)
	}
	var billing struct {
		Charge int64 `json:"charge"`
	}
	rawBilling, err := json.Marshal(meta["token_billing"])
	if err != nil || json.Unmarshal(rawBilling, &billing) != nil || billing.Charge != 240 {
		t.Fatalf("Claude priority 计费日志未保留 240 charge: err=%v meta=%s", err, rawBilling)
	}

	var defaultProviderUsage claude.Usage
	if err := json.Unmarshal([]byte(`{"input_tokens":100,"output_tokens":20,"service_tier":"default"}`), &defaultProviderUsage); err != nil {
		t.Fatalf("解码 Claude default usage 失败: %v", err)
	}
	defaultUsage := &types.Usage{}
	if !claude.ClaudeUsageToOpenaiUsage(&defaultProviderUsage, defaultUsage) {
		t.Fatalf("Claude default usage 转换失败: %+v", defaultProviderUsage)
	}
	defaultUsage.MergeProviderAttribution("claude-test", "")
	defaultDecision := quota.EvaluateProviderUsage(defaultUsage)
	if !defaultDecision.Confirm || defaultDecision.FinalQuota != 120 {
		t.Fatalf("Claude 实际 default 不应套 priority 倍率: usage=%+v decision=%+v", defaultUsage, defaultDecision)
	}
}
