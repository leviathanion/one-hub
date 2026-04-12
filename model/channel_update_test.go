package model

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/logger"

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

func stringPtr(value string) *string {
	return &value
}

func primeChannelDerivedCaches(t *testing.T, channelID int) {
	t.Helper()

	cacheEntries := map[string]string{
		fmt.Sprintf("%s:%d", codexTokenCacheKeyPrefix, channelID):        "cached-token",
		fmt.Sprintf("%s:%d", codexUsagePreviewCacheKeyPrefix, channelID): "cached-preview",
		fmt.Sprintf("%s:%d", codexUsageDetailCacheKeyPrefix, channelID):  "cached-detail",
	}

	for key, value := range cacheEntries {
		if err := cache.SetCache(key, value, time.Minute); err != nil {
			t.Fatalf("expected cache priming to succeed for %s, got %v", key, err)
		}
	}
}

func assertChannelDerivedCachesCleared(t *testing.T, channelID int) {
	t.Helper()

	cacheKeys := []string{
		fmt.Sprintf("%s:%d", codexTokenCacheKeyPrefix, channelID),
		fmt.Sprintf("%s:%d", codexUsagePreviewCacheKeyPrefix, channelID),
		fmt.Sprintf("%s:%d", codexUsageDetailCacheKeyPrefix, channelID),
	}

	for _, key := range cacheKeys {
		if _, err := cache.GetCache[string](key); !errors.Is(err, cache.CacheNotFound) {
			t.Fatalf("expected cache key %s to be cleared, got err=%v", key, err)
		}
	}
}

func assertChannelDerivedCachesPresent(t *testing.T, channelID int) {
	t.Helper()

	cacheKeys := []string{
		fmt.Sprintf("%s:%d", codexTokenCacheKeyPrefix, channelID),
		fmt.Sprintf("%s:%d", codexUsagePreviewCacheKeyPrefix, channelID),
		fmt.Sprintf("%s:%d", codexUsageDetailCacheKeyPrefix, channelID),
	}

	for _, key := range cacheKeys {
		if _, err := cache.GetCache[string](key); err != nil {
			t.Fatalf("expected cache key %s to still exist, got err=%v", key, err)
		}
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

func TestChannelUpdateRawClearsCodexDerivedCaches(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex",
		Key:    "sk-codex",
		Group:  "default",
		Models: "gpt-5",
	})
	primeChannelDerivedCaches(t, 1)

	update := &Channel{
		Id:   1,
		Name: "codex-updated",
	}
	if err := update.UpdateRaw(false); err != nil {
		t.Fatalf("expected channel update to succeed, got %v", err)
	}

	assertChannelDerivedCachesCleared(t, 1)
}

func TestChannelUpdateRawIgnoresMissingCodexDerivedCaches(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeAzure,
		Name:   "azure",
		Key:    "sk-azure",
		Group:  "default",
		Models: "gpt-4o",
	})

	update := &Channel{
		Id:   1,
		Name: "azure-updated",
	}
	if err := update.UpdateRaw(false); err != nil {
		t.Fatalf("expected channel update to ignore missing derived caches, got %v", err)
	}
}

func TestUpdateChannelKeyClearsCodexDerivedCaches(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex",
		Key:    "sk-codex",
		Group:  "default",
		Models: "gpt-5",
	})
	primeChannelDerivedCaches(t, 1)

	if err := UpdateChannelKey(1, "sk-codex-updated"); err != nil {
		t.Fatalf("expected key update to succeed, got %v", err)
	}

	assertChannelDerivedCachesCleared(t, 1)
}

func TestChannelDeleteClearsCodexDerivedCaches(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-delete",
		Key:    "sk-delete",
		Group:  "default",
		Models: "gpt-5",
	})
	primeChannelDerivedCaches(t, 1)

	channel := &Channel{Id: 1}
	if err := channel.Delete(); err != nil {
		t.Fatalf("expected channel delete to succeed, got %v", err)
	}

	assertChannelDerivedCachesCleared(t, 1)
}

func TestBatchDeleteChannelClearsCodexDerivedCachesOnlyForCodexRows(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-batch-delete",
		Key:    "sk-delete-1",
		Group:  "default",
		Models: "gpt-5",
	})
	insertTestChannel(t, &Channel{
		Id:     2,
		Type:   3,
		Name:   "azure-batch-delete",
		Key:    "sk-delete-2",
		Group:  "default",
		Models: "gpt-4o",
	})
	primeChannelDerivedCaches(t, 1)
	primeChannelDerivedCaches(t, 2)

	rows, err := BatchDeleteChannel([]int{1, 2})
	if err != nil {
		t.Fatalf("expected batch delete to succeed, got %v", err)
	}
	if rows != 2 {
		t.Fatalf("expected two channels to be deleted, got %d", rows)
	}

	assertChannelDerivedCachesCleared(t, 1)
	assertChannelDerivedCachesPresent(t, 2)
}

func TestDeleteDisabledChannelClearsCodexDerivedCachesOnlyForDeletedRows(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Status: config.ChannelStatusAutoDisabled,
		Name:   "codex-disabled",
		Key:    "sk-disabled",
		Group:  "default",
		Models: "gpt-5",
	})
	insertTestChannel(t, &Channel{
		Id:     2,
		Type:   config.ChannelTypeCodex,
		Status: config.ChannelStatusEnabled,
		Name:   "codex-enabled",
		Key:    "sk-enabled",
		Group:  "default",
		Models: "gpt-5",
	})
	insertTestChannel(t, &Channel{
		Id:     3,
		Type:   3,
		Status: config.ChannelStatusManuallyDisabled,
		Name:   "azure-disabled",
		Key:    "sk-azure-disabled",
		Group:  "default",
		Models: "gpt-4o",
	})
	primeChannelDerivedCaches(t, 1)
	primeChannelDerivedCaches(t, 2)
	primeChannelDerivedCaches(t, 3)

	rows, err := DeleteDisabledChannel()
	if err != nil {
		t.Fatalf("expected delete disabled channels to succeed, got %v", err)
	}
	if rows != 2 {
		t.Fatalf("expected exactly two disabled channels to be deleted, got %d", rows)
	}

	assertChannelDerivedCachesCleared(t, 1)
	assertChannelDerivedCachesPresent(t, 2)
	assertChannelDerivedCachesPresent(t, 3)
}

func TestDeleteChannelsTagClearsCodexDerivedCaches(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-tag-delete-1",
		Key:    "sk-tag-delete-1",
		Group:  "default",
		Models: "gpt-5",
		Tag:    "codex-team",
	})
	insertTestChannel(t, &Channel{
		Id:     2,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-tag-delete-2",
		Key:    "sk-tag-delete-2",
		Group:  "default",
		Models: "gpt-5",
		Tag:    "codex-team",
	})
	insertTestChannel(t, &Channel{
		Id:     3,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-other-tag",
		Key:    "sk-other-tag",
		Group:  "default",
		Models: "gpt-5",
		Tag:    "other-team",
	})
	primeChannelDerivedCaches(t, 1)
	primeChannelDerivedCaches(t, 2)
	primeChannelDerivedCaches(t, 3)

	if err := DeleteChannelsTag("codex-team", false); err != nil {
		t.Fatalf("expected tag delete to succeed, got %v", err)
	}

	assertChannelDerivedCachesCleared(t, 1)
	assertChannelDerivedCachesCleared(t, 2)
	assertChannelDerivedCachesPresent(t, 3)
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

func TestUpdateChannelsTagClearsCodexDerivedCaches(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:      1,
		Type:    config.ChannelTypeCodex,
		Name:    "codex-tag",
		Key:     "sk-tagged",
		Group:   "default",
		Models:  "gpt-5",
		Tag:     "codex-team",
		BaseURL: stringPtr("https://old.example"),
	})
	insertTestChannel(t, &Channel{
		Id:      2,
		Type:    config.ChannelTypeCodex,
		Name:    "codex-tag-2",
		Key:     "sk-tagged-2",
		Group:   "default",
		Models:  "gpt-5",
		Tag:     "codex-team",
		BaseURL: stringPtr("https://old.example"),
	})
	primeChannelDerivedCaches(t, 1)
	primeChannelDerivedCaches(t, 2)

	if err := UpdateChannelsTag("codex-team", &Channel{
		Name:    "codex-tag",
		Key:     "sk-tagged\nsk-tagged-2",
		Group:   "default",
		Models:  "gpt-5",
		BaseURL: stringPtr("https://new.example"),
	}); err != nil {
		t.Fatalf("expected tag update to succeed, got %v", err)
	}

	assertChannelDerivedCachesCleared(t, 1)
	assertChannelDerivedCachesCleared(t, 2)
}

func TestBatchUpdateChannelsAzureApiRejectsUnsupportedCodexOtherWhenTypeOmitted(t *testing.T) {
	useTestChannelDB(t)
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-batch",
		Key:    "sk-batch",
		Group:  "default",
		Models: "gpt-5",
	})

	if _, err := BatchUpdateChannelsAzureApi(&BatchChannelsParams{
		Ids:   []int{1},
		Value: `{"user_agent_regex":"^Codex/"}`,
	}); err == nil {
		t.Fatal("expected batch other update to reject unsupported Codex fields when payload omits type")
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Codex channel lookup to succeed, got %v", err)
	}
	if persisted.Other != "" {
		t.Fatalf("expected rejected batch update not to mutate other, got %q", persisted.Other)
	}
}

func TestBatchUpdateChannelsAzureApiClearsCodexDerivedCaches(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-batch",
		Key:    "sk-batch",
		Group:  "default",
		Models: "gpt-5",
		Other:  `{"user_agent":"old-agent"}`,
	})
	insertTestChannel(t, &Channel{
		Id:     2,
		Type:   3,
		Name:   "azure-batch",
		Key:    "sk-azure",
		Group:  "default",
		Models: "gpt-4o",
		Other:  "2024-05-01-preview",
	})
	primeChannelDerivedCaches(t, 1)
	primeChannelDerivedCaches(t, 2)

	count, err := BatchUpdateChannelsAzureApi(&BatchChannelsParams{
		Ids:   []int{1, 2},
		Value: `{"user_agent":"new-agent"}`,
	})
	if err != nil {
		t.Fatalf("expected batch update to succeed, got %v", err)
	}
	if count != 2 {
		t.Fatalf("expected both rows to update, got %d", count)
	}

	assertChannelDerivedCachesCleared(t, 1)

	for _, key := range []string{
		fmt.Sprintf("%s:%d", codexTokenCacheKeyPrefix, 2),
		fmt.Sprintf("%s:%d", codexUsagePreviewCacheKeyPrefix, 2),
		fmt.Sprintf("%s:%d", codexUsageDetailCacheKeyPrefix, 2),
	} {
		if _, err := cache.GetCache[string](key); err != nil {
			t.Fatalf("expected non-Codex batch update not to clear cache key %s, got %v", key, err)
		}
	}
}

func TestBatchUpdateChannelsAzureApiAllowsPlainOtherForAzureChannels(t *testing.T) {
	useTestChannelDB(t)
	logger.SetupLogger()

	insertTestChannel(t, &Channel{
		Id:     1,
		Type:   3,
		Name:   "azure-batch",
		Key:    "sk-azure",
		Group:  "default",
		Models: "gpt-4o",
		Other:  "2024-05-01-preview",
	})

	count, err := BatchUpdateChannelsAzureApi(&BatchChannelsParams{
		Ids:   []int{1},
		Value: "2024-06-01",
	})
	if err != nil {
		t.Fatalf("expected Azure batch other update to stay valid, got %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one Azure channel update, got %d", count)
	}

	persisted, err := GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Azure channel lookup to succeed, got %v", err)
	}
	if persisted.Other != "2024-06-01" {
		t.Fatalf("expected Azure batch update to persist plain other value, got %q", persisted.Other)
	}
}
