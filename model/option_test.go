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
	t.Cleanup(func() {
		config.GlobalOption = originalOptionManager
		DB = originalDB
		config.PreferredChannelWaitMilliseconds = originalWait
		config.PreferredChannelWaitPollMilliseconds = originalPoll
		config.LarkClientId = originalLarkClientID
		config.LarkClientSecret = originalLarkClientSecret
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

func TestInitOptionMapSkipsUnknownDatabaseOptions(t *testing.T) {
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

	if !shouldLogInvalidOptionLoadError("PreferredChannelWaitMilliseconds", errors.New("must be an integer")) {
		t.Fatal("expected first invalid option load error to be logged")
	}
	if shouldLogInvalidOptionLoadError("PreferredChannelWaitMilliseconds", errors.New("must be an integer")) {
		t.Fatal("expected repeated invalid option load error to be deduplicated")
	}
	if !shouldLogInvalidOptionLoadError("PreferredChannelWaitMilliseconds", errors.New("must be positive")) {
		t.Fatal("expected changed invalid option load error to be logged again")
	}
}

func TestLoadOptionsFromDatabaseClearsLoggedInvalidOptionLoadErrorAfterSuccessfulReload(t *testing.T) {
	resetOptionSyncLogState(t)

	originalOptionManager := config.GlobalOption
	originalDB := DB
	originalWait := config.PreferredChannelWaitMilliseconds
	t.Cleanup(func() {
		config.GlobalOption = originalOptionManager
		DB = originalDB
		config.PreferredChannelWaitMilliseconds = originalWait
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
	if err := testDB.Create(&Option{Key: "PreferredChannelWaitMilliseconds", Value: "bad"}).Error; err != nil {
		t.Fatalf("expected invalid preferred wait seed to persist, got %v", err)
	}

	config.GlobalOption = config.NewOptionManager()
	DB = testDB
	config.PreferredChannelWaitMilliseconds = 10
	InitOptionMap()

	if _, exists := loggedInvalidOptionLoadErrors.Load("PreferredChannelWaitMilliseconds"); !exists {
		t.Fatal("expected invalid preferred wait load to be tracked")
	}

	if err := testDB.Model(&Option{}).Where("key = ?", "PreferredChannelWaitMilliseconds").Update("value", "7").Error; err != nil {
		t.Fatalf("expected preferred wait fix to persist, got %v", err)
	}

	loadOptionsFromDatabase()

	if _, exists := loggedInvalidOptionLoadErrors.Load("PreferredChannelWaitMilliseconds"); exists {
		t.Fatal("expected invalid preferred wait load state to clear after successful reload")
	}
	if got := config.GlobalOption.Get("PreferredChannelWaitMilliseconds"); got != "7" {
		t.Fatalf("expected repaired preferred wait to reload as 7, got %q", got)
	}
}

func TestLoadOptionsFromDatabaseClearsLoggedInvalidOptionLoadErrorWhenOptionDeleted(t *testing.T) {
	resetOptionSyncLogState(t)

	originalOptionManager := config.GlobalOption
	originalDB := DB
	originalWait := config.PreferredChannelWaitMilliseconds
	t.Cleanup(func() {
		config.GlobalOption = originalOptionManager
		DB = originalDB
		config.PreferredChannelWaitMilliseconds = originalWait
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
	if err := testDB.Create(&Option{Key: "PreferredChannelWaitMilliseconds", Value: "bad"}).Error; err != nil {
		t.Fatalf("expected invalid preferred wait seed to persist, got %v", err)
	}

	config.GlobalOption = config.NewOptionManager()
	DB = testDB
	config.PreferredChannelWaitMilliseconds = 10
	InitOptionMap()

	if _, exists := loggedInvalidOptionLoadErrors.Load("PreferredChannelWaitMilliseconds"); !exists {
		t.Fatal("expected invalid preferred wait load to be tracked")
	}

	if err := testDB.Delete(&Option{}, "key = ?", "PreferredChannelWaitMilliseconds").Error; err != nil {
		t.Fatalf("expected invalid preferred wait row deletion to succeed, got %v", err)
	}

	loadOptionsFromDatabase()

	if _, exists := loggedInvalidOptionLoadErrors.Load("PreferredChannelWaitMilliseconds"); exists {
		t.Fatal("expected deleted invalid option load state to be cleared")
	}
}
