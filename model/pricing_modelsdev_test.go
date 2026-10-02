package model

import (
	"context"
	"errors"
	"gorm.io/datatypes"
	"io"
	"net/http"
	"one-api/common/config"
	"strings"
	"testing"
)

func TestModelsDevAbsolutePricesZeroMissingAndConflicts(t *testing.T) {
	catalog, err := ConvertModelsDevPrices(strings.NewReader(`{"openai":{"models":{"paid":{"cost":{"input":2,"output":12}},"zero":{"cost":{"input":0,"output":4}},"missing":{"cost":{"input":1}},"same":{"cost":{"input":4,"output":8}}}},"other":{"models":{"same":{"cost":{"input":0,"output":0}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Candidates) != 5 {
		t.Fatal(catalog)
	}
	for _, candidate := range catalog.Candidates {
		switch candidate.Model {
		case "paid":
			if candidate.Price.Input != 1 || candidate.Price.Output != 6 {
				t.Fatal(candidate)
			}
		case "zero":
			if candidate.Price == nil || candidate.Price.Input != 0 || candidate.Price.Output != 2 {
				t.Fatal(candidate)
			}
		case "missing":
			if candidate.Price != nil || candidate.Reason == "" {
				t.Fatal(candidate)
			}
		case "same":
			if !candidate.Conflict {
				t.Fatal(candidate)
			}
		}
	}
}
func TestModelsDevMultipleTiersAndCacheAbsoluteRates(t *testing.T) {
	p, err := convertModelsDevPrice("tiered", "anthropic", []byte(`{"input":2,"output":8,"cache_read":0.2,"cache_write":2.5,"tiers":[{"input":8,"output":16,"cache_read":0.8,"tier":{"type":"context","size":200}},{"input":4,"output":12,"cache_read":0.2,"tier":{"type":"context","size":100}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tokens                int
		in, out, cache, write float64
	}{{100, 1, 1, 1, 1}, {101, 2, 1.5, 1, 1}, {200, 2, 1.5, 1, 1}, {201, 4, 2, 4, 1}} {
		r := p.RateRules.Data().Evaluate(PriceRuleFacts{InputTokens: &tc.tokens})
		if r.Conflict || r.Missing || r.Input != tc.in || r.Output != tc.out || r.Extra[config.UsageExtraCacheReadInputTokens] != tc.cache || r.Extra[config.UsageExtraCacheWrite] != tc.write || r.Extra[config.UsageExtraEphemeral1hInputTokens] != tc.write {
			t.Fatalf("tokens=%d result=%+v", tc.tokens, r)
		}
	}
}
func TestModelsDevRejectsUnrepresentablePrices(t *testing.T) {
	for _, raw := range []string{`{"input":0,"output":2,"cache_read":1}`, `{"input":2,"output":2,"image":5}`, `{"input":2,"output":2,"tiers":[{"input":4,"image":1,"tier":{"type":"context","size":100}}]}`, `{"input":-1,"output":2}`, `{"input":2,"output":2,"context_over_200k":{"input":4}}`, `{"input":2,"output":2,"tiers":[{"tier":{"type":"unknown","size":100}}]}`} {
		if _, err := convertModelsDevPrice("bad", "openai", []byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := ConvertModelsDevPrices(strings.NewReader(`{} {}`)); err == nil {
		t.Fatal("accepted trailing JSON")
	}
	if _, err := ConvertModelsDevPrices(strings.NewReader(strings.Repeat(" ", int(MaxRemotePriceCatalogBytes)+1))); err == nil {
		t.Fatal("accepted oversized response")
	}
}

type modelsDevRoundTrip func(*http.Request) (*http.Response, error)

func (f modelsDevRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestModelsDevFixedSourceAndFailures(t *testing.T) {
	for _, status := range []int{200, 503} {
		client := &http.Client{Transport: modelsDevRoundTrip(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() != ModelsDevURL || req.Method != "GET" {
				t.Fatal(req)
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"p":{"models":{}}}`)), Header: make(http.Header)}, nil
		})}
		_, err := fetchModelsDevPrices(context.Background(), client)
		if (err != nil) != (status != 200) {
			t.Fatal(err)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Transport: modelsDevRoundTrip(func(req *http.Request) (*http.Response, error) { return nil, req.Context().Err() })}
	if _, err := fetchModelsDevPrices(canceled, client); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestModelsDevMergePreservesUnselectedLockedAndLocalRules(t *testing.T) {
	db, publisher := setupVersionedPricingTest(t)
	rules := datatypes.NewJSONType(PriceRateRules{Version: 2, ServiceTier: []PriceRateRule{{ID: "local", When: PriceRuleCondition{ServiceTier: []string{"flex"}}, Multipliers: PriceRateMultiplier{All: ptrModelsDev(0.5)}}}})
	extras := datatypes.NewJSONType(map[string]float64{config.UsageExtraInputAudio: 7})
	if err := db.Create(&[]*Price{{Model: "keep", Type: TokensPriceType, Input: 1, Output: 2}, {Model: "local", Type: TokensPriceType, ChannelType: 99, Input: 1, Output: 2, RateRules: &rules, ExtraRatios: &extras}, {Model: "locked", Type: TokensPriceType, Input: 1, Output: 2, Locked: true}}).Error; err != nil {
		t.Fatal(err)
	}
	catalog, err := ConvertModelsDevPrices(strings.NewReader(`{"openai":{"models":{"local":{"cost":{"input":4,"output":8,"tiers":[{"input":8,"tier":{"type":"context","size":100}}]}},"locked":{"cost":{"input":4,"output":8}},"new":{"cost":{"input":0,"output":4}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	source := []*Price{}
	for _, c := range catalog.Candidates {
		source = append(source, c.Price)
	}
	preview, err := PreviewPriceChange(context.Background(), source, PriceUpdateModeMerge)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ApplyPriceChange(context.Background(), publisher, source, PriceUpdateModeMerge, preview.BaseVersion, preview.Digest)
	if err != nil || version != 2 {
		t.Fatal(version, err)
	}
	local, _ := publisher.FindExactPrice("local")
	if local.ChannelType != 99 || local.Input != 2 || len(local.RateRules.Data().ServiceTier) != 1 || len(local.RateRules.Data().LongContext) != 0 || local.ExtraRatios.Data()[config.UsageExtraInputAudio] != 7 {
		t.Fatal(local)
	}
	if _, ok := publisher.FindExactPrice("keep"); !ok {
		t.Fatal("unselected deleted")
	}
	locked, _ := publisher.FindExactPrice("locked")
	if locked.Input != 1 || !locked.Locked {
		t.Fatal(locked)
	}
	if _, err := ApplyPriceChange(context.Background(), publisher, source, PriceUpdateModeMerge, preview.BaseVersion, preview.Digest); !errors.Is(err, ErrPublicationVersionConflict) {
		t.Fatal(err)
	}
	preview, err = PreviewPriceChange(context.Background(), source, PriceUpdateModeMerge)
	if err != nil || len(preview.Plan.Changes) != 1 {
		t.Fatal(preview, err)
	} // locked proposal remains visible
	version, err = ApplyPriceChange(context.Background(), publisher, source, PriceUpdateModeMerge, preview.BaseVersion, preview.Digest)
	if err != nil || version != 2 {
		t.Fatal(version, err)
	}
}
func ptrModelsDev(v float64) *float64 { return &v }
