package relay_util

import (
	"context"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	"one-api/types"
	"strings"
	"testing"
)

func TestSourceTierUpdatesReachActualTokenBilling(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&model.Price{}, &model.ModelInfo{}, &model.PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err = model.EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	oldDB, oldPricing := model.DB, model.PricingInstance
	publisher := &model.Pricing{Prices: make(map[string]*model.Price)}
	model.DB, model.PricingInstance = db, publisher
	t.Cleanup(func() { model.DB, model.PricingInstance = oldDB, oldPricing; pool, _ := db.DB(); pool.Close() })
	for _, cost := range []string{`{"input":2,"output":8,"tiers":[{"input":4,"tier":{"type":"context","size":200000}}]}`, `{"input":3,"output":8,"tiers":[{"input":9,"tier":{"type":"context","size":200000}}]}`} {
		catalog, err := model.ConvertModelsDevPrices(strings.NewReader(`{"openai":{"models":{"review-tiered":{"cost":` + cost + `}}}}`))
		if err != nil {
			t.Fatal(err)
		}
		source := []*model.Price{catalog.Candidates[0].Price}
		preview, err := model.PreviewPriceChange(context.Background(), source, model.PriceUpdateModeMerge)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = model.ApplyPriceChange(context.Background(), publisher, source, model.PriceUpdateModeMerge, preview.BaseVersion, preview.Digest); err != nil {
			t.Fatal(err)
		}
	}
	p, ok := publisher.FindExactPrice("review-tiered")
	if !ok {
		t.Fatal("missing price")
	}
	q := &Quota{modelName: p.Model, price: *p, groupName: useQuotaTestGroup(t), groupRatio: 1, inputRatio: p.Input, outputRatio: p.Output}
	got := mustEvaluateProviderQuota(t, q, &types.Usage{PromptTokens: 300000, ServiceTier: "default"})
	if got != 1350000 {
		t.Fatalf("got quota=%d; want source-consistent quota 1350000", got)
	}
	// No tier data from the next valid catalog must remove the multiplier.
	source := []*model.Price{{Model: p.Model, Type: model.TokensPriceType, Input: p.Input, Output: p.Output}}
	preview, err := model.PreviewPriceChange(context.Background(), source, model.PriceUpdateModeMerge)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = model.ApplyPriceChange(context.Background(), publisher, source, model.PriceUpdateModeMerge, preview.BaseVersion, preview.Digest); err != nil {
		t.Fatal(err)
	}
	p, _ = publisher.FindExactPrice(p.Model)
	q.price = *p
	if got := mustEvaluateProviderQuota(t, q, &types.Usage{PromptTokens: 300000, ServiceTier: "default"}); got != 450000 {
		t.Fatalf("cleared tiers quota=%d; want 450000", got)
	}
}
