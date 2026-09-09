package model

import (
	"context"
	"errors"
	"testing"

	"one-api/common/config"
	"one-api/internal/testutil/sqlitetest"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupOptionsPublicationTest(t *testing.T) (*gorm.DB, *string, *string) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Option{}, &PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	originalDB := DB
	originalOptions := config.GlobalOption
	DB = db
	config.GlobalOption = config.NewOptionManager()
	publicValue := "default"
	secretValue := ""
	config.GlobalOption.RegisterStringOption("Public", &publicValue, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	config.GlobalOption.RegisterStringOption("Secret", &secretValue, config.OptionMetadata{Visibility: config.OptionVisibilitySensitive})
	if err := LoadAndPublishOptions(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		DB = originalDB
		config.GlobalOption = originalOptions
		setOptionsPublicationError(nil)
	})
	return db, &publicValue, &secretValue
}

func TestOptionOverrideAndInheritRoundTrip(t *testing.T) {
	db, _, _ := setupOptionsPublicationTest(t)
	empty := ""
	version, err := ApplyOptionMutations(context.Background(), 1, []OptionMutation{{Key: "Public", Value: &empty}})
	if err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("override version=%d", version)
	}
	value, ok := config.GlobalOption.RuntimeSnapshot().Get("Public")
	if !ok || value.Source != config.RuntimeOptionSourceOverride || value.Override == nil || *value.Override != "" || value.Effective != "" {
		t.Fatalf("explicit empty override lost: %+v", value)
	}
	var stored Option
	if err := db.Where("key = ?", "Public").Take(&stored).Error; err != nil || stored.Value != "" {
		t.Fatalf("explicit empty override row missing: %+v err=%v", stored, err)
	}
	version, err = ApplyOptionMutations(context.Background(), 2, []OptionMutation{{Key: "Public", Inherit: true}})
	if err != nil {
		t.Fatal(err)
	}
	value, _ = config.GlobalOption.RuntimeSnapshot().Get("Public")
	if version != 3 || value.Source != config.RuntimeOptionSourceDefault || value.Override != nil || value.Effective != "default" {
		t.Fatalf("inherit did not restore registry default: version=%d value=%+v", version, value)
	}
	var count int64
	if err := db.Model(&Option{}).Where("key = ?", "Public").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("inherit did not delete override row: count=%d err=%v", count, err)
	}
}

func TestOptionBatchCASAndValidationAreAtomic(t *testing.T) {
	_, _, _ = setupOptionsPublicationTest(t)
	public := "changed"
	if _, err := ApplyOptionMutations(context.Background(), 1, []OptionMutation{
		{Key: "Public", Value: &public},
		{Key: "Unknown", Inherit: true},
	}); err == nil {
		t.Fatal("invalid batch succeeded")
	}
	if snapshot := config.GlobalOption.RuntimeSnapshot(); snapshot.Version() != 1 || snapshot.EffectiveValues()["Public"] != "default" {
		t.Fatalf("invalid batch partially published: version=%d values=%v", snapshot.Version(), snapshot.EffectiveValues())
	}
	if _, err := ApplyOptionMutations(context.Background(), 0, []OptionMutation{{Key: "Public", Value: &public}}); err == nil {
		t.Fatal("non-positive expected version succeeded")
	}
	if _, err := ApplyOptionMutations(context.Background(), 1, []OptionMutation{{Key: "Public", Value: &public}}); err != nil {
		t.Fatal(err)
	}
	second := "second"
	if _, err := ApplyOptionMutations(context.Background(), 1, []OptionMutation{{Key: "Public", Value: &second}}); !errors.Is(err, ErrPublicationVersionConflict) {
		t.Fatalf("stale options writer returned %v", err)
	}
	if got := config.GlobalOption.RuntimeSnapshot().EffectiveValues()["Public"]; got != "changed" {
		t.Fatalf("stale writer changed runtime value to %q", got)
	}
}

func TestOptionsWatcherPublishesAdvancedDatabaseHead(t *testing.T) {
	db, _, _ := setupOptionsPublicationTest(t)
	if err := db.Create(&Option{Key: "Public", Value: "other-instance"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := BumpPublicationVersionCAS(context.Background(), db, PublicationOwnerOptions, 1); err != nil {
		t.Fatal(err)
	}
	if err := SyncOptionsPublication(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := config.GlobalOption.RuntimeSnapshot()
	if snapshot.Version() != 2 || snapshot.EffectiveValues()["Public"] != "other-instance" {
		t.Fatalf("watcher did not converge: version=%d values=%v", snapshot.Version(), snapshot.EffectiveValues())
	}
	if err := CheckOptionsPublication(context.Background()); err != nil {
		t.Fatalf("converged options were not ready: %v", err)
	}
}

func TestResolveOptionsCommitOutcomeRequiresExactHeadAndOverrideSet(t *testing.T) {
	db, _, _ := setupOptionsPublicationTest(t)
	commitErr := errors.New("commit acknowledgement lost")
	target := map[string]string{"Public": "committed"}
	if err := resolveOptionsCommitOutcome(context.Background(), 1, commitErr, target); !errors.Is(err, commitErr) {
		t.Fatalf("unchanged head must retain commit error: %v", err)
	}
	if err := db.Create(&Option{Key: "Public", Value: "committed"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := BumpPublicationVersionCAS(context.Background(), db, PublicationOwnerOptions, 1); err != nil {
		t.Fatal(err)
	}
	if err := resolveOptionsCommitOutcome(context.Background(), 1, commitErr, target); err != nil {
		t.Fatalf("matching committed target was not resolved: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := resolveOptionsCommitOutcome(canceled, 1, commitErr, target); err != nil {
		t.Fatalf("options commit probe must outlive canceled request context: %v", err)
	}
	if err := db.Model(&Option{}).Where("key = ?", "Public").Update("value", "different").Error; err != nil {
		t.Fatal(err)
	}
	if err := resolveOptionsCommitOutcome(context.Background(), 1, commitErr, target); !errors.Is(err, ErrOptionsCommitOutcomeUnknown) {
		t.Fatalf("mismatched target returned %v", err)
	}
}
