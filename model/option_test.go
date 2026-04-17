package model

import (
	"errors"
	"sync"
	"testing"

	"one-api/common/config"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func resetOptionSyncLogState(t *testing.T) {
	t.Helper()
	loggedUnknownOptionKeys = sync.Map{}
	loggedInvalidOptionLoadErrors = sync.Map{}
	t.Cleanup(func() {
		loggedUnknownOptionKeys = sync.Map{}
		loggedInvalidOptionLoadErrors = sync.Map{}
	})
}

func TestInitOptionMapRegistersPreferredChannelWaitOptions(t *testing.T) {
	originalOptionManager := config.GlobalOption
	originalDB := DB
	originalWait := config.PreferredChannelWaitMilliseconds
	originalPoll := config.PreferredChannelWaitPollMilliseconds
	originalLarkClientID := config.LarkClientId
	originalLarkClientSecret := config.LarkClientSecret
	originalRecoverEnabled := config.AutomaticRecoverChannelsEnabled
	originalRecoverInterval := config.AutomaticRecoverChannelsIntervalMinutes
	t.Cleanup(func() {
		config.GlobalOption = originalOptionManager
		DB = originalDB
		config.PreferredChannelWaitMilliseconds = originalWait
		config.PreferredChannelWaitPollMilliseconds = originalPoll
		config.LarkClientId = originalLarkClientID
		config.LarkClientSecret = originalLarkClientSecret
		config.AutomaticRecoverChannelsEnabled = originalRecoverEnabled
		config.AutomaticRecoverChannelsIntervalMinutes = originalRecoverInterval
	})

	testDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&Option{}); err != nil {
		t.Fatalf("expected option schema migration, got %v", err)
	}

	config.GlobalOption = config.NewOptionManager()
	DB = testDB
	config.PreferredChannelWaitMilliseconds = 125
	config.PreferredChannelWaitPollMilliseconds = 25
	config.LarkClientId = "cli_123"
	config.LarkClientSecret = "secret_123"

	InitOptionMap()

	if got := config.GlobalOption.Get("PreferredChannelWaitMilliseconds"); got != "125" {
		t.Fatalf("expected preferred wait option registration, got %q", got)
	}
	if got := config.GlobalOption.Get("PreferredChannelWaitPollMilliseconds"); got != "25" {
		t.Fatalf("expected preferred wait poll option registration, got %q", got)
	}
	if got := config.GlobalOption.Get("LarkClientId"); got != "cli_123" {
		t.Fatalf("expected lark client id registration, got %q", got)
	}
	if got := config.GlobalOption.Get("LarkClientSecret"); got != "secret_123" {
		t.Fatalf("expected lark client secret registration, got %q", got)
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

	testDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&Option{}); err != nil {
		t.Fatalf("expected option schema migration, got %v", err)
	}

	config.GlobalOption = config.NewOptionManager()
	DB = testDB

	InitOptionMap()

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

func TestInitOptionMapMigratesLegacyAutomaticRecoverInterval(t *testing.T) {
	originalOptionManager := config.GlobalOption
	originalDB := DB
	originalRecoverEnabled := config.AutomaticRecoverChannelsEnabled
	originalRecoverInterval := config.AutomaticRecoverChannelsIntervalMinutes
	t.Cleanup(func() {
		config.GlobalOption = originalOptionManager
		DB = originalDB
		config.AutomaticRecoverChannelsEnabled = originalRecoverEnabled
		config.AutomaticRecoverChannelsIntervalMinutes = originalRecoverInterval
	})

	testDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&Option{}); err != nil {
		t.Fatalf("expected option schema migration, got %v", err)
	}
	if err := testDB.Exec("DELETE FROM options").Error; err != nil {
		t.Fatalf("expected option table reset, got %v", err)
	}
	if err := testDB.Create(&Option{Key: "AutomaticEnableChannelRecoverFrequency", Value: "27"}).Error; err != nil {
		t.Fatalf("expected legacy recover frequency to persist, got %v", err)
	}

	config.GlobalOption = config.NewOptionManager()
	DB = testDB
	config.AutomaticRecoverChannelsEnabled = false
	config.AutomaticRecoverChannelsIntervalMinutes = 10

	InitOptionMap()

	if got := config.GlobalOption.Get("AutomaticRecoverChannelsIntervalMinutes"); got != "27" {
		t.Fatalf("expected legacy interval migration to register new option value, got %q", got)
	}
	if config.AutomaticRecoverChannelsEnabled {
		t.Fatal("expected legacy interval migration to preserve disabled auto-recover toggle")
	}

	migratedOption, err := GetOption("AutomaticRecoverChannelsIntervalMinutes")
	if err != nil {
		t.Fatalf("expected migrated recover interval lookup to succeed, got %v", err)
	}
	if migratedOption.Value != "27" {
		t.Fatalf("expected migrated recover interval value 27, got %q", migratedOption.Value)
	}
	if _, err := GetOption("AutomaticEnableChannelRecoverFrequency"); err == nil {
		t.Fatal("expected legacy recover interval row to be deleted after migration")
	}
}

func TestInitOptionMapCanonicalAutomaticRecoverIntervalWinsOverLegacyValue(t *testing.T) {
	originalOptionManager := config.GlobalOption
	originalDB := DB
	originalRecoverEnabled := config.AutomaticRecoverChannelsEnabled
	originalRecoverInterval := config.AutomaticRecoverChannelsIntervalMinutes
	t.Cleanup(func() {
		config.GlobalOption = originalOptionManager
		DB = originalDB
		config.AutomaticRecoverChannelsEnabled = originalRecoverEnabled
		config.AutomaticRecoverChannelsIntervalMinutes = originalRecoverInterval
	})

	testDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&Option{}); err != nil {
		t.Fatalf("expected option schema migration, got %v", err)
	}
	if err := testDB.Exec("DELETE FROM options").Error; err != nil {
		t.Fatalf("expected option table reset, got %v", err)
	}
	if err := testDB.Create(&Option{Key: "AutomaticEnableChannelRecoverFrequency", Value: "27"}).Error; err != nil {
		t.Fatalf("expected legacy recover frequency to persist, got %v", err)
	}
	if err := testDB.Create(&Option{Key: "AutomaticRecoverChannelsIntervalMinutes", Value: "11"}).Error; err != nil {
		t.Fatalf("expected canonical recover interval to persist, got %v", err)
	}

	config.GlobalOption = config.NewOptionManager()
	DB = testDB
	config.AutomaticRecoverChannelsEnabled = false
	config.AutomaticRecoverChannelsIntervalMinutes = 10

	InitOptionMap()

	if got := config.GlobalOption.Get("AutomaticRecoverChannelsIntervalMinutes"); got != "11" {
		t.Fatalf("expected canonical interval value to win over legacy row, got %q", got)
	}
	if _, err := GetOption("AutomaticEnableChannelRecoverFrequency"); err == nil {
		t.Fatal("expected legacy recover interval row to be deleted when canonical row already exists")
	}
}

func TestInitOptionMapSkipsUnknownDatabaseOptions(t *testing.T) {
	originalOptionManager := config.GlobalOption
	originalDB := DB
	originalWait := config.PreferredChannelWaitMilliseconds
	originalPoll := config.PreferredChannelWaitPollMilliseconds
	originalRecoverEnabled := config.AutomaticRecoverChannelsEnabled
	originalRecoverInterval := config.AutomaticRecoverChannelsIntervalMinutes
	t.Cleanup(func() {
		config.GlobalOption = originalOptionManager
		DB = originalDB
		config.PreferredChannelWaitMilliseconds = originalWait
		config.PreferredChannelWaitPollMilliseconds = originalPoll
		config.AutomaticRecoverChannelsEnabled = originalRecoverEnabled
		config.AutomaticRecoverChannelsIntervalMinutes = originalRecoverInterval
	})

	testDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&Option{}); err != nil {
		t.Fatalf("expected option schema migration, got %v", err)
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
	config.AutomaticRecoverChannelsEnabled = false
	config.AutomaticRecoverChannelsIntervalMinutes = 10

	InitOptionMap()

	if got := config.GlobalOption.Get("PreferredChannelWaitMilliseconds"); got != "30" {
		t.Fatalf("expected known option to load despite unknown rows, got %q", got)
	}
	if got := config.GlobalOption.Get("UnknownOption"); got != "" {
		t.Fatalf("expected unknown option row to be ignored, got %q", got)
	}
}

func TestUpdateOptionRejectsUnknownKeysBeforePersistence(t *testing.T) {
	originalOptionManager := config.GlobalOption
	originalDB := DB
	t.Cleanup(func() {
		config.GlobalOption = originalOptionManager
		DB = originalDB
	})

	testDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&Option{}); err != nil {
		t.Fatalf("expected option schema migration, got %v", err)
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

func TestShouldLogInvalidOptionLoadErrorDeduplicatesByMessage(t *testing.T) {
	resetOptionSyncLogState(t)

	if !shouldLogInvalidOptionLoadError("AutomaticRecoverChannelsIntervalMinutes", errors.New("must be an integer")) {
		t.Fatal("expected first invalid option load error to be logged")
	}
	if shouldLogInvalidOptionLoadError("AutomaticRecoverChannelsIntervalMinutes", errors.New("must be an integer")) {
		t.Fatal("expected repeated invalid option load error to be deduplicated")
	}
	if !shouldLogInvalidOptionLoadError("AutomaticRecoverChannelsIntervalMinutes", errors.New("must be positive")) {
		t.Fatal("expected changed invalid option load error to be logged again")
	}
}

func TestLoadOptionsFromDatabaseClearsLoggedInvalidOptionLoadErrorAfterSuccessfulReload(t *testing.T) {
	resetOptionSyncLogState(t)

	originalOptionManager := config.GlobalOption
	originalDB := DB
	originalRecoverInterval := config.AutomaticRecoverChannelsIntervalMinutes
	t.Cleanup(func() {
		config.GlobalOption = originalOptionManager
		DB = originalDB
		config.AutomaticRecoverChannelsIntervalMinutes = originalRecoverInterval
	})

	testDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&Option{}); err != nil {
		t.Fatalf("expected option schema migration, got %v", err)
	}
	if err := testDB.Exec("DELETE FROM options").Error; err != nil {
		t.Fatalf("expected option table reset, got %v", err)
	}
	if err := testDB.Create(&Option{Key: "AutomaticRecoverChannelsIntervalMinutes", Value: "bad"}).Error; err != nil {
		t.Fatalf("expected invalid recover interval seed to persist, got %v", err)
	}

	config.GlobalOption = config.NewOptionManager()
	DB = testDB
	config.AutomaticRecoverChannelsIntervalMinutes = 10
	InitOptionMap()

	if _, exists := loggedInvalidOptionLoadErrors.Load("AutomaticRecoverChannelsIntervalMinutes"); !exists {
		t.Fatal("expected invalid recover interval load to be tracked")
	}

	if err := testDB.Model(&Option{}).Where("key = ?", "AutomaticRecoverChannelsIntervalMinutes").Update("value", "7").Error; err != nil {
		t.Fatalf("expected recover interval fix to persist, got %v", err)
	}

	loadOptionsFromDatabase()

	if _, exists := loggedInvalidOptionLoadErrors.Load("AutomaticRecoverChannelsIntervalMinutes"); exists {
		t.Fatal("expected invalid recover interval load state to clear after successful reload")
	}
	if got := config.GlobalOption.Get("AutomaticRecoverChannelsIntervalMinutes"); got != "7" {
		t.Fatalf("expected repaired recover interval to reload as 7, got %q", got)
	}
}

func TestLoadOptionsFromDatabaseClearsLoggedInvalidOptionLoadErrorWhenOptionDeleted(t *testing.T) {
	resetOptionSyncLogState(t)

	originalOptionManager := config.GlobalOption
	originalDB := DB
	originalRecoverInterval := config.AutomaticRecoverChannelsIntervalMinutes
	t.Cleanup(func() {
		config.GlobalOption = originalOptionManager
		DB = originalDB
		config.AutomaticRecoverChannelsIntervalMinutes = originalRecoverInterval
	})

	testDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&Option{}); err != nil {
		t.Fatalf("expected option schema migration, got %v", err)
	}
	if err := testDB.Exec("DELETE FROM options").Error; err != nil {
		t.Fatalf("expected option table reset, got %v", err)
	}
	if err := testDB.Create(&Option{Key: "AutomaticRecoverChannelsIntervalMinutes", Value: "bad"}).Error; err != nil {
		t.Fatalf("expected invalid recover interval seed to persist, got %v", err)
	}

	config.GlobalOption = config.NewOptionManager()
	DB = testDB
	config.AutomaticRecoverChannelsIntervalMinutes = 10
	InitOptionMap()

	if _, exists := loggedInvalidOptionLoadErrors.Load("AutomaticRecoverChannelsIntervalMinutes"); !exists {
		t.Fatal("expected invalid recover interval load to be tracked")
	}

	if err := testDB.Delete(&Option{}, "key = ?", "AutomaticRecoverChannelsIntervalMinutes").Error; err != nil {
		t.Fatalf("expected invalid recover interval row deletion to succeed, got %v", err)
	}

	loadOptionsFromDatabase()

	if _, exists := loggedInvalidOptionLoadErrors.Load("AutomaticRecoverChannelsIntervalMinutes"); exists {
		t.Fatal("expected deleted invalid option load state to be cleared")
	}
}
