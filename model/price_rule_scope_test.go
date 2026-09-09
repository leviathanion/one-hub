package model

import (
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"one-api/common/utils"
	"slices"
	"testing"
)

func TestPriceRulesRejectCrossDimensionConditionsAndNames(t *testing.T) {
	for _, raw := range []string{
		`{"version":2,"service_tier":[{"id":"x","when":{"service_tier":["flex"],"speed":["fast"]},"multipliers":{}}]}`,
		`{"version":2,"speed":[{"id":"x","when":{"speed":["fast"],"input_tokens":{"gt":1}},"multipliers":{}}]}`,
		`{"version":2,"schedule":{"timezone":"UTC","rules":[{"id":"x","when":{"speed":["fast"],"weekdays":[6]},"multipliers":{}}]}}`,
		`{"version":2,"timezone":"UTC"}`,
		`{"version":2,"service_tier":[{"id":"x","name":"not needed","when":{"service_tier":["flex"]},"multipliers":{}}]}`,
	} {
		t.Run(raw, func(t *testing.T) { var rules PriceRateRules; assert.Error(t, json.Unmarshal([]byte(raw), &rules)) })
	}
}

func TestPriceRulesRejectOverlappingEnumsAndRanges(t *testing.T) {
	for _, raw := range []string{
		`{"version":2,"service_tier":[{"id":"a","when":{"service_tier":["flex","priority"]},"multipliers":{}},{"id":"b","when":{"service_tier":["priority"]},"multipliers":{}}]}`,
		`{"version":2,"speed":[{"id":"a","when":{"speed":["fast"]},"multipliers":{}},{"id":"b","when":{"speed":["fast"]},"multipliers":{}}]}`,
		`{"version":2,"long_context":[{"id":"a","when":{"input_tokens":{"gt":100}},"multipliers":{}},{"id":"b","when":{"input_tokens":{"gt":200}},"multipliers":{}}]}`,
	} {
		var rules PriceRateRules
		assert.Error(t, json.Unmarshal([]byte(raw), &rules))
	}
	var rules PriceRateRules
	require.NoError(t, json.Unmarshal([]byte(`{"version":2,"long_context":[{"id":"a","when":{"input_tokens":{"gt":100,"lte":200}},"multipliers":{"all":2}},{"id":"b","when":{"input_tokens":{"gt":200}},"multipliers":{"all":3}}]}`), &rules))
	assert.Equal(t, 2.0, rules.Evaluate(PriceRuleFacts{InputTokens: utils.GetPointer(200)}).Input)
	assert.Equal(t, 3.0, rules.Evaluate(PriceRuleFacts{InputTokens: utils.GetPointer(201)}).Input)
	slices.Reverse(rules.LongContext)
	assert.Equal(t, 2.0, rules.Evaluate(PriceRuleFacts{InputTokens: utils.GetPointer(200)}).Input)
	assert.Equal(t, 3.0, rules.Evaluate(PriceRuleFacts{InputTokens: utils.GetPointer(201)}).Input)
}
