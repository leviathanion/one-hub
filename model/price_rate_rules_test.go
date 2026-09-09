package model

import (
	"encoding/json"
	"math"
	"one-api/common/utils"
	"path/filepath"
	"testing"

	"one-api/common/config"
	"one-api/internal/testutil/sqlitetest"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestPriceRateRulesJSONRejectsUnknownFields(t *testing.T) {
	tests := []string{
		`{"fast":{"input":2,"output":2}}`,
		`{"flex":{"input":2,"output":2,"future":1}}`,
		`{"long_context":{"input_threshold":1000,"input_multiplier":2,"output_multiplier":2,"future":1}}`,
	}
	for _, payload := range tests {
		var rules PriceRateRules
		if err := json.Unmarshal([]byte(payload), &rules); err == nil {
			t.Fatalf("expected unknown billing field to be rejected: %s", payload)
		}
	}

	var empty PriceRateRules
	if err := json.Unmarshal([]byte(`{}`), &empty); err != nil {
		t.Fatalf("explicit empty rules must remain valid: %v", err)
	}
}

func TestPriceTokenAdjustmentAndPolicyValidationRejectOverflowInputs(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	if got := GetIncreaseTokens(maxInt, 2); got != maxInt {
		t.Fatalf("overflowing token adjustment = %d, want saturation at %d", got, maxInt)
	}
	if got := GetIncreaseTokens(maxInt, 0); got != -maxInt {
		t.Fatalf("zero-ratio token adjustment = %d, want %d", got, -maxInt)
	}
	if got := GetIncreaseTokens(10, math.NaN()); got != 0 {
		t.Fatalf("NaN token adjustment = %d, want defensive zero", got)
	}

	invalidExtraRatios := datatypes.NewJSONType(map[string]float64{"future_usage": math.Inf(1)})
	if err := (&Price{Type: TokensPriceType, ExtraRatios: &invalidExtraRatios}).prepareForPersistence(); err == nil {
		t.Fatal("expected non-finite extra ratio to be rejected")
	}
	invalidRateRules := datatypes.NewJSONType(PriceRateRules{Version: 2, ServiceTier: []PriceRateRule{{ID: "flex", When: PriceRuleCondition{ServiceTier: []string{"flex"}}, Multipliers: PriceRateMultiplier{Input: utils.GetPointer(float64(math.NaN())), Output: utils.GetPointer(float64(1))}}}})
	if err := (&Price{Type: TokensPriceType, RateRules: &invalidRateRules}).prepareForPersistence(); err == nil {
		t.Fatal("expected non-finite rate multiplier to be rejected")
	}
}

func TestPricePersistenceRejectsInvalidBaseFields(t *testing.T) {
	tests := []struct {
		name  string
		price Price
	}{
		{name: "missing type", price: Price{}},
		{name: "unknown type", price: Price{Type: "token"}},
		{name: "whitespace type", price: Price{Type: " tokens "}},
		{name: "negative channel type", price: Price{Type: TokensPriceType, ChannelType: -1}},
		{name: "negative input", price: Price{Type: TokensPriceType, Input: -1}},
		{name: "negative output", price: Price{Type: TokensPriceType, Output: -1}},
		{name: "nan input", price: Price{Type: TokensPriceType, Input: math.NaN()}},
		{name: "infinite output", price: Price{Type: TokensPriceType, Output: math.Inf(1)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.price.prepareForPersistence(); err == nil {
				t.Fatalf("invalid price was accepted: %+v", test.price)
			}
		})
	}
	for _, priceType := range []string{TokensPriceType, TimesPriceType} {
		if err := (&Price{Model: "valid-zero", Type: priceType}).prepareForPersistence(); err != nil {
			t.Fatalf("valid zero-valued %s price was rejected: %v", priceType, err)
		}
	}
}

func openPriceSyncTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "prices.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Price{}, &ModelInfo{}, &PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	originalDB := DB
	DB = db
	t.Cleanup(func() { DB = originalDB })
	return db
}

func TestPriceSyncRejectsInvalidRemoteBaseFieldsBeforePersistence(t *testing.T) {
	syncModes := []struct {
		name string
		sync func(*Pricing, []*Price) error
	}{
		{name: "without overwrite", sync: (*Pricing).SyncPriceWithoutOverwrite},
		{name: "overwrite", sync: (*Pricing).SyncPriceWithOverwrite},
		{name: "only update", sync: (*Pricing).SyncPriceOnlyUpdate},
	}
	invalidFields := []struct {
		name   string
		mutate func(*Price)
	}{
		{name: "unknown type", mutate: func(price *Price) { price.Type = "token" }},
		{name: "negative input", mutate: func(price *Price) { price.Input = -1 }},
		{name: "negative output", mutate: func(price *Price) { price.Output = -1 }},
	}

	for _, mode := range syncModes {
		for _, field := range invalidFields {
			t.Run(mode.name+"/"+field.name, func(t *testing.T) {
				db := openPriceSyncTestDB(t)
				stored := &Price{Model: "existing", Type: TokensPriceType, Input: 1, Output: 2}
				if err := db.Create(stored).Error; err != nil {
					t.Fatal(err)
				}
				pricing := &Pricing{Prices: map[string]*Price{stored.Model: stored}, Match: []string{}}
				invalid := &Price{Model: stored.Model, Type: TokensPriceType, Input: 3, Output: 4}
				if mode.name == "without overwrite" {
					invalid.Model = "remote-model"
				}
				field.mutate(invalid)

				if err := mode.sync(pricing, []*Price{invalid}); err == nil {
					t.Fatal("invalid remote price sync succeeded")
				}
				var reloaded Price
				if err := db.Where("model = ?", stored.Model).First(&reloaded).Error; err != nil {
					t.Fatalf("original price was not preserved after rollback: %v", err)
				}
				if reloaded.Type != TokensPriceType || reloaded.Input != 1 || reloaded.Output != 2 {
					t.Fatalf("original price changed after rejected sync: %+v", reloaded)
				}
				var invalidCount int64
				if err := db.Model(&Price{}).Where("model = ?", invalid.Model).Count(&invalidCount).Error; err != nil {
					t.Fatal(err)
				}
				if invalid.Model != stored.Model && invalidCount != 0 {
					t.Fatalf("invalid remote price was persisted, rows=%d", invalidCount)
				}
			})
		}
	}
}

func TestPriceSyncClosesNoCandidateTransactions(t *testing.T) {
	t.Run("overwrite commits removal when remote prices are locked locally", func(t *testing.T) {
		db := openPriceSyncTestDB(t)
		locked := &Price{Model: "locked", Type: TokensPriceType, Input: 1, Output: 2, Locked: true}
		stale := &Price{Model: "stale", Type: TokensPriceType, Input: 3, Output: 4}
		if err := db.Create([]*Price{locked, stale}).Error; err != nil {
			t.Fatal(err)
		}
		pricing := &Pricing{Prices: map[string]*Price{locked.Model: locked, stale.Model: stale}, Match: []string{}}
		remote := &Price{Model: locked.Model, Type: TokensPriceType, Input: 9, Output: 9}

		if err := pricing.SyncPriceWithOverwrite([]*Price{remote}); err != nil {
			t.Fatal(err)
		}
		if _, ok := pricing.Prices[locked.Model]; !ok {
			t.Fatal("locked price was removed")
		}
		if _, ok := pricing.Prices[stale.Model]; ok {
			t.Fatal("stale unlocked price was not removed")
		}
	})

	t.Run("only update returns before opening an empty transaction", func(t *testing.T) {
		db := openPriceSyncTestDB(t)
		stored := &Price{Model: "existing", Type: TokensPriceType, Input: 1, Output: 2}
		if err := db.Create(stored).Error; err != nil {
			t.Fatal(err)
		}
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		sqlDB.SetMaxOpenConns(1)
		pricing := &Pricing{Prices: map[string]*Price{stored.Model: stored}, Match: []string{}}
		remote := &Price{Model: "missing", Type: TokensPriceType, Input: 3, Output: 4}

		if err := pricing.SyncPriceOnlyUpdate([]*Price{remote}); err != nil {
			t.Fatal(err)
		}
		if inUse := sqlDB.Stats().InUse; inUse != 0 {
			t.Fatalf("empty update leaked a transaction connection: in_use=%d", inUse)
		}
	})

	t.Run("empty overwrite is rejected without mutation", func(t *testing.T) {
		db := openPriceSyncTestDB(t)
		stored := &Price{Model: "existing", Type: TokensPriceType, Input: 1, Output: 2}
		if err := db.Create(stored).Error; err != nil {
			t.Fatal(err)
		}
		pricing := &Pricing{Prices: map[string]*Price{stored.Model: stored}, Match: []string{}}
		if err := pricing.SyncPriceWithOverwrite(nil); err == nil {
			t.Fatal("empty remote catalog was accepted")
		}
		var count int64
		if err := db.Model(&Price{}).Where("model = ?", stored.Model).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("empty remote list changed stored prices, rows=%d", count)
		}
	})
}

func TestPriceRulesAreExplicitAndModelIndependent(t *testing.T) {
	for _, price := range GetDefaultPrice() {
		if price.Model == "gpt-5.6" || price.Model == "gpt-5.6-sol" || price.Model == "gpt-5.6-terra" || price.Model == "gpt-5.6-luna" {
			t.Fatalf("GPT-5.6 price %q must be configured through the pricing page", price.Model)
		}
	}
	for _, modelName := range []string{"gpt-5.6", "gpt-5.6-sol", "custom-model"} {
		price := &Price{Model: modelName}
		if rules := price.EffectiveRateRules(); testRuleMultiplier(rules, "flex") != nil || testRuleMultiplier(rules, "priority") != nil || rules.LongContext != nil {
			t.Fatalf("model name %q must not create implicit rate rules, got %+v", modelName, rules)
		}
		if got := price.GetExtraRatio(config.UsageExtraCache); got != 1 {
			t.Fatalf("model name %q must not create an implicit cache ratio, got %v", modelName, got)
		}
	}

	rules := datatypes.NewJSONType(PriceRateRules{Version: 2, ServiceTier: []PriceRateRule{{ID: "flex", When: PriceRuleCondition{ServiceTier: []string{"flex"}}, Multipliers: PriceRateMultiplier{Input: utils.GetPointer(float64(0.5)), Output: utils.GetPointer(float64(0.5))}}}})
	extra := datatypes.NewJSONType(map[string]float64{config.UsageExtraCache: 0.1})
	configured := &Price{Model: "any-model", RateRules: &rules, ExtraRatios: &extra}
	if got := testRuleMultiplier(configured.EffectiveRateRules(), "flex"); got == nil || *got.Input != 0.5 || *got.Output != 0.5 {
		t.Fatalf("expected explicit generic rate rules, got %+v", got)
	}
	if got := configured.GetExtraRatio(config.UsageExtraCache); got != 0.1 {
		t.Fatalf("expected explicit generic cache ratio 0.1, got %v", got)
	}
}

func TestPricePersistsOmittedRulesWithoutInventingDefaults(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Price{}); err != nil {
		t.Fatal(err)
	}

	defaultPrice := &Price{Model: "custom-model", Type: TokensPriceType, Input: 1, Output: 6}
	if err := InsertPrices(db, []*Price{defaultPrice}); err != nil {
		t.Fatal(err)
	}

	var stored Price
	if err := db.Where("model = ?", defaultPrice.Model).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RateRules != nil {
		t.Fatalf("expected unlocked rate rules to remain omitted after persistence, got %+v", stored.RateRules.Data())
	}
	if got := stored.GetExtraRatio(config.UsageExtraCache); got != 1 {
		t.Fatalf("expected persisted price not to invent a cache ratio, got %v", got)
	}
	if rules := stored.EffectiveRateRules(); testRuleMultiplier(rules, "flex") != nil || testRuleMultiplier(rules, "priority") != nil || rules.LongContext != nil {
		t.Fatalf("expected omitted rate rules to remain empty, got %+v", rules)
	}
}

func TestPriceForSyncTreatsMissingRateRulesAsKeepLocal(t *testing.T) {
	localRules := datatypes.NewJSONType(PriceRateRules{Version: 2, ServiceTier: []PriceRateRule{{ID: "flex", When: PriceRuleCondition{ServiceTier: []string{"flex"}}, Multipliers: PriceRateMultiplier{Input: utils.GetPointer(float64(0.5)), Output: utils.GetPointer(float64(0.5))}}}})
	local := &Price{Model: "custom-model", RateRules: &localRules}
	remote := &Price{Model: "custom-model", Input: 3, Output: 4}

	synced := priceForSync(remote, local)
	if synced == remote {
		t.Fatal("expected sync preparation not to mutate the remote object")
	}
	if synced.RateRules == nil || testRuleMultiplier(synced.RateRules.Data(), "flex") == nil {
		t.Fatal("expected omitted remote rate_rules to preserve the local override")
	}
	if remote.RateRules != nil {
		t.Fatal("expected the remote object to remain unchanged")
	}

	emptyRules := datatypes.NewJSONType(PriceRateRules{})
	remote.RateRules = &emptyRules
	synced = priceForSync(remote, local)
	if synced.RateRules != &emptyRules {
		t.Fatal("expected an explicit empty rate_rules object to replace the local override")
	}
}

func TestUpdatePriceRateRulesPresenceContract(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Price{}, &ModelInfo{}, &PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	originalDB := DB
	DB = db
	t.Cleanup(func() { DB = originalDB })

	rules := datatypes.NewJSONType(PriceRateRules{Version: 2, ServiceTier: []PriceRateRule{{ID: "flex", When: PriceRuleCondition{ServiceTier: []string{"flex"}}, Multipliers: PriceRateMultiplier{Input: utils.GetPointer(float64(0.5)), Output: utils.GetPointer(float64(0.5))}}}})
	stored := &Price{Model: "gpt-5", Type: TokensPriceType, ChannelType: 1, Input: 1, Output: 2, RateRules: &rules}
	if err := stored.Insert(); err != nil {
		t.Fatal(err)
	}
	pricing := &Pricing{Prices: map[string]*Price{stored.Model: stored}}

	omitted := &Price{Model: stored.Model, Type: TokensPriceType, ChannelType: 1, Input: 3, Output: 4}
	newerRules := datatypes.NewJSONType(PriceRateRules{Version: 2, LongContext: []PriceRateRule{{ID: "long_context", When: PriceRuleCondition{InputTokens: &PriceTokenRange{GT: utils.GetPointer(1000)}}, Multipliers: PriceRateMultiplier{Input: utils.GetPointer(float64(2)), Output: utils.GetPointer(float64(1.5))}}}})
	if err := db.Model(&Price{}).Where("model = ?", stored.Model).Update("rate_rules", &newerRules).Error; err != nil {
		t.Fatalf("simulate a newer rule saved by another instance: %v", err)
	}
	if err := pricing.UpdatePriceWithRateRulesPresence(stored.Model, omitted, false); err != nil {
		t.Fatalf("update with omitted rules: %v", err)
	}
	if got := pricing.Prices[stored.Model].EffectiveRateRules(); got.LongContext == nil || testRuleMultiplier(got, "flex") != nil {
		t.Fatalf("expected omitted rules to preserve the newer database policy, got %+v", got)
	}

	emptyRules := datatypes.NewJSONType(PriceRateRules{})
	cleared := &Price{Model: stored.Model, Type: TokensPriceType, ChannelType: 1, Input: 5, Output: 6, RateRules: &emptyRules}
	if err := pricing.UpdatePriceWithRateRulesPresence(stored.Model, cleared, true); err != nil {
		t.Fatalf("clear explicit rules: %v", err)
	}
	if got := pricing.Prices[stored.Model].EffectiveRateRules(); testRuleMultiplier(got, "flex") != nil || pricing.Prices[stored.Model].RateRules == nil {
		t.Fatalf("expected explicit empty rules to clear the policy while preserving presence, got %+v", pricing.Prices[stored.Model].RateRules)
	}

	replacementRules := datatypes.NewJSONType(PriceRateRules{Version: 2, ServiceTier: []PriceRateRule{{ID: "priority", When: PriceRuleCondition{ServiceTier: []string{"fast", "priority"}}, Multipliers: PriceRateMultiplier{Input: utils.GetPointer(float64(1.5)), Output: utils.GetPointer(float64(2))}}}})
	replacement := &Price{Model: stored.Model, Type: TokensPriceType, ChannelType: 1, Input: 7, Output: 8, RateRules: &replacementRules}
	if err := pricing.UpdatePriceWithRateRulesPresence(stored.Model, replacement, true); err != nil {
		t.Fatalf("replace explicit rules: %v", err)
	}
	if got := testRuleMultiplier(pricing.Prices[stored.Model].EffectiveRateRules(), "priority"); got == nil || *got.Input != 1.5 {
		t.Fatalf("expected new rules to replace the policy, got %+v", got)
	}

	invalidRules := datatypes.NewJSONType(PriceRateRules{Version: 2, ServiceTier: []PriceRateRule{{ID: "flex", When: PriceRuleCondition{ServiceTier: []string{"flex"}}, Multipliers: PriceRateMultiplier{Input: utils.GetPointer(float64(-1)), Output: utils.GetPointer(float64(1))}}}})
	invalid := &Price{Model: stored.Model, Type: TokensPriceType, ChannelType: 1, Input: 9, Output: 10, RateRules: &invalidRules}
	if err := pricing.UpdatePriceWithRateRulesPresence(stored.Model, invalid, true); err == nil {
		t.Fatal("expected invalid replacement rules to fail")
	}
	var durable Price
	if err := db.Where("model = ?", stored.Model).First(&durable).Error; err != nil {
		t.Fatal(err)
	}
	if durable.Input != 7 || testRuleMultiplier(durable.EffectiveRateRules(), "priority") == nil {
		t.Fatalf("expected invalid replacement not to mutate durable price, got %+v", durable)
	}
}

func TestBatchPriceUpdatePreservesOmittedOptionalPolicies(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Price{}); err != nil {
		t.Fatal(err)
	}

	rules := datatypes.NewJSONType(PriceRateRules{Version: 2, ServiceTier: []PriceRateRule{{ID: "flex", When: PriceRuleCondition{ServiceTier: []string{"flex"}}, Multipliers: PriceRateMultiplier{Input: utils.GetPointer(float64(0.4)), Output: utils.GetPointer(float64(0.6))}}}})
	extra := datatypes.NewJSONType(map[string]float64{"cache": 0.2})
	stored := &Price{Model: "custom-model", Type: TokensPriceType, Input: 1, Output: 2, RateRules: &rules, ExtraRatios: &extra}
	if err := db.Create(stored).Error; err != nil {
		t.Fatal(err)
	}
	invalidExtra := datatypes.NewJSONType(map[string]float64{"cache": -0.1})
	if err := UpdatePrices(db, []string{stored.Model}, &Price{Type: TokensPriceType, Input: 99, Output: 99, ExtraRatios: &invalidExtra}); err == nil {
		t.Fatal("expected unlocked batch update to reject a negative extra ratio")
	}
	var unchanged Price
	if err := db.Where("model = ?", stored.Model).First(&unchanged).Error; err != nil {
		t.Fatal(err)
	}
	if unchanged.Input != 1 || unchanged.Output != 2 || unchanged.ExtraRatios.Data()["cache"] != 0.2 {
		t.Fatalf("expected rejected batch update not to mutate persistence, got %+v", unchanged)
	}

	if err := UpdatePrices(db, []string{stored.Model}, &Price{Type: TokensPriceType, Input: 3, Output: 4}); err != nil {
		t.Fatal(err)
	}
	var updated Price
	if err := db.Where("model = ?", stored.Model).First(&updated).Error; err != nil {
		t.Fatal(err)
	}
	if updated.Input != 3 || updated.Output != 4 || updated.RateRules == nil || updated.ExtraRatios == nil {
		t.Fatalf("expected base price update to preserve omitted policies, got %+v", updated)
	}
	if got := testRuleMultiplier(updated.RateRules.Data(), "flex"); got == nil || *got.Input != 0.4 || updated.ExtraRatios.Data()["cache"] != 0.2 {
		t.Fatalf("expected custom policies to survive batch update, rules=%+v extra=%+v", updated.RateRules.Data(), updated.ExtraRatios.Data())
	}

	if err := UpdatePrices(db, []string{stored.Model}, &Price{Type: TokensPriceType, Input: 5, Output: 6, Locked: true}); err != nil {
		t.Fatalf("lock price without optional policies: %v", err)
	}
	if err := db.Where("model = ?", stored.Model).First(&updated).Error; err != nil {
		t.Fatalf("reload locked price: %v", err)
	}
	if !updated.Locked || updated.RateRules == nil || updated.ExtraRatios == nil {
		t.Fatalf("expected locking to preserve omitted optional policies, got %+v", updated)
	}
	if got := testRuleMultiplier(updated.RateRules.Data(), "flex"); got == nil || *got.Input != 0.4 || updated.ExtraRatios.Data()["cache"] != 0.2 {
		t.Fatalf("expected custom policies to survive locking, rules=%+v extra=%+v", updated.RateRules.Data(), updated.ExtraRatios.Data())
	}

	emptyRules := datatypes.NewJSONType(PriceRateRules{})
	if err := UpdatePrices(db, []string{stored.Model}, &Price{Type: TokensPriceType, Input: 3, Output: 4, RateRules: &emptyRules}); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("model = ?", stored.Model).First(&updated).Error; err != nil {
		t.Fatal(err)
	}
	if updated.RateRules == nil || testRuleMultiplier(updated.RateRules.Data(), "flex") != nil {
		t.Fatalf("expected explicit empty rate_rules to clear the override, got %+v", updated.RateRules)
	}
}

func TestPriceOverwriteSyncPreservesOmittedLocalRateRules(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Price{}, &ModelInfo{}, &PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	originalDB := DB
	DB = db
	t.Cleanup(func() { DB = originalDB })

	localRules := datatypes.NewJSONType(PriceRateRules{Version: 2, ServiceTier: []PriceRateRule{{ID: "flex", When: PriceRuleCondition{ServiceTier: []string{"flex"}}, Multipliers: PriceRateMultiplier{Input: utils.GetPointer(float64(0.4)), Output: utils.GetPointer(float64(0.6))}}}})
	local := &Price{Model: "custom-model", Type: TokensPriceType, Input: 1, Output: 2, RateRules: &localRules}
	if err := db.Create(local).Error; err != nil {
		t.Fatal(err)
	}
	pricing := &Pricing{Prices: map[string]*Price{local.Model: local}}
	remote := &Price{Model: local.Model, Type: TokensPriceType, Input: 3, Output: 4}

	if err := pricing.SyncPriceWithOverwrite([]*Price{remote}); err != nil {
		t.Fatal(err)
	}
	var stored Price
	if err := db.Where("model = ?", local.Model).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Input != 3 || stored.Output != 4 {
		t.Fatalf("expected remote base price 3/4, got %v/%v", stored.Input, stored.Output)
	}
	if stored.RateRules == nil || testRuleMultiplier(stored.RateRules.Data(), "flex") == nil || *testRuleMultiplier(stored.RateRules.Data(), "flex").Input != 0.4 {
		t.Fatalf("expected local rate_rules to survive overwrite sync, got %+v", stored.RateRules)
	}
}

func TestLockedPricePersistsExplicitRateRules(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Price{}); err != nil {
		t.Fatal(err)
	}
	rules := datatypes.NewJSONType(PriceRateRules{Version: 2, ServiceTier: []PriceRateRule{{ID: "flex", When: PriceRuleCondition{ServiceTier: []string{"flex"}}, Multipliers: PriceRateMultiplier{Input: utils.GetPointer(float64(0.4)), Output: utils.GetPointer(float64(0.6))}}}})
	price := &Price{Model: "custom-model", Type: TokensPriceType, Input: 1, Output: 6, Locked: true, RateRules: &rules}
	if err := InsertPrices(db, []*Price{price}); err != nil {
		t.Fatal(err)
	}

	var stored Price
	if err := db.Where("model = ?", "custom-model").First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RateRules == nil {
		t.Fatal("expected locked explicit rules to persist")
	}
	storedRules := stored.RateRules.Data()
	if testRuleMultiplier(storedRules, "flex") == nil || *testRuleMultiplier(storedRules, "flex").Input != 0.4 || testRuleMultiplier(storedRules, "priority") != nil || storedRules.LongContext != nil {
		t.Fatalf("expected only the explicitly configured locked policy, got %+v", storedRules)
	}
}

func testRuleMultiplier(rules PriceRateRules, id string) *PriceRateMultiplier {
	for _, rule := range rules.ServiceTier {
		if rule.ID == id {
			return &rule.Multipliers
		}
	}
	return nil
}
