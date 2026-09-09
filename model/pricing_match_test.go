package model

import (
	"reflect"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/internal/testutil/sqlitetest"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestPricingWildcardUsesLongestStablePrefix(t *testing.T) {
	patterns := []string{"gpt-*", "gpt-5.6-*", "gpt-5*", "claude-*"}
	sortPriceMatchPatterns(patterns)
	wantOrder := []string{"gpt-5.6-*", "claude-*", "gpt-5*", "gpt-*"}
	if !reflect.DeepEqual(patterns, wantOrder) {
		t.Fatalf("wildcard order=%v, want %v", patterns, wantOrder)
	}

	pricing := &Pricing{
		Prices: map[string]*Price{
			"gpt-*":     {Model: "gpt-*", Input: 1},
			"gpt-5*":    {Model: "gpt-5*", Input: 2},
			"gpt-5.6-*": {Model: "gpt-5.6-*", Input: 3},
		},
		Match: patterns,
	}
	price, ok := pricing.FindPrice("gpt-5.6-sol")
	if !ok || price.Model != "gpt-5.6-*" {
		t.Fatalf("matched price=%+v ok=%t, want longest prefix", price, ok)
	}
	price, ok = pricing.FindPrice("gpt-5.7")
	if !ok || price.Model != "gpt-5*" {
		t.Fatalf("matched price=%+v ok=%t, want gpt-5*", price, ok)
	}
}

func TestPricePolicyRejectsAmbiguousWildcardSyntax(t *testing.T) {
	for _, modelName := range []string{"gpt**", "g*pt", "**"} {
		price := &Price{Model: modelName, Type: TokensPriceType, Input: 1, Output: 2}
		if err := price.prepareForPersistence(); err == nil {
			t.Fatalf("ambiguous wildcard %q was accepted", modelName)
		}
	}
	for _, modelName := range []string{"gpt*", "*", "gpt-5"} {
		price := &Price{Model: modelName, Type: TokensPriceType, Input: 1, Output: 2}
		if err := price.prepareForPersistence(); err != nil {
			t.Fatalf("valid price policy %q rejected: %v", modelName, err)
		}
	}
}

func TestPricePolicyRejectsModelBeyondDatabaseLimit(t *testing.T) {
	price := &Price{Model: strings.Repeat("模", config.MaxPricingModelRunes+1), Type: TokensPriceType}
	if err := price.prepareForPersistence(); err == nil {
		t.Fatal("overlong price model was accepted")
	}
}

func TestBatchDeletePricesRebuildsWildcardMatch(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("open sql connection: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&Price{}, &ModelInfo{}, &PublicationVersion{}); err != nil {
		t.Fatalf("migrate pricing tables: %v", err)
	}
	if err := EnsurePublicationVersionRows(db); err != nil {
		t.Fatalf("initialize price version: %v", err)
	}
	if err := db.Create([]*Price{
		{Model: "gpt-*", Type: TokensPriceType, Input: 2, Output: 2},
		{Model: "gpt-5*", Type: TokensPriceType, Input: 5, Output: 5},
	}).Error; err != nil {
		t.Fatalf("insert prices: %v", err)
	}
	originalDB := DB
	DB = db
	t.Cleanup(func() { DB = originalDB })

	pricing := &Pricing{}
	if err := pricing.Init(); err != nil {
		t.Fatalf("initialize pricing: %v", err)
	}
	if price, ok := pricing.FindPrice("gpt-5.6"); !ok || price.Model != "gpt-5*" {
		t.Fatalf("expected specific wildcard before deletion, price=%+v ok=%t", price, ok)
	}
	if err := pricing.BatchDeletePrices([]string{"gpt-5*"}); err != nil {
		t.Fatalf("delete specific wildcard: %v", err)
	}
	price, ok := pricing.FindPrice("gpt-5.6")
	if !ok || price.Model != "gpt-*" || price.Input != 2 {
		t.Fatalf("expected remaining broad wildcard after deletion, price=%+v ok=%t match=%v", price, ok, pricing.Match)
	}
	if !reflect.DeepEqual(pricing.Match, []string{"gpt-*"}) {
		t.Fatalf("stale wildcard patterns remained after deletion: %v", pricing.Match)
	}
}
