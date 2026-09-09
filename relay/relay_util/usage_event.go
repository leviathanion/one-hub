package relay_util

import (
	"strings"

	"one-api/types"
)

// ProviderUsageEventForBilling keeps independently trusted provider
// components while removing token numerics that lack provider-local evidence.
// Source is diagnostic metadata and never grants billing authority.
func ProviderUsageEventForBilling(usage *types.UsageEvent) *types.UsageEvent {
	projected := usage.Clone()
	if projected == nil {
		return nil
	}
	if !projected.ProviderTokenEvidence {
		projected.InputTokens = 0
		projected.OutputTokens = 0
		projected.TotalTokens = 0
		projected.InputTokenDetails = types.PromptTokensDetails{}
		projected.OutputTokenDetails = types.CompletionTokensDetails{}
		projected.ExtraTokens = nil
	}
	projected.ExtraUsageUnits, projected.ProviderIndependentUsageUnits = providerIndependentUsageForBilling(
		projected.ExtraUsageUnits,
		projected.ProviderIndependentUsageUnits,
	)
	projected.ExtraBilling, projected.ProviderExtraBilling = providerExtraBillingForBilling(
		projected.ExtraBilling,
		projected.ProviderExtraBilling,
	)
	return projected
}

func providerIndependentUsageForBilling(values map[string]float64, evidence map[string]bool) (map[string]float64, map[string]bool) {
	var trustedValues map[string]float64
	var trustedEvidence map[string]bool
	for key, present := range evidence {
		value, exists := values[key]
		if !present || !exists {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if trustedValues == nil {
			trustedValues = make(map[string]float64)
			trustedEvidence = make(map[string]bool)
		}
		trustedValues[key] += value
		trustedEvidence[key] = true
	}
	return trustedValues, trustedEvidence
}

func providerExtraBillingForBilling(values map[string]types.ExtraBilling, evidence map[string]bool) (map[string]types.ExtraBilling, map[string]bool) {
	var trustedValues map[string]types.ExtraBilling
	var trustedEvidence map[string]bool
	for key, present := range evidence {
		value, exists := values[key]
		if !present || !exists {
			continue
		}
		serviceType := types.ResolveExtraBillingServiceType(key, value)
		billingType := types.ResolveExtraBillingType(key, value)
		key = types.BuildExtraBillingKey(serviceType, billingType)
		if key == "" {
			continue
		}
		if trustedValues == nil {
			trustedValues = make(map[string]types.ExtraBilling)
			trustedEvidence = make(map[string]bool)
		}
		trusted := trustedValues[key]
		trusted.ServiceType = serviceType
		trusted.Type = billingType
		trusted.CallCount += value.CallCount
		trustedValues[key] = trusted
		trustedEvidence[key] = true
	}
	return trustedValues, trustedEvidence
}
