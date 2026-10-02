package model

import (
	"context"
	"testing"
)

func TestSourceNoExtraSourceDoesNotCreateChange(t *testing.T) {
	db, publisher := setupVersionedPricingTest(t)
	source, err := convertModelsDevPrice("unchanged", "openai", []byte(`{"input":2,"output":8}`))
	if err != nil {
		t.Fatal(err)
	}
	existing := *source
	existing.ExtraRatios = nil
	if err := db.Create(&existing).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		preview, err := PreviewPriceChange(context.Background(), []*Price{source}, PriceUpdateModeUpdate)
		if err != nil {
			t.Fatal(err)
		}
		if len(preview.Plan.Changes) != 0 {
			t.Fatalf("round %d: absent source extras created fake changes: %+v", i, preview.Plan.Changes)
		}
		if _, err := ApplyPriceChange(context.Background(), publisher, []*Price{source}, PriceUpdateModeUpdate, preview.BaseVersion, preview.Digest); err != nil {
			t.Fatal(err)
		}
	}
}
