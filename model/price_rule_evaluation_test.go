package model

import (
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"one-api/common/utils"
	"testing"
	"time"
)

func TestRuleCalendarPriorityAndMeterOverrides(t *testing.T) {
	var rules PriceRateRules
	require.NoError(t, json.Unmarshal([]byte(`{"version":2,"speed":[{"id":"fast","when":{"speed":["fast"]},"multipliers":{"all":2}}],"schedule":{"rules":[{"id":"weekend-night","when":{"weekdays":[6,7],"time":{"start":"23:00","end":"07:00"}},"multipliers":{"all":0.4,"extra_multipliers":{"cached_read_tokens":1}}},{"id":"night","when":{"time":{"start":"23:00","end":"07:00"}},"multipliers":{"all":0.5}},{"id":"weekend","when":{"weekdays":[6,7]},"multipliers":{"all":0.8}}],"timezone":"Asia/Shanghai"}}`), &rules))
	for _, tc := range []struct {
		at           string
		input, cache float64
		id           string
	}{
		{"2026-09-11T23:30:00+08:00", 1, 1, "night"},
		{"2026-09-12T00:30:00+08:00", 0.8, 2, "weekend-night"},
		{"2026-09-12T07:00:00+08:00", 1.6, 1.6, "weekend"},
	} {
		t.Run(tc.at, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, tc.at)
			require.NoError(t, err)
			got := rules.Evaluate(PriceRuleFacts{Speed: "fast", StartedAt: at})
			require.False(t, got.Missing)
			assert.Equal(t, tc.input, got.Input)
			assert.Equal(t, tc.cache, got.Extra["cached_read_tokens"])
			require.Len(t, got.Matches, 2)
			assert.Equal(t, tc.id, got.Matches[1].ID)
		})
	}
}

func TestRuleMissingFactsCannotSkipEarlierPrice(t *testing.T) {
	rules := PriceRateRules{Version: 2, Speed: []PriceRateRule{
		{ID: "fast", When: PriceRuleCondition{Speed: []string{"fast"}}, Multipliers: PriceRateMultiplier{All: utils.GetPointer(2.0)}},
		{ID: "fallback", When: PriceRuleCondition{Speed: []string{"standard"}}, Multipliers: PriceRateMultiplier{All: utils.GetPointer(1.0)}},
	}}
	assert.True(t, rules.Evaluate(PriceRuleFacts{}).Missing)
	assert.True(t, rules.Evaluate(PriceRuleFacts{Speed: "fast", SpeedConflict: true}).Conflict)
	got := rules.Evaluate(PriceRuleFacts{Speed: "standard"})
	assert.False(t, got.Missing)
	assert.Equal(t, 1.0, got.Input)
	rules.Speed[0].Multipliers.All = utils.GetPointer(1.0)
	got = rules.Evaluate(PriceRuleFacts{})
	assert.False(t, got.Missing)
	assert.Empty(t, got.Matches)
	// 长度已知不匹配时，无需依赖该区间附带的速度条件。
	rules = PriceRateRules{Version: 2, LongContext: []PriceRateRule{{ID: "long", When: PriceRuleCondition{InputTokens: &PriceTokenRange{GT: utils.GetPointer(200000)}, Speed: []string{"standard"}}, Multipliers: PriceRateMultiplier{All: utils.GetPointer(2.0)}}}}
	assert.False(t, rules.Evaluate(PriceRuleFacts{InputTokens: utils.GetPointer(100)}).Missing)
}

func TestRuleLengthBoundaryAndSpeedAreSeparate(t *testing.T) {
	rules := PriceRateRules{Version: 2, LongContext: []PriceRateRule{{ID: "long", When: PriceRuleCondition{Speed: []string{"standard"}, InputTokens: &PriceTokenRange{GT: utils.GetPointer(200000)}}, Multipliers: PriceRateMultiplier{Input: utils.GetPointer(2.0)}}}}
	for _, tc := range []struct {
		length int
		speed  string
		want   float64
	}{{200000, "standard", 1}, {200001, "standard", 2}, {200001, "fast", 1}} {
		got := rules.Evaluate(PriceRuleFacts{Speed: tc.speed, InputTokens: &tc.length})
		require.False(t, got.Missing)
		assert.Equal(t, tc.want, got.Input)
	}
}

func TestRuleSchemaPreservesZeroAndRejectsUnsafeConfiguration(t *testing.T) {
	var rules PriceRateRules
	require.NoError(t, json.Unmarshal([]byte(`{"version":2,"schedule":{"rules":[{"id":"free","when":{},"multipliers":{"all":0,"extra_multipliers":{"cached_read_tokens":1}}}]}}`), &rules))
	cloned := ClonePriceRateRules(rules)
	*cloned.Schedule.Rules[0].Multipliers.All = 0.5
	got := rules.Evaluate(PriceRuleFacts{})
	assert.Zero(t, got.Input)
	assert.Equal(t, 1.0, got.Extra["cached_read_tokens"])
	for _, raw := range []string{
		`{"flex":{"input":0.5,"output":0.5}}`,
		`{"version":2,"speed":[{"id":"x","when":{},"multipliers":{"all":null}}]}`,
		`{"version":2,"speed":[{"id":"x","when":{},"multipliers":{"all":-1}}]}`,
		`{"version":2,"speed":[{"id":"x","when":{"speed":[]},"multipliers":{}}]}`,
		`{"version":2,"schedule":{"rules":[{"id":"x","when":{"weekdays":[6]},"multipliers":{}}]}}`,
		`{"version":2,"schedule":{"rules":[{"id":"x","when":{"time":{"start":"07:00","end":"07:00"}},"multipliers":{}}],"timezone":"Asia/Shanghai"}}`,
		`{"version":2,"speed":[{"id":"x","when":{},"multipliers":{}},{"id":"y","when":{"speed":["fast"]},"multipliers":{}}]}`,
		`{"version":2,"speed":[{"id":"x","when":{},"multipliers":{"extra_multipliers":{"unknown":1}}}]}`,
	} {
		t.Run(raw, func(t *testing.T) { var r PriceRateRules; assert.Error(t, json.Unmarshal([]byte(raw), &r)) })
	}
}

func TestRuleCalendarDSTUsesOperationInstant(t *testing.T) {
	rules := PriceRateRules{Version: 2, Schedule: &PriceScheduleRules{Timezone: "America/New_York", Rules: []PriceRateRule{{ID: "night", When: PriceRuleCondition{Time: &PriceTimeRange{Start: "01:00", End: "02:00"}}, Multipliers: PriceRateMultiplier{All: utils.GetPointer(0.5)}}}}}
	for _, raw := range []string{"2026-11-01T01:30:00-04:00", "2026-11-01T01:30:00-05:00"} {
		at, err := time.Parse(time.RFC3339, raw)
		require.NoError(t, err)
		assert.Equal(t, 0.5, rules.Evaluate(PriceRuleFacts{StartedAt: at}).Input)
	}
}
