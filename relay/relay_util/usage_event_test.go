package relay_util

import (
	"testing"

	"one-api/types"
)

func TestProviderUsageEventForBillingKeepsOnlyExplicitProviderComponents(t *testing.T) {
	trustedUnit := " trusted_unit "
	canonicalTrustedUnit := "trusted_unit"
	untrustedUnit := "untrusted_unit"
	trustedTool := " trusted_tool|metered "
	canonicalTrustedTool := types.BuildExtraBillingKey("trusted_tool", "metered")
	untrustedTool := "untrusted_tool"
	original := &types.UsageEvent{
		InputTokens:  100,
		OutputTokens: 50,
		TotalTokens:  150,
		ExtraUsageUnits: map[string]float64{
			trustedUnit:   2,
			untrustedUnit: 200,
		},
		ProviderIndependentUsageUnits: map[string]bool{trustedUnit: true},
		ExtraBilling: map[string]types.ExtraBilling{
			trustedTool:   {CallCount: 1},
			untrustedTool: {ServiceType: untrustedTool, CallCount: 100},
		},
		ProviderExtraBilling: map[string]bool{trustedTool: true},
	}
	projected := ProviderUsageEventForBilling(original)

	if projected.InputTokens != 0 || projected.OutputTokens != 0 || projected.TotalTokens != 0 {
		t.Fatalf("untrusted token component survived billing projection: %+v", projected)
	}
	if len(projected.ExtraUsageUnits) != 1 || projected.ExtraUsageUnits[canonicalTrustedUnit] != 2 || !projected.ProviderIndependentUsageUnits[canonicalTrustedUnit] {
		t.Fatalf("independent usage projection mismatch: %+v", projected)
	}
	if len(projected.ExtraBilling) != 1 || projected.ExtraBilling[canonicalTrustedTool].CallCount != 1 || !projected.ProviderExtraBilling[canonicalTrustedTool] {
		t.Fatalf("extra billing projection mismatch: %+v", projected)
	}
	if original.InputTokens != 100 || original.ExtraUsageUnits[trustedUnit] != 2 || original.ExtraBilling[trustedTool].CallCount != 1 {
		t.Fatalf("billing projection modified provider observation: %+v", original)
	}
}

func TestProviderUsageEventForBillingNormalizesOnlyAfterExactEvidenceMatch(t *testing.T) {
	rawUnitKey := " duration "
	canonicalUnitKey := "duration"
	rawToolKey := " web_search_preview|medium "
	canonicalToolKey := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	projected := ProviderUsageEventForBilling(&types.UsageEvent{
		ExtraUsageUnits: map[string]float64{
			rawUnitKey:       2,
			canonicalUnitKey: 200,
		},
		ProviderIndependentUsageUnits: map[string]bool{rawUnitKey: true},
		ExtraBilling: map[string]types.ExtraBilling{
			rawToolKey:       {CallCount: 1},
			canonicalToolKey: {CallCount: 100},
		},
		ProviderExtraBilling: map[string]bool{rawToolKey: true},
	})

	if len(projected.ExtraUsageUnits) != 1 || projected.ExtraUsageUnits[canonicalUnitKey] != 2 {
		t.Fatalf("untrusted unit alias entered normalized projection: %+v", projected)
	}
	if len(projected.ExtraBilling) != 1 || projected.ExtraBilling[canonicalToolKey].CallCount != 1 || !projected.ProviderExtraBilling[canonicalToolKey] {
		t.Fatalf("untrusted billing alias entered normalized projection: %+v", projected)
	}
}

func TestProviderUsageEventForBillingAddsExplicitCanonicalAliases(t *testing.T) {
	unitKey := "duration"
	toolKey := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	projected := ProviderUsageEventForBilling(&types.UsageEvent{
		ExtraUsageUnits: map[string]float64{
			unitKey:      2,
			" duration ": 3,
		},
		ProviderIndependentUsageUnits: map[string]bool{unitKey: true, " duration ": true},
		ExtraBilling: map[string]types.ExtraBilling{
			toolKey:                       {ServiceType: types.APIToolTypeWebSearchPreview, Type: "medium", CallCount: 1},
			" web_search_preview|medium ": {ServiceType: types.APIToolTypeWebSearchPreview, Type: "medium", CallCount: 2},
		},
		ProviderExtraBilling: map[string]bool{toolKey: true, " web_search_preview|medium ": true},
	})

	if len(projected.ExtraUsageUnits) != 1 || projected.ExtraUsageUnits[unitKey] != 5 || !projected.ProviderIndependentUsageUnits[unitKey] {
		t.Fatalf("explicit unit aliases were not combined: %+v", projected)
	}
	tool := projected.ExtraBilling[toolKey]
	if len(projected.ExtraBilling) != 1 || tool.CallCount != 3 || tool.ServiceType != types.APIToolTypeWebSearchPreview || tool.Type != "medium" || !projected.ProviderExtraBilling[toolKey] {
		t.Fatalf("explicit billing aliases were not combined: %+v", projected)
	}
}

func TestProviderUsageEventForBillingPreservesExplicitZeroEvidence(t *testing.T) {
	unitKey := "duration"
	toolKey := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	projected := ProviderUsageEventForBilling(&types.UsageEvent{
		ProviderTokenEvidence: true,
		ExtraUsageUnits:       map[string]float64{unitKey: 0},
		ProviderIndependentUsageUnits: map[string]bool{
			unitKey: true,
		},
		ExtraBilling: map[string]types.ExtraBilling{
			toolKey: {CallCount: 0},
		},
		ProviderExtraBilling: map[string]bool{toolKey: true},
	})

	if !projected.ProviderTokenEvidence {
		t.Fatal("explicit zero token evidence was removed")
	}
	if value, exists := projected.ExtraUsageUnits[unitKey]; !exists || value != 0 || !projected.ProviderIndependentUsageUnits[unitKey] {
		t.Fatalf("explicit zero independent unit was removed: %+v", projected)
	}
	if value, exists := projected.ExtraBilling[toolKey]; !exists || value.CallCount != 0 || !projected.ProviderExtraBilling[toolKey] {
		t.Fatalf("explicit zero tool evidence was removed: %+v", projected)
	}
}

func TestProviderUsageEventForBillingRetainsCompleteTokenEvidence(t *testing.T) {
	projected := ProviderUsageEventForBilling(&types.UsageEvent{
		InputTokens:           7,
		OutputTokens:          3,
		TotalTokens:           10,
		ProviderTokenEvidence: true,
	})
	if projected.InputTokens != 7 || projected.OutputTokens != 3 || projected.TotalTokens != 10 {
		t.Fatalf("trusted token component was changed: %+v", projected)
	}
}
