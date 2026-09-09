package model

import (
	"context"
	"errors"
	"one-api/common/utils"
	"testing"

	"gorm.io/datatypes"
)

func TestPriceChangePreviewIsCompleteAndPreservesRulesFromDatabaseBase(t *testing.T) {
	db, _ := setupVersionedPricingTest(t)
	rules := datatypes.NewJSONType(PriceRateRules{Version: 2, ServiceTier: []PriceRateRule{{ID: "flex", When: PriceRuleCondition{ServiceTier: []string{"flex"}}, Multipliers: PriceRateMultiplier{Input: utils.GetPointer(float64(0.5)), Output: utils.GetPointer(float64(0.75))}}}})
	extra := datatypes.NewJSONType(map[string]float64{"cache": 0.4})
	current := []*Price{
		{Model: "changed", Type: TokensPriceType, ChannelType: 1, Input: 1, Output: 2, ExtraRatios: &extra, RateRules: &rules},
		{Model: "delete-me", Type: TokensPriceType, ChannelType: 1, Input: 3, Output: 4},
		{Model: "locked-change", Type: TokensPriceType, ChannelType: 1, Input: 5, Output: 6, Locked: true},
		{Model: "locked-delete", Type: TokensPriceType, ChannelType: 1, Input: 7, Output: 8, Locked: true},
	}
	if err := db.Create(&current).Error; err != nil {
		t.Fatal(err)
	}
	newExtra := datatypes.NewJSONType(map[string]float64{"cache": 0.2, "reasoning": 1.5})
	source := []*Price{
		{Model: "new", Type: TokensPriceType, ChannelType: 2, Input: 9, Output: 10},
		{Model: "changed", Type: TokensPriceType, ChannelType: 1, Input: 11, Output: 12, ExtraRatios: &newExtra},
		{Model: "locked-change", Type: TokensPriceType, ChannelType: 1, Input: 50, Output: 60, Locked: false},
	}

	preview, err := PreviewPriceChange(context.Background(), source, PriceUpdateModeOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if preview.BaseVersion != 1 || preview.Digest == "" {
		t.Fatalf("invalid preview identity: %+v", preview)
	}
	byModel := make(map[string]PriceChange, len(preview.Plan.Changes))
	for _, change := range preview.Plan.Changes {
		byModel[change.Model] = change
	}
	if got := byModel["new"].Action; got != PriceChangeAdd {
		t.Fatalf("new action=%q", got)
	}
	if got := byModel["delete-me"].Action; got != PriceChangeDelete {
		t.Fatalf("delete action=%q", got)
	}
	if got := byModel["locked-change"].Action; got != PriceChangeLocked || byModel["locked-change"].After == nil {
		t.Fatalf("locked update omitted proposed after policy: %+v", byModel["locked-change"])
	}
	if got := byModel["locked-delete"].Action; got != PriceChangeLocked || byModel["locked-delete"].After != nil {
		t.Fatalf("locked deletion not represented: %+v", byModel["locked-delete"])
	}
	changed := byModel["changed"]
	if changed.Action != PriceChangeUpdate || changed.Before == nil || changed.After == nil {
		t.Fatalf("changed policy is incomplete: %+v", changed)
	}
	if changed.Before.ExtraRatios == nil || (*changed.Before.ExtraRatios)["cache"] != 0.4 || changed.After.ExtraRatios == nil || (*changed.After.ExtraRatios)["cache"] != 0.2 {
		t.Fatalf("extra_ratios before/after missing: %+v", changed)
	}
	if changed.Before.RateRules == nil || changed.After.RateRules == nil || testRuleMultiplier(*changed.After.RateRules, "flex") == nil || *testRuleMultiplier(*changed.After.RateRules, "flex").Input != 0.5 {
		t.Fatalf("omitted remote rate_rules did not preserve database base: %+v", changed)
	}
}

func TestApplyPriceChangeRecomputesDigestAndAppliesPreview(t *testing.T) {
	db, pricing := setupVersionedPricingTest(t)
	rules := datatypes.NewJSONType(PriceRateRules{Version: 2, ServiceTier: []PriceRateRule{{ID: "flex", When: PriceRuleCondition{ServiceTier: []string{"flex"}}, Multipliers: PriceRateMultiplier{Input: utils.GetPointer(float64(0.5)), Output: utils.GetPointer(float64(0.5))}}}})
	if err := db.Create(&[]*Price{
		{Model: "changed", Type: TokensPriceType, Input: 1, Output: 2, RateRules: &rules},
		{Model: "delete-me", Type: TokensPriceType, Input: 3, Output: 4},
		{Model: "locked", Type: TokensPriceType, Input: 5, Output: 6, Locked: true},
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := pricing.Init(); err != nil {
		t.Fatal(err)
	}
	emptyRules := datatypes.NewJSONType(PriceRateRules{})
	source := []*Price{
		{Model: "changed", Type: TokensPriceType, Input: 10, Output: 20, RateRules: &emptyRules},
		{Model: "new", Type: TimesPriceType, Input: 7, Output: 7},
	}
	preview, err := PreviewPriceChange(context.Background(), source, PriceUpdateModeOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	newVersion, err := ApplyPriceChange(context.Background(), pricing, source, PriceUpdateModeOverwrite, preview.BaseVersion, preview.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if newVersion != 2 || pricing.PublishedVersion() != 2 {
		t.Fatalf("version=%d published=%d", newVersion, pricing.PublishedVersion())
	}
	var changed Price
	if err := db.Where("model = ?", "changed").Take(&changed).Error; err != nil {
		t.Fatal(err)
	}
	if changed.Input != 10 || changed.RateRules == nil || testRuleMultiplier(changed.RateRules.Data(), "flex") != nil {
		t.Fatalf("explicit empty rate_rules did not clear policy: %+v", changed)
	}
	var count int64
	if err := db.Model(&Price{}).Where("model = ?", "delete-me").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("model was not deleted: count=%d err=%v", count, err)
	}
	if _, ok := pricing.FindExactPrice("new"); !ok {
		t.Fatal("new policy was not published")
	}
	if locked, ok := pricing.FindExactPrice("locked"); !ok || !locked.Locked {
		t.Fatal("locked policy was not retained")
	}
}

func TestApplyPriceChangeRejectsStaleVersionAndDigestWithoutMutation(t *testing.T) {
	db, pricing := setupVersionedPricingTest(t)
	if err := db.Create(&Price{Model: "existing", Type: TokensPriceType, Input: 1, Output: 2}).Error; err != nil {
		t.Fatal(err)
	}
	if err := pricing.Init(); err != nil {
		t.Fatal(err)
	}
	source := []*Price{{Model: "new", Type: TokensPriceType, Input: 3, Output: 4}}
	preview, err := PreviewPriceChange(context.Background(), source, PriceUpdateModeAdd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyPriceChange(context.Background(), pricing, source, PriceUpdateModeAdd, preview.BaseVersion, "00"); !errors.Is(err, ErrPriceChangePlanMismatch) {
		t.Fatalf("digest mismatch returned %v", err)
	}
	var count int64
	if err := db.Model(&Price{}).Where("model = ?", "new").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("digest mismatch mutated rows: count=%d err=%v", count, err)
	}
	if _, err := BumpPublicationVersionCAS(context.Background(), db, PublicationOwnerPrice, preview.BaseVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyPriceChange(context.Background(), pricing, source, PriceUpdateModeAdd, preview.BaseVersion, preview.Digest); !errors.Is(err, ErrPublicationVersionConflict) {
		t.Fatalf("stale preview returned %v", err)
	}
	if err := db.Model(&Price{}).Where("model = ?", "new").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("stale preview mutated rows: count=%d err=%v", count, err)
	}
}

func TestPriceChangeDigestUsesCanonicalSourceOrder(t *testing.T) {
	_, _ = setupVersionedPricingTest(t)
	first := []*Price{
		{Model: "b", Type: TokensPriceType, Input: 1, Output: 2},
		{Model: "a", Type: TokensPriceType, Input: 3, Output: 4},
	}
	second := []*Price{first[1], first[0]}
	left, err := PreviewPriceChange(context.Background(), first, PriceUpdateModeOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	right, err := PreviewPriceChange(context.Background(), second, PriceUpdateModeOverwrite)
	if err != nil {
		t.Fatal(err)
	}
	if left.Digest != right.Digest {
		t.Fatalf("source order changed digest: %s != %s", left.Digest, right.Digest)
	}
}
