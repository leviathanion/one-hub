package model

import (
	"math"
	"one-api/common/config"
	"testing"
)

// Input=2 happens to hide the old output/input ratio bug at DollarRate=0.002.
func TestModelsDevIndependentAbsoluteNonDefaultInput(t *testing.T) {
	p, err := convertModelsDevPrice("asymmetric", []byte(`{"input":4,"output":12,"cache_read":0.4,"reasoning":18}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Input != 2 || p.Output != 6 {
		t.Fatalf("absolute independent rates input=%g output=%g", p.Input, p.Output)
	}
	extra := p.ExtraRatios.Data()
	if math.Abs(extra[config.UsageExtraCache]-0.1) > 1e-12 || extra[config.UsageExtraReasoning] != 1.5 {
		t.Fatalf("cache ratio=%+v", extra)
	}
}
