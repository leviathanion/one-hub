package model

import (
	"testing"

	"one-api/common/config"

	"gorm.io/datatypes"
)

func issue030PriceWithRatios(ratios map[string]float64) *Price {
	price := &Price{Model: "claude-i030", Type: TokensPriceType, Input: 1, Output: 1}
	if ratios != nil {
		extra := datatypes.NewJSONType(ratios)
		price.ExtraRatios = &extra
	}
	return price
}

func TestI030ClaudeTTLPriceUsesStrictFallbackPriority(t *testing.T) {
	tests := []struct {
		name   string
		ratio  map[string]float64
		want5m float64
		want1h float64
	}{
		{name: "内置默认", want5m: 1.25, want1h: 2},
		{name: "通用显式零值", ratio: map[string]float64{config.UsageExtraCachedWrite: 0}, want5m: 0, want1h: 0},
		{name: "通用显式倍率", ratio: map[string]float64{config.UsageExtraCachedWrite: 0.5}, want5m: 0.5, want1h: 0.5},
		{name: "TTL显式值优先", ratio: map[string]float64{
			config.UsageExtraCachedWrite:        2,
			config.UsageExtraClaudeCacheWrite5m: 0.25,
			config.UsageExtraClaudeCacheWrite1h: 0,
		}, want5m: 0.25, want1h: 0},
		{name: "仅显式一小时", ratio: map[string]float64{
			config.UsageExtraCachedWrite:        0.5,
			config.UsageExtraClaudeCacheWrite1h: 0.75,
		}, want5m: 0.5, want1h: 0.75},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			price := issue030PriceWithRatios(test.ratio)
			if got := price.GetExtraRatio(config.UsageExtraClaudeCacheWrite5m); got != test.want5m {
				t.Fatalf("5m ratio=%v want=%v", got, test.want5m)
			}
			if got := price.GetExtraRatio(config.UsageExtraClaudeCacheWrite1h); got != test.want1h {
				t.Fatalf("1h ratio=%v want=%v", got, test.want1h)
			}
		})
	}
}

func TestI030NonClaudePriceKeysRemainIndependent(t *testing.T) {
	price := issue030PriceWithRatios(map[string]float64{config.UsageExtraCachedWrite: 2})
	if got := price.GetExtraRatio(config.UsageExtraCacheWrite); got != 1.25 {
		t.Fatalf("OpenAI cache-write key inherited Claude generic ratio=%v", got)
	}
	if got := price.GetExtraRatio(config.UsageExtraCachedRead); got != 0.1 {
		t.Fatalf("cache-read key inherited Claude generic ratio=%v", got)
	}
	if got := price.GetExtraRatio(config.UsageExtraClaudeCacheWrite5m); got != 2 {
		t.Fatalf("Claude TTL did not use explicit generic fallback=%v", got)
	}
}
