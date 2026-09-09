package relay_util

import (
	"errors"
	"math"
	"one-api/model"
	"one-api/types"
)

// PreviewTokenPrice 只对显式模拟事实运行同一个 reducer，不持久化、预扣或调用 provider。
func PreviewTokenPrice(price model.Price, facts model.PriceRuleFacts, usage *types.Usage, groupRatio float64) (UsageSettlementDecision, map[string]any, error) {
	if price.Type != model.TokensPriceType || usage == nil || groupRatio < 0 || math.IsNaN(groupRatio) || math.IsInf(groupRatio, 0) {
		return UsageSettlementDecision{}, nil, errors.New("preview requires a token price, usage and a finite non-negative group ratio")
	}
	q := &Quota{startTime: facts.StartedAt}
	usage.ServiceTier, usage.Speed = facts.ServiceTier, facts.Speed
	policy := quotaPricePolicy{price: cloneQuotaPrice(price), modelName: price.Model, serviceTier: facts.ServiceTier, inputMultiplier: 1, outputMultiplier: 1, groupRatio: groupRatio}
	policy = q.applyRateRules(policy, usage)
	decision := q.evaluateProviderUsageWithPolicy(usage, policy)
	return decision, q.GetLogMeta(usage), nil
}
