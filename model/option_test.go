package model

import (
	"context"
	"sync"
	"testing"

	"one-api/common/config"
	"one-api/internal/testutil/sqlitetest"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestBillingOptionRangesRejectUnsafeValues(t *testing.T) {
	originalOptionManager := config.GlobalOption
	originalDB := DB
	t.Cleanup(func() {
		config.GlobalOption = originalOptionManager
		DB = originalDB
	})

	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := testDB.AutoMigrate(&Option{}, &PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePublicationVersionRows(testDB); err != nil {
		t.Fatal(err)
	}
	DB = testDB
	config.GlobalOption = config.NewOptionManager()
	InitOptionMap()

	for key, values := range map[string][]string{
		"PreConsumedQuota": {"-1"},
		"QuotaPerUnit":     {"0", "-0.1", "NaN", "+Inf"},
	} {
		for _, value := range values {
			if _, err := config.GlobalOption.ValidateRuntimeOverrides(map[string]string{key: value}); err == nil {
				t.Fatalf("%s accepted unsafe value %q", key, value)
			}
		}
	}
	if _, err := config.GlobalOption.ValidateRuntimeOverrides(map[string]string{"PreConsumedQuota": "0", "QuotaPerUnit": "1"}); err != nil {
		t.Fatalf("valid billing option ranges rejected: %v", err)
	}
}

func resetOptionSyncLogState(t *testing.T) {
	t.Helper()
	loggedUnknownOptionKeys = sync.Map{}
	t.Cleanup(func() {
		loggedUnknownOptionKeys = sync.Map{}
	})
}

func TestInitOptionMapRegistersPreferredChannelWaitOptions(t *testing.T) {
	originalOptionManager := config.GlobalOption
	originalDB := DB
	originalWait := config.PreferredChannelWaitMilliseconds
	originalPoll := config.PreferredChannelWaitPollMilliseconds
	originalChannelTestConcurrency := config.ChannelTestConcurrency
	originalLarkClientID := config.LarkClientId
	originalLarkClientSecret := config.LarkClientSecret
	originalRetryStatusCodes := config.RetryStatusCodes
	t.Cleanup(func() {
		config.GlobalOption = originalOptionManager
		DB = originalDB
		config.PreferredChannelWaitMilliseconds = originalWait
		config.PreferredChannelWaitPollMilliseconds = originalPoll
		config.ChannelTestConcurrency = originalChannelTestConcurrency
		config.LarkClientId = originalLarkClientID
		config.LarkClientSecret = originalLarkClientSecret
		if err := config.SetRetryStatusCodes(originalRetryStatusCodes); err != nil {
			t.Fatalf("restore retry status codes: %v", err)
		}
	})

	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&Option{}, &PublicationVersion{}); err != nil {
		t.Fatalf("expected option schema migration, got %v", err)
	}
	if err := EnsurePublicationVersionRows(testDB); err != nil {
		t.Fatal(err)
	}

	config.GlobalOption = config.NewOptionManager()
	DB = testDB
	config.PreferredChannelWaitMilliseconds = 125
	config.PreferredChannelWaitPollMilliseconds = 25
	config.ChannelTestConcurrency = 6
	config.LarkClientId = "cli_123"
	config.LarkClientSecret = "secret_123"
	if err := config.SetRetryStatusCodes("401,5xx"); err != nil {
		t.Fatalf("expected retry status code seed to parse, got %v", err)
	}

	InitOptionMap()

	if got := config.GlobalOption.Get("PreferredChannelWaitMilliseconds"); got != "125" {
		t.Fatalf("expected preferred wait option registration, got %q", got)
	}
	if got := config.GlobalOption.Get("PreferredChannelWaitPollMilliseconds"); got != "25" {
		t.Fatalf("expected preferred wait poll option registration, got %q", got)
	}
	if got := config.GlobalOption.Get("ChannelTestConcurrency"); got != "6" {
		t.Fatalf("expected channel test concurrency option registration, got %q", got)
	}
	if got := config.GlobalOption.Get("LarkClientId"); got != "cli_123" {
		t.Fatalf("expected lark client id registration, got %q", got)
	}
	if got := config.GlobalOption.Get("LarkClientSecret"); got != "secret_123" {
		t.Fatalf("expected lark client secret registration, got %q", got)
	}
	if got := config.GlobalOption.Get("RetryStatusCodes"); got != config.DefaultRetryStatusCodes {
		t.Fatalf("expected retry status codes default registration, got %q", got)
	}
	if err := UpdateOption("RetryStatusCodes", "401,403"); err != nil {
		t.Fatalf("expected retry status codes option update to succeed, got %v", err)
	}
	if got := config.GlobalOption.Get("RetryStatusCodes"); got != "401,403" {
		t.Fatalf("expected retry status codes option to update, got %q", got)
	}
	if err := config.GlobalOption.Validate("RetryStatusCodes", "999"); err == nil {
		t.Fatal("expected invalid retry status codes option to fail validation")
	}
	if _, exists := config.GlobalOption.GetPublic()["LarkClientSecret"]; exists {
		t.Fatal("expected lark client secret to be excluded from public options")
	}
}

func TestInitOptionMapRegistersExplicitVisibilityForAllOptions(t *testing.T) {
	originalOptionManager := config.GlobalOption
	originalDB := DB
	t.Cleanup(func() {
		config.GlobalOption = originalOptionManager
		DB = originalDB
	})

	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&Option{}, &PublicationVersion{}); err != nil {
		t.Fatalf("expected option schema migration, got %v", err)
	}
	if err := EnsurePublicationVersionRows(testDB); err != nil {
		t.Fatal(err)
	}

	config.GlobalOption = config.NewOptionManager()
	DB = testDB

	InitOptionMap()
	for _, removed := range []string{"ChatImageRequestProxy", "CFWorkerImageUrl", "CFWorkerImageKey"} {
		if config.GlobalOption.IsRegistered(removed) {
			t.Fatalf("legacy media proxy option remains registered: %s", removed)
		}
	}

	for key := range config.GlobalOption.GetAll() {
		metadata, exists := config.GlobalOption.GetMetadata(key)
		if !exists {
			t.Fatalf("expected metadata for registered option %s", key)
		}
		if metadata.Visibility == config.OptionVisibilityUnspecified {
			t.Fatalf("expected explicit visibility for registered option %s", key)
		}
	}
}

func TestInitOptionMapRejectsUnknownDatabaseOverrides(t *testing.T) {
	originalOptionManager := config.GlobalOption
	originalDB := DB
	originalWait := config.PreferredChannelWaitMilliseconds
	originalPoll := config.PreferredChannelWaitPollMilliseconds
	t.Cleanup(func() {
		config.GlobalOption = originalOptionManager
		DB = originalDB
		config.PreferredChannelWaitMilliseconds = originalWait
		config.PreferredChannelWaitPollMilliseconds = originalPoll
	})

	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&Option{}, &PublicationVersion{}); err != nil {
		t.Fatalf("expected option schema migration, got %v", err)
	}
	if err := EnsurePublicationVersionRows(testDB); err != nil {
		t.Fatal(err)
	}
	if err := testDB.Exec("DELETE FROM options").Error; err != nil {
		t.Fatalf("expected option table reset, got %v", err)
	}
	if err := testDB.Create(&Option{Key: "UnknownOption", Value: "value"}).Error; err != nil {
		t.Fatalf("expected unknown option seed to persist, got %v", err)
	}
	if err := testDB.Create(&Option{Key: "PreferredChannelWaitMilliseconds", Value: "30"}).Error; err != nil {
		t.Fatalf("expected known option seed to persist, got %v", err)
	}

	config.GlobalOption = config.NewOptionManager()
	DB = testDB
	config.PreferredChannelWaitMilliseconds = 125
	config.PreferredChannelWaitPollMilliseconds = 25

	InitOptionMap()

	if snapshot := config.GlobalOption.RuntimeSnapshot(); snapshot != nil {
		t.Fatalf("unknown override must prevent publication, got version %d", snapshot.Version())
	}
	if err := CheckOptionsPublication(context.Background()); err == nil {
		t.Fatal("unknown override must make options unready")
	}
}

func TestUpdateOptionRejectsUnknownKeysBeforePersistence(t *testing.T) {
	originalOptionManager := config.GlobalOption
	originalDB := DB
	t.Cleanup(func() {
		config.GlobalOption = originalOptionManager
		DB = originalDB
	})

	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&Option{}, &PublicationVersion{}); err != nil {
		t.Fatalf("expected option schema migration, got %v", err)
	}
	if err := EnsurePublicationVersionRows(testDB); err != nil {
		t.Fatal(err)
	}
	if err := testDB.Exec("DELETE FROM options").Error; err != nil {
		t.Fatalf("expected option table reset, got %v", err)
	}

	config.GlobalOption = config.NewOptionManager()
	DB = testDB
	InitOptionMap()

	if err := UpdateOption("UnknownOption", "value"); err == nil {
		t.Fatal("expected unknown option update to fail")
	}
	if _, err := GetOption("UnknownOption"); err == nil {
		t.Fatal("expected unknown option update to avoid persistence")
	}
}
