package model

import (
	"testing"

	"one-api/common/config"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestInitOptionMapRegistersPreferredChannelWaitOptions(t *testing.T) {
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

	config.GlobalOption = config.NewOptionManager()
	DB = testDB
	config.PreferredChannelWaitMilliseconds = 125
	config.PreferredChannelWaitPollMilliseconds = 25

	InitOptionMap()

	if got := config.GlobalOption.Get("PreferredChannelWaitMilliseconds"); got != "125" {
		t.Fatalf("expected preferred wait option registration, got %q", got)
	}
	if got := config.GlobalOption.Get("PreferredChannelWaitPollMilliseconds"); got != "25" {
		t.Fatalf("expected preferred wait poll option registration, got %q", got)
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
}
