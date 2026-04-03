package model

import (
	"fmt"
	"testing"

	"one-api/common/config"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useTestChannelDB(t *testing.T) {
	t.Helper()

	originalDB := DB
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&Channel{}); err != nil {
		t.Fatalf("expected channel schema migration for test database, got %v", err)
	}

	DB = testDB
	t.Cleanup(func() {
		DB = originalDB
	})
}

func insertTestChannel(t *testing.T, channel *Channel) {
	t.Helper()
	if err := DB.Create(channel).Error; err != nil {
		t.Fatalf("expected channel fixture to persist, got %v", err)
	}
}

func TestChannelUpdateRawRejectsInvalidCodexOtherWhenTypeOmitted(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex",
		Key:    "sk-codex",
		Group:  "default",
		Models: "gpt-5",
	})

	update := &Channel{
		Id:    1,
		Other: `{"prompt_cache_key_strategy":`,
	}
	if err := update.UpdateRaw(false); err == nil {
		t.Fatal("expected invalid Codex other JSON to be rejected when update payload omits type")
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted channel lookup to succeed, got %v", err)
	}
	if persisted.Type != config.ChannelTypeCodex {
		t.Fatalf("expected persisted channel type to remain Codex, got %d", persisted.Type)
	}
	if persisted.Other != "" {
		t.Fatalf("expected rejected update not to mutate other, got %q", persisted.Other)
	}
}

func TestChannelUpdateRawOverwritePreservesPersistedTypeWhenTypeOmitted(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex",
		Key:    "sk-codex",
		Group:  "default",
		Models: "gpt-5",
	})

	update := &Channel{
		Id:     1,
		Name:   "codex-updated",
		Key:    "sk-codex-updated",
		Group:  "default",
		Models: "gpt-5",
	}
	if err := update.UpdateRaw(true); err != nil {
		t.Fatalf("expected overwrite update to succeed without zeroing type, got %v", err)
	}
	if update.Type != config.ChannelTypeCodex {
		t.Fatalf("expected in-memory channel type to be hydrated from persistence, got %d", update.Type)
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted channel lookup to succeed, got %v", err)
	}
	if persisted.Type != config.ChannelTypeCodex {
		t.Fatalf("expected overwrite update to preserve persisted type, got %d", persisted.Type)
	}
	if persisted.Name != "codex-updated" {
		t.Fatalf("expected overwrite update to persist requested fields, got %q", persisted.Name)
	}
}

func TestUpdateChannelsTagRejectsInvalidCodexOtherWhenTypeOmitted(t *testing.T) {
	useTestChannelDB(t)

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-tag",
		Key:    "sk-tagged",
		Group:  "default",
		Models: "gpt-5",
		Tag:    "codex-team",
	})

	if err := UpdateChannelsTag("codex-team", &Channel{
		Key:   "sk-tagged",
		Other: `{"prompt_cache_key_strategy":`,
	}); err == nil {
		t.Fatal("expected tag update to reject invalid Codex other JSON when payload omits type")
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted tagged channel lookup to succeed, got %v", err)
	}
	if persisted.Type != config.ChannelTypeCodex {
		t.Fatalf("expected tagged channel type to remain Codex, got %d", persisted.Type)
	}
	if persisted.Other != "" {
		t.Fatalf("expected rejected tag update not to mutate other, got %q", persisted.Other)
	}
}
