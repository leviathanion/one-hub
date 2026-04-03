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
