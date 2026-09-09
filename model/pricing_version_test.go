package model

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"one-api/internal/testutil/sqlitetest"

	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupVersionedPricingTest(t *testing.T) (*gorm.DB, *Pricing) {
	t.Helper()
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
	return db, &Pricing{Prices: make(map[string]*Price)}
}

func TestPricingManagementReadsReturnDeepCopies(t *testing.T) {
	extra := datatypes.NewJSONType(map[string]float64{"cache": 0.5})
	pricing := &Pricing{Prices: map[string]*Price{
		"model": {Model: "model", Type: TokensPriceType, Input: 1, Output: 2, ExtraRatios: &extra},
	}}
	byName := pricing.GetAllPrices()
	byName["model"].Input = 99
	byName["model"].ExtraRatios.Data()["cache"] = 9
	list := pricing.GetAllPricesList()
	list[0].Output = 88
	list[0].ExtraRatios.Data()["cache"] = 8
	stored := pricing.Prices["model"]
	if stored.Input != 1 || stored.Output != 2 || stored.ExtraRatios.Data()["cache"] != 0.5 {
		t.Fatalf("management read mutated published state: %+v", stored)
	}
}

func TestManagementPricesAndVersionAreReadTogether(t *testing.T) {
	pricing := &Pricing{Prices: make(map[string]*Price)}
	if !pricing.replacePublication(2, map[string]*Price{"new": {Model: "new", Type: TokensPriceType, Input: 3, Output: 4}}, nil) {
		t.Fatal("publish prices")
	}
	rows, version := pricing.GetAllPricesListWithVersion()
	if version != 2 || len(rows) != 1 || rows[0].Model != "new" {
		t.Fatalf("management state mismatch: version=%d rows=%+v", version, rows)
	}
}

func TestSyncPricePublicationConvergesToAdvancedHeadAndClearsTransientError(t *testing.T) {
	db, pricing := setupVersionedPricingTest(t)
	originalPricing := PricingInstance
	PricingInstance = pricing
	t.Cleanup(func() { PricingInstance = originalPricing })
	if err := pricing.Init(); err != nil {
		t.Fatal(err)
	}
	pricing.setPublicationError(errors.New("transient read failure"))
	if err := SyncPricePublication(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pricing.IsDegraded() {
		t.Fatal("successful head observation did not clear transient publication degradation")
	}
	if err := db.Create(&Price{Model: "watcher-model", Type: TokensPriceType, Input: 1, Output: 2}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := BumpPublicationVersionCAS(context.Background(), db, PublicationOwnerPrice, 1); err != nil {
		t.Fatal(err)
	}
	if err := SyncPricePublication(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pricing.PublishedVersion() != 2 {
		t.Fatalf("watcher published version %d", pricing.PublishedVersion())
	}
	if _, ok := pricing.FindExactPrice("watcher-model"); !ok {
		t.Fatal("watcher did not publish the advanced database state")
	}
}

func TestPricingDegradedStateSeparatesPublicationAndRemoteSyncFailures(t *testing.T) {
	pricing := &Pricing{Prices: make(map[string]*Price)}
	pricing.setPublicationError(errors.New("load failed"))
	if !pricing.IsDegraded() {
		t.Fatal("publication failure was not visible")
	}
	pricing.setPublicationError(nil)
	if pricing.IsDegraded() {
		t.Fatal("cleared publication failure remained visible")
	}
	pricing.setRemoteSyncError(errors.New("remote catalog unavailable"))
	pricing.setPublicationError(nil)
	if !pricing.IsDegraded() {
		t.Fatal("remote sync failure was cleared by publication success")
	}
	pricing.setRemoteSyncError(nil)
	if pricing.IsDegraded() {
		t.Fatal("successful remote sync did not clear degradation")
	}
}

func TestResolvePriceCommitOutcomeRequiresExpectedHeadAndTarget(t *testing.T) {
	db, _ := setupVersionedPricingTest(t)
	commitErr := errors.New("commit acknowledgement lost")
	verifyTarget := func(tx *gorm.DB) (bool, error) {
		var count int64
		err := tx.Model(&Price{}).Where("model = ? AND input = ?", "target", 3).Count(&count).Error
		return count == 1, err
	}
	if err := resolvePriceCommitOutcome(context.Background(), 1, commitErr, verifyTarget); !errors.Is(err, commitErr) {
		t.Fatalf("unchanged head must retain transaction error, got %v", err)
	}
	if err := db.Create(&Price{Model: "target", Type: TokensPriceType, Input: 3, Output: 4}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := BumpPublicationVersionCAS(context.Background(), db, PublicationOwnerPrice, 1); err != nil {
		t.Fatal(err)
	}
	if err := resolvePriceCommitOutcome(context.Background(), 1, commitErr, verifyTarget); err != nil {
		t.Fatalf("matching committed head and target must resolve success: %v", err)
	}
	if err := db.Create(&Price{Model: "unexpected", Type: TokensPriceType, Input: 1, Output: 1}).Error; err != nil {
		t.Fatal(err)
	}
	completeTarget, err := loadPricePolicyState(db.Where("model <> ?", "unexpected"))
	if err != nil {
		t.Fatal(err)
	}
	verifyCompleteTarget := func(tx *gorm.DB) (bool, error) {
		actual, err := loadPricePolicyState(tx)
		return reflect.DeepEqual(actual, completeTarget), err
	}
	if err := resolvePriceCommitOutcome(context.Background(), 1, commitErr, verifyCompleteTarget); !errors.Is(err, ErrPriceCommitOutcomeUnknown) {
		t.Fatalf("extra committed state must not resolve success: %v", err)
	}
	if err := db.Where("model = ?", "unexpected").Delete(&Price{}).Error; err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := resolvePriceCommitOutcome(canceled, 1, commitErr, verifyTarget); err != nil {
		t.Fatalf("commit probe must outlive canceled request context: %v", err)
	}
	if err := db.Model(&Price{}).Where("model = ?", "target").Update("input", 9).Error; err != nil {
		t.Fatal(err)
	}
	if err := resolvePriceCommitOutcome(context.Background(), 1, commitErr, verifyTarget); !errors.Is(err, ErrPriceCommitOutcomeUnknown) {
		t.Fatalf("mismatched target must be unknown, got %v", err)
	}
	if _, err := BumpPublicationVersionCAS(context.Background(), db, PublicationOwnerPrice, 2); err != nil {
		t.Fatal(err)
	}
	if err := resolvePriceCommitOutcome(context.Background(), 1, commitErr, verifyTarget); !errors.Is(err, ErrPriceCommitOutcomeUnknown) {
		t.Fatalf("advanced head beyond expected commit must be unknown, got %v", err)
	}
}

func TestResolvePriceCommitOutcomeRejectsHeadChangeDuringStateProbe(t *testing.T) {
	db, _ := setupVersionedPricingTest(t)
	if err := db.Create(&Price{Model: "target", Type: TokensPriceType, Input: 3, Output: 4}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := BumpPublicationVersionCAS(context.Background(), db, PublicationOwnerPrice, 1); err != nil {
		t.Fatal(err)
	}
	verifyAndAdvance := func(tx *gorm.DB) (bool, error) {
		var count int64
		if err := tx.Model(&Price{}).Where("model = ? AND input = ?", "target", 3).Count(&count).Error; err != nil {
			return false, err
		}
		_, err := BumpPublicationVersionCAS(context.Background(), db, PublicationOwnerPrice, 2)
		return count == 1, err
	}
	commitErr := errors.New("commit acknowledgement lost")
	if err := resolvePriceCommitOutcome(context.Background(), 1, commitErr, verifyAndAdvance); !errors.Is(err, ErrPriceCommitOutcomeUnknown) {
		t.Fatalf("head change during target probe returned %v", err)
	}
}

func TestVersionedPricingMutationRejectsStaleWriterAndPublishesCommit(t *testing.T) {
	db, pricing := setupVersionedPricingTest(t)
	if err := pricing.Init(); err != nil {
		t.Fatal(err)
	}
	if got := pricing.PublishedVersion(); got != 1 {
		t.Fatalf("initial published version=%d", got)
	}
	if err := pricing.AddPriceAtVersion(&Price{Model: "model-a", Type: TokensPriceType, Input: 1, Output: 2}, 1); err != nil {
		t.Fatal(err)
	}
	if got := pricing.PublishedVersion(); got != 2 {
		t.Fatalf("committed price version was not published: %d", got)
	}
	if _, ok := pricing.FindPrice("model-a"); !ok {
		t.Fatal("published catalog omitted committed model")
	}
	if err := pricing.AddPriceAtVersion(&Price{Model: "model-b", Type: TokensPriceType, Input: 1, Output: 2}, 1); !errors.Is(err, ErrPublicationVersionConflict) {
		t.Fatalf("stale price writer returned %v", err)
	}
	var count int64
	if err := db.Model(&Price{}).Where("model = ?", "model-b").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("stale writer mutated rows: count=%d err=%v", count, err)
	}
}

func TestVersionedPriceNoOpDoesNotDependOnDriverRowsAffected(t *testing.T) {
	db, pricing := setupVersionedPricingTest(t)
	stored := &Price{Model: "same", Type: TokensPriceType, Input: 1, Output: 2}
	if err := db.Create(stored).Error; err != nil {
		t.Fatal(err)
	}
	if err := pricing.Init(); err != nil {
		t.Fatal(err)
	}
	if err := pricing.UpdatePriceAtVersion("same", &Price{Model: "same", Type: TokensPriceType, Input: 1, Output: 2}, false, 1); err != nil {
		t.Fatalf("idempotent update failed: %v", err)
	}
	if head, err := ReadPublicationVersion(context.Background(), db, PublicationOwnerPrice); err != nil || head != 1 {
		t.Fatalf("idempotent update advanced head: head=%d err=%v", head, err)
	}
	if _, err := BumpPublicationVersionCAS(context.Background(), db, PublicationOwnerPrice, 1); err != nil {
		t.Fatal(err)
	}
	if err := pricing.UpdatePriceAtVersion("same", &Price{Model: "same", Type: TokensPriceType, Input: 1, Output: 2}, false, 1); !errors.Is(err, ErrPublicationVersionConflict) {
		t.Fatalf("stale idempotent update returned %v", err)
	}
}

func TestPricePublicationNeverMovesBackward(t *testing.T) {
	pricing := &Pricing{Prices: make(map[string]*Price)}
	newer := map[string]*Price{"new": {Model: "new", Type: TokensPriceType}}
	older := map[string]*Price{"old": {Model: "old", Type: TokensPriceType}}
	if !pricing.replacePublication(3, newer, nil) {
		t.Fatal("newer catalog was not published")
	}
	if pricing.replacePublication(2, older, nil) {
		t.Fatal("older catalog replaced newer publication")
	}
	if got := pricing.PublishedVersion(); got != 3 {
		t.Fatalf("published version moved backward: %d", got)
	} else if _, ok := pricing.FindPrice("new"); !ok {
		t.Fatal("newer catalog contents were replaced")
	}
}

func TestPriceLoaderPublishesOnlyAfterHeadAdvances(t *testing.T) {
	db, pricing := setupVersionedPricingTest(t)
	if err := pricing.Init(); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Price{Model: "unsupported-direct-write", Type: TokensPriceType, Input: 1, Output: 2}).Error; err != nil {
		t.Fatal(err)
	}
	if err := pricing.Init(); err != nil {
		t.Fatal(err)
	}
	if _, ok := pricing.FindPrice("unsupported-direct-write"); ok {
		t.Fatal("same-version direct table edit replaced the immutable publication")
	}
	if _, err := BumpPublicationVersionCAS(context.Background(), db, PublicationOwnerPrice, 1); err != nil {
		t.Fatal(err)
	}
	if err := pricing.Init(); err != nil {
		t.Fatal(err)
	}
	if _, ok := pricing.FindPrice("unsupported-direct-write"); !ok || pricing.PublishedVersion() != 2 {
		t.Fatalf("advanced head was not loaded: version=%d", pricing.PublishedVersion())
	}
}
