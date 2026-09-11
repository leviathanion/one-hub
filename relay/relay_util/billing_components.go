package relay_util

import (
	"math"
	"sort"
	"strings"

	"one-api/model"
	"one-api/types"
)

type PriceComponentStatus string

const (
	PriceComponentPriceable           PriceComponentStatus = "priceable"
	PriceComponentNotApplicable       PriceComponentStatus = "not_applicable"
	PriceComponentMissingEvidence     PriceComponentStatus = "missing_evidence"
	PriceComponentConflictingEvidence PriceComponentStatus = "conflicting_evidence"
)

type PriceComponentDecision struct {
	Name          string
	Status        PriceComponentStatus
	Charge        int64
	ProviderUnits bool
}

type UsageSettlementDecision struct {
	Confirm    bool
	FinalQuota int64
	Components []PriceComponentDecision
}

// EvaluateProviderUsage reduces independent price components. A missing token
// component cannot erase a complete provider tool/search/image unit, while an
// incomplete token partition is never charged partially.
func (q *Quota) EvaluateProviderUsage(usage *types.Usage) UsageSettlementDecision {
	decision := UsageSettlementDecision{}
	if q == nil || usage == nil {
		return decision
	}
	for diagnostic, present := range usage.BillingDiagnostics {
		if present {
			q.addBillingDiagnostic(diagnostic)
		}
	}
	actualModel := usage.ResponseModel
	serviceTier := usage.ServiceTier
	if usage.AttributionConflict {
		q.addBillingDiagnostic("billing_attribution_conflict")
		actualModel = ""
		serviceTier = ""
	}
	policy := q.resolveQuotaPricePolicy(actualModel, serviceTier, usage)
	return q.evaluateProviderUsageWithPolicy(usage, policy)
}

func (q *Quota) evaluateProviderUsageWithPolicy(usage *types.Usage, policy quotaPricePolicy) UsageSettlementDecision {
	decision := UsageSettlementDecision{}
	if usage.ProviderOperationUnits != nil && usage.AttributionConflict {
		decision.Components = append(decision.Components,
			PriceComponentDecision{Name: "operation_units", Status: PriceComponentConflictingEvidence},
			PriceComponentDecision{Name: "tokens", Status: PriceComponentNotApplicable},
		)
	} else if !policy.missing && policy.price.Type == model.TimesPriceType && usage.ProviderOperationUnits != nil {
		if *usage.ProviderOperationUnits < 0 {
			q.addBillingDiagnostic("operation_unit_component_conflict")
			decision.Components = append(decision.Components,
				PriceComponentDecision{Name: "operation_units", Status: PriceComponentConflictingEvidence},
				PriceComponentDecision{Name: "tokens", Status: PriceComponentNotApplicable},
			)
		} else {
			perOperation := q.getTotalQuotaWithPolicyUnits(policy, 0, 0, nil)
			charge := saturatingMultiplyInt(perOperation, *usage.ProviderOperationUnits)
			decision.Components = append(decision.Components,
				PriceComponentDecision{Name: "operation_units", Status: PriceComponentPriceable, Charge: int64(charge), ProviderUnits: true},
				PriceComponentDecision{Name: "tokens", Status: PriceComponentNotApplicable},
			)
		}
	} else if usage.AttributionConflict || usage.ProviderTokenConflict {
		q.addBillingDiagnostic("billing_attribution_conflict")
		decision.Components = append(decision.Components, PriceComponentDecision{Name: "tokens", Status: PriceComponentConflictingEvidence})
	} else if policy.missing {
		q.addBillingDiagnostic("price_policy_missing_at_settlement")
		decision.Components = append(decision.Components, PriceComponentDecision{Name: "tokens", Status: PriceComponentMissingEvidence})
	} else if policy.ruleStatus != "" {
		decision.Components = append(decision.Components, PriceComponentDecision{Name: "tokens", Status: policy.ruleStatus})
	} else if usage.HasProviderBaseUsage() {
		tokenStatus, partitionAdjustment := assessTokenEvidenceForPrice(usage, policy)
		if tokenStatus == PriceComponentConflictingEvidence {
			q.addBillingDiagnostic("token_partition_conflict")
			decision.Components = append(decision.Components, PriceComponentDecision{Name: "tokens", Status: tokenStatus})
		} else if tokenStatus != PriceComponentPriceable {
			q.addBillingDiagnostic("token_component_missing_evidence")
			decision.Components = append(decision.Components, PriceComponentDecision{Name: "tokens", Status: tokenStatus})
		} else {
			promptUnits, completionUnits := q.getComputeTokenUnitsByUsageWithPrice(usage, policy, partitionAdjustment)
			if promptUnits < 0 || completionUnits < 0 || math.IsNaN(promptUnits) || math.IsNaN(completionUnits) {
				q.addBillingDiagnostic("token_partition_conflict")
				decision.Components = append(decision.Components, PriceComponentDecision{Name: "tokens", Status: PriceComponentConflictingEvidence})
			} else {
				charge := q.getTotalQuotaWithPolicyUnits(policy, promptUnits, completionUnits, nil)
				if math.IsNaN(promptUnits) || math.IsInf(promptUnits, 0) || math.IsNaN(completionUnits) || math.IsInf(completionUnits, 0) {
					// 计费沿用既有饱和处理；不可表示的审计基数不能让整条日志序列化失败。
					q.settlementTokenBilling = nil
					q.addBillingDiagnostic("token_billing_units_unrepresentable")
				}
				if q.settlementTokenBilling != nil {
					q.settlementTokenBilling.InputUnits = promptUnits
					q.settlementTokenBilling.OutputUnits = completionUnits
				}
				decision.Components = append(decision.Components, PriceComponentDecision{Name: "tokens", Status: PriceComponentPriceable, Charge: int64(charge), ProviderUnits: true})
			}
		}
	} else {
		q.addBillingDiagnostic("token_component_missing_evidence")
		decision.Components = append(decision.Components, PriceComponentDecision{Name: "tokens", Status: PriceComponentMissingEvidence})
	}

	if details := q.settlementTokenBilling; details != nil {
		for _, component := range decision.Components {
			if component.Name == "tokens" {
				details.Status = component.Status
				details.Charge = component.Charge
				if component.Status != PriceComponentPriceable {
					// 独立工具收费不能让未获授权的 Token 规则看起来已产生费用。
					details.Rules = details.Rules[:0]
				}
				break
			}
		}
	}

	independentKeys := make([]string, 0, len(usage.ProviderIndependentUsageUnits))
	for key, present := range usage.ProviderIndependentUsageUnits {
		if present {
			independentKeys = append(independentKeys, key)
		}
	}
	sort.Strings(independentKeys)
	for _, key := range independentKeys {
		value, present := usage.ExtraUsageUnits[key]
		componentName := "independent_unit:" + key
		if !present || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			decision.Components = append(decision.Components, PriceComponentDecision{Name: componentName, Status: PriceComponentConflictingEvidence})
			continue
		}
		if usage.AttributionConflict {
			decision.Components = append(decision.Components, PriceComponentDecision{Name: componentName, Status: PriceComponentConflictingEvidence})
			continue
		}
		if policy.price.ExtraRatios == nil {
			q.addBillingDiagnostic("independent_unit_price_missing:" + key)
			decision.Components = append(decision.Components, PriceComponentDecision{Name: componentName, Status: PriceComponentMissingEvidence})
			continue
		}
		if _, ok := policy.price.ExtraRatios.Data()[key]; !ok {
			q.addBillingDiagnostic("independent_unit_price_missing:" + key)
			decision.Components = append(decision.Components, PriceComponentDecision{Name: componentName, Status: PriceComponentMissingEvidence})
			continue
		}
		baseRate := policy.price.GetInput()
		if !model.GetExtraPriceIsPrompt(key) {
			baseRate = policy.price.GetOutput()
		}
		charge := saturatingCeilToInt(value * baseRate * policy.price.GetExtraRatio(key) * policy.groupRatio)
		decision.Components = append(decision.Components, PriceComponentDecision{Name: componentName, Status: PriceComponentPriceable, Charge: int64(charge), ProviderUnits: true})
	}

	providerUnits := make(map[string]types.ExtraBilling)
	keys := make([]string, 0, len(usage.ExtraBilling))
	for key := range usage.ExtraBilling {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		billing := usage.ExtraBilling[key]
		componentName := "unit:" + key
		if usage.HasExtraBillingConflict(types.ResolveExtraBillingServiceType(key, billing)) {
			decision.Components = append(decision.Components, PriceComponentDecision{Name: componentName, Status: PriceComponentConflictingEvidence})
			continue
		}
		if !usage.HasProviderExtraBilling(key) {
			q.addBillingDiagnostic("unit_component_untrusted:" + key)
			decision.Components = append(decision.Components, PriceComponentDecision{Name: componentName, Status: PriceComponentMissingEvidence})
			continue
		}
		if billing.CallCount < 0 {
			q.addBillingDiagnostic("unit_component_conflict:" + key)
			decision.Components = append(decision.Components, PriceComponentDecision{Name: componentName, Status: PriceComponentConflictingEvidence})
			continue
		}
		serviceType := types.ResolveExtraBillingServiceType(key, billing)
		billingType := types.ResolveExtraBillingType(key, billing)
		price, diagnostic := getDefaultExtraServicePriceDecision(serviceType, policy.modelName, billingType)
		if diagnostic != "" {
			q.addBillingDiagnostic(diagnostic)
		}
		if usage.AttributionConflict && serviceType == types.APIToolTypeWebSearchPreview {
			decision.Components = append(decision.Components, PriceComponentDecision{Name: componentName, Status: PriceComponentConflictingEvidence})
			continue
		}
		if price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
			q.addBillingDiagnostic("unit_component_price_missing:" + key)
			decision.Components = append(decision.Components, PriceComponentDecision{Name: componentName, Status: PriceComponentMissingEvidence})
			continue
		}
		charge := saturatingCeilToInt(float64(billing.CallCount) * price * q.effectiveQuotaPerUnit() * policy.groupRatio)
		decision.Components = append(decision.Components, PriceComponentDecision{Name: componentName, Status: PriceComponentPriceable, Charge: int64(charge), ProviderUnits: true})
		providerUnits[key] = billing
	}
	q.getExtraBillingDataForModel(providerUnits, policy.modelName)

	for _, component := range decision.Components {
		if component.Status != PriceComponentPriceable || !component.ProviderUnits {
			continue
		}
		decision.Confirm = true
		if component.Charge > 0 && decision.FinalQuota > math.MaxInt64-component.Charge {
			decision.FinalQuota = math.MaxInt64
		} else {
			decision.FinalQuota += component.Charge
		}
	}
	return decision
}

func saturatingMultiplyInt(value, count int) int {
	if value <= 0 || count <= 0 {
		return 0
	}
	maxInt := int(^uint(0) >> 1)
	if value > maxInt/count {
		return maxInt
	}
	return value * count
}

func componentDiagnostics(components []PriceComponentDecision) []string {
	diagnostics := make([]string, 0, len(components))
	for _, component := range components {
		if component.Status == PriceComponentPriceable || component.Status == PriceComponentNotApplicable {
			continue
		}
		diagnostics = append(diagnostics, strings.TrimSpace(component.Name)+":"+string(component.Status))
	}
	sort.Strings(diagnostics)
	return diagnostics
}
