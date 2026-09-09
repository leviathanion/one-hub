package relay_util

import (
	"math"
	"strings"

	"one-api/model"
	"one-api/types"
)

// tokenPartitionAdjustment contains only derived billing units.  It is never
// copied into provider evidence fields or ProviderTokenFields.
type tokenPartitionAdjustment struct {
	prompt     float64
	completion float64
}

// assessTokenEvidenceForPrice separates the provider's base token fact from
// the partition facts needed by the effective price.  A missing partition
// whose multiplier is one (or whose side has a zero effective price) cannot
// change the charge.  A declared exhaustive partition may derive its missing
// remainder only when all unknown buckets have the same multiplier.
func assessTokenEvidenceForPrice(usage *types.Usage, policy quotaPricePolicy) (PriceComponentStatus, tokenPartitionAdjustment) {
	if usage == nil || !usage.HasProviderBaseUsage() {
		return PriceComponentMissingEvidence, tokenPartitionAdjustment{}
	}

	required := normalizedTokenEvidenceKeys(usage.RequiredTokenExtraKeys)
	requiredSet := make(map[string]bool, len(required))
	for _, key := range required {
		requiredSet[key] = true
	}
	groups := usableTokenEvidenceGroups(usage.TokenExtraEvidenceGroups)
	grouped := make(map[string]bool)
	extraTokens := usage.GetExtraTokens()
	adjustment := tokenPartitionAdjustment{}

	for _, group := range groups {
		// A one-bucket declaration does not establish a useful remainder
		// equation; handle it using the ordinary missing-key rule below.
		if len(group) < 2 {
			continue
		}
		prompt, knownDimension := model.ExtraKeyIsPrompt[group[0]]
		if !knownDimension || !requiredSet[group[0]] {
			continue
		}
		validGroup := true
		for _, key := range group[1:] {
			keyPrompt, ok := model.ExtraKeyIsPrompt[key]
			if !ok || keyPrompt != prompt || grouped[key] || !requiredSet[key] {
				validGroup = false
				break
			}
		}
		if !validGroup || grouped[group[0]] {
			continue
		}

		baseTokens := usage.PromptTokens
		if !prompt {
			baseTokens = usage.CompletionTokens
		}
		knownSum, missing := tokenPartitionFacts(usage, group, extraTokens)
		if knownSum > int64(baseTokens) {
			return PriceComponentConflictingEvidence, tokenPartitionAdjustment{}
		}
		for _, key := range group {
			grouped[key] = true
		}

		if len(missing) == 0 {
			if knownSum != int64(baseTokens) {
				return PriceComponentConflictingEvidence, tokenPartitionAdjustment{}
			}
			continue
		}

		remaining := int64(baseTokens) - knownSum
		ratio, sameRatio, ratioValid := missingTokenPartitionRatio(policy, missing)
		if !ratioValid {
			return PriceComponentMissingEvidence, tokenPartitionAdjustment{}
		}
		sideRate := policy.price.GetOutput() * policy.groupRatio
		if prompt {
			sideRate = policy.price.GetInput() * policy.groupRatio
		}
		if remaining != 0 && sideRate != 0 && !sameRatio {
			return PriceComponentMissingEvidence, tokenPartitionAdjustment{}
		}
		if remaining != 0 {
			sideMultiplier := policy.outputMultiplier
			if prompt {
				sideMultiplier = policy.inputMultiplier
			}
			delta := float64(remaining) * (ratio - sideMultiplier)
			if math.IsNaN(delta) || math.IsInf(delta, 0) {
				return PriceComponentMissingEvidence, tokenPartitionAdjustment{}
			}
			if prompt {
				adjustment.prompt += delta
			} else {
				adjustment.completion += delta
			}
		}
	}

	for _, key := range required {
		if usage.ProviderTokenFields[key] || grouped[key] {
			continue
		}
		// Required keys outside the shared price dimension registry are
		// provider evidence sentinels.  They cannot be made harmless by the
		// default ratio of one: a producer uses such a key to say that a
		// separate, non-priceable fact (for example a cache partition
		// consistency check) failed.
		if _, knownDimension := model.ExtraKeyIsPrompt[key]; !knownDimension {
			return PriceComponentMissingEvidence, tokenPartitionAdjustment{}
		}
		prompt := model.ExtraKeyIsPrompt[key]
		ratio := policy.price.GetExtraRatio(key) * policy.rates.For(key, prompt)
		if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 {
			return PriceComponentMissingEvidence, tokenPartitionAdjustment{}
		}
		sideMultiplier := policy.outputMultiplier
		if prompt {
			sideMultiplier = policy.inputMultiplier
		}
		if ratio == sideMultiplier {
			continue
		}
		if prompt, knownDimension := model.ExtraKeyIsPrompt[key]; knownDimension {
			base := policy.price.GetOutput()
			if prompt {
				base = policy.price.GetInput()
			}
			if base*policy.groupRatio == 0 {
				continue
			}
		} else if effectiveTokenSideRate(policy, true) == 0 && effectiveTokenSideRate(policy, false) == 0 {
			continue
		}
		return PriceComponentMissingEvidence, tokenPartitionAdjustment{}
	}

	return PriceComponentPriceable, adjustment
}

func normalizedTokenEvidenceKeys(keys []string) []string {
	seen := make(map[string]bool, len(keys))
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, key)
	}
	return result
}

func usableTokenEvidenceGroups(groups [][]string) [][]string {
	if len(groups) == 0 {
		return nil
	}
	result := make([][]string, 0, len(groups))
	for _, group := range groups {
		keys := normalizedTokenEvidenceKeys(group)
		if len(keys) > 0 {
			result = append(result, keys)
		}
	}
	return result
}

func tokenPartitionFacts(usage *types.Usage, group []string, extraTokens map[string]int) (knownSum int64, missing []string) {
	for _, key := range group {
		if !usage.ProviderTokenFields[key] {
			missing = append(missing, key)
			continue
		}
		value := extraTokens[key]
		if value < 0 {
			return math.MaxInt64, nil
		}
		if int64(value) > math.MaxInt64-knownSum {
			return math.MaxInt64, nil
		}
		knownSum += int64(value)
	}
	return knownSum, missing
}

func missingTokenPartitionRatio(policy quotaPricePolicy, missing []string) (ratio float64, same bool, valid bool) {
	if len(missing) == 0 {
		return 1, true, true
	}
	same = true
	ratio = policy.price.GetExtraRatio(missing[0]) * policy.rates.For(missing[0], model.ExtraKeyIsPrompt[missing[0]])
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 {
		return 0, false, false
	}
	for _, key := range missing[1:] {
		candidate := policy.price.GetExtraRatio(key) * policy.rates.For(key, model.ExtraKeyIsPrompt[key])
		if math.IsNaN(candidate) || math.IsInf(candidate, 0) || candidate < 0 {
			return 0, false, false
		}
		if candidate != ratio {
			same = false
		}
	}
	if len(missing) == 1 {
		same = true
	}
	return ratio, same, true
}

func effectiveTokenSideRate(policy quotaPricePolicy, prompt bool) float64 {
	rate := policy.price.GetOutput()
	multiplier := policy.outputMultiplier
	if prompt {
		rate = policy.price.GetInput()
		multiplier = policy.inputMultiplier
	}
	rate *= policy.groupRatio * multiplier
	if math.IsNaN(rate) || math.IsInf(rate, 0) {
		return math.NaN()
	}
	return rate
}
