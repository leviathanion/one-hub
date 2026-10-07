package model

import (
	"context"
	"encoding/json"
	"github.com/spf13/viper"
	"net/http"
	"net/http/httptest"
	"testing"

	"gorm.io/datatypes"
)

func TestSourcePriceUpdatesReplaceOrClearContextTiers(t *testing.T) {
	for _, mode := range []PriceUpdateMode{PriceUpdateModeMerge, PriceUpdateModeUpdate, PriceUpdateModeOverwrite} {
		t.Run(string(mode), func(t *testing.T) {
			db, publisher := setupVersionedPricingTest(t)
			tiered := func(input, tier float64) *Price {
				raw, _ := json.Marshal(map[string]any{"input": input, "output": 8, "tiers": []any{map[string]any{"input": tier, "tier": map[string]any{"type": "context", "size": 200000}}}})
				p, err := convertModelsDevPrice("tiered", raw)
				if err != nil {
					t.Fatal(err)
				}
				return p
			}
			original := tiered(2, 4)
			// Non-context local policy must survive sources that do not supply it.
			rules := original.RateRules.Data()
			rules.ServiceTier = []PriceRateRule{{ID: "flex", When: PriceRuleCondition{ServiceTier: []string{"flex"}}, Multipliers: PriceRateMultiplier{All: ptrModelsDev(0.5)}}}
			encoded := datatypes.NewJSONType(rules)
			original.RateRules = &encoded
			if err := db.Create(original).Error; err != nil {
				t.Fatal(err)
			}
			source := tiered(3, 9)
			if mode != PriceUpdateModeMerge {
				r := source.RateRules.Data()
				r.ServiceTier = rules.ServiceTier
				v := datatypes.NewJSONType(r)
				source.RateRules = &v
			}
			for _, step := range []struct {
				source              *Price
				wantInput, wantTier float64
			}{
				{source, 1.5, 3},
				{tiered(3, 12), 1.5, 4},
				{&Price{Model: "tiered", Type: TokensPriceType, Input: 1.5, Output: 4}, 1.5, 1},
			} {
				if mode != PriceUpdateModeMerge && step.source.RateRules != nil {
					r := step.source.RateRules.Data()
					r.ServiceTier = rules.ServiceTier
					v := datatypes.NewJSONType(r)
					step.source.RateRules = &v
				}
				preview, err := PreviewPriceChange(context.Background(), []*Price{step.source}, mode)
				if err != nil {
					t.Fatal(err)
				}
				if len(preview.Plan.Changes) != 1 || preview.Plan.Changes[0].Action != PriceChangeUpdate {
					t.Fatalf("tier-only changes/deletion must be visible: %+v", preview.Plan)
				}
				if _, err := ApplyPriceChange(context.Background(), publisher, []*Price{step.source}, mode, preview.BaseVersion, preview.Digest); err != nil {
					t.Fatal(err)
				}
				got, _ := publisher.FindExactPrice("tiered")
				tokens := 300000
				result := got.EffectiveRateRules().Evaluate(PriceRuleFacts{InputTokens: &tokens, ServiceTier: "default"})
				if got.Input != step.wantInput || result.Input != step.wantTier {
					t.Fatalf("price=%+v result=%+v", got, result)
				}
				if len(got.RateRules.Data().ServiceTier) != 1 {
					t.Fatal("unrelated local rules cleared")
				}
			}
		})
	}
}

func TestSourceTierRemovalPreservesLockedAndUnselectedModels(t *testing.T) {
	for _, mode := range []PriceUpdateMode{PriceUpdateModeMerge, PriceUpdateModeUpdate, PriceUpdateModeAdd} {
		t.Run(string(mode), func(t *testing.T) {
			db, publisher := setupVersionedPricingTest(t)
			for _, name := range []string{"selected", "locked", "unselected"} {
				p, err := convertModelsDevPrice(name, []byte(`{"input":2,"output":8,"tiers":[{"input":4,"tier":{"type":"context","size":200000}}]}`))
				if err != nil {
					t.Fatal(err)
				}
				p.Locked = name == "locked"
				if err := db.Create(p).Error; err != nil {
					t.Fatal(err)
				}
			}
			source := []*Price{{Model: "selected", Type: TokensPriceType, Input: 1, Output: 4}, {Model: "locked", Type: TokensPriceType, Input: 1, Output: 4}}
			preview, err := PreviewPriceChange(context.Background(), source, mode)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ApplyPriceChange(context.Background(), publisher, source, mode, preview.BaseVersion, preview.Digest); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"selected", "locked", "unselected"} {
				p, _ := publisher.FindExactPrice(name)
				want := 1
				if name == "selected" && mode != PriceUpdateModeAdd {
					want = 0
				}
				if len(p.RateRules.Data().LongContext) != want {
					t.Fatalf("%s tiers=%+v", name, p.RateRules)
				}
			}
		})
	}
}

func TestFailedAutomaticCatalogDoesNotClearExistingTiers(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"fetch failure", "unavailable", http.StatusServiceUnavailable},
		{"invalid catalog", `[{"model":"tiered","type":"tokens","input":-1,"output":4}]`, http.StatusOK},
		{"malformed catalog", `[{`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, publisher := setupVersionedPricingTest(t)
			oldPublisher := PricingInstance
			PricingInstance = publisher
			t.Cleanup(func() { PricingInstance = oldPublisher })
			p, err := convertModelsDevPrice("tiered", []byte(`{"input":2,"output":8,"tiers":[{"input":4,"tier":{"type":"context","size":200000}}]}`))
			if err != nil {
				t.Fatal(err)
			}
			if err = db.Create(p).Error; err != nil {
				t.Fatal(err)
			}
			before, _ := ReadPublicationVersion(context.Background(), db, PublicationOwnerPrice)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			oldURL, oldMode := viper.GetString("update_price_service"), viper.GetString("auto_price_updates_mode")
			viper.Set("update_price_service", server.URL)
			viper.Set("auto_price_updates_mode", "update")
			t.Cleanup(func() { viper.Set("update_price_service", oldURL); viper.Set("auto_price_updates_mode", oldMode) })
			if err = UpdatePriceByPriceService(); err == nil {
				t.Fatal("bad source accepted")
			}
			var got Price
			if err = db.Where("model = ?", "tiered").First(&got).Error; err != nil {
				t.Fatal(err)
			}
			after, _ := ReadPublicationVersion(context.Background(), db, PublicationOwnerPrice)
			if got.Input != p.Input || got.RateRules == nil || len(got.RateRules.Data().LongContext) != 1 || after != before {
				t.Fatalf("failed source changed price/version: %+v %d->%d", got, before, after)
			}
		})
	}
}
