package model

import (
	"fmt"
	"testing"

	"one-api/common/cache"
	"one-api/common/config"

	"gorm.io/gorm"
)

func TestChangeChannelsTagStatusRotatesOnlyAffectedCodexChannelsBothDirections(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	channels := []*Channel{
		{Id: 7101, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled, Tag: "status-invalidation", Name: "codex", Key: "old", Group: "default", Models: "gpt-5"},
		{Id: 7102, Type: config.ChannelTypeOpenAI, Status: config.ChannelStatusEnabled, Tag: "status-invalidation", Name: "openai", Key: "key", Group: "default", Models: "gpt-4o"},
		{Id: 7103, Type: config.ChannelTypeCodex, Status: config.ChannelStatusManuallyDisabled, Tag: "other", Name: "untouched", Key: "key", Group: "default", Models: "gpt-5"},
	}
	for _, channel := range channels {
		insertTestChannel(t, channel)
		primeChannelDerivedCaches(t, channel.Id)
	}

	if err := ChangeChannelsTagStatus("status-invalidation", config.ChannelStatusManuallyDisabled); err != nil {
		t.Fatal(err)
	}
	assertChannelDerivedCachesCleared(t, 7101)
	assertChannelDerivedCachesPresent(t, 7102)
	assertChannelDerivedCachesPresent(t, 7103)
	generationAfterDisable, err := cache.GetCache[string](fmt.Sprintf("%s:%d", codexUsageGenerationCacheKeyPrefix, 7101))
	if err != nil {
		t.Fatal(err)
	}

	if err := ChangeChannelsTagStatus("status-invalidation", config.ChannelStatusEnabled); err != nil {
		t.Fatal(err)
	}
	generationAfterEnable, err := cache.GetCache[string](fmt.Sprintf("%s:%d", codexUsageGenerationCacheKeyPrefix, 7101))
	if err != nil || generationAfterEnable == generationAfterDisable {
		t.Fatalf("enable must rotate again so disabled-era data cannot revive: before=%q after=%q err=%v", generationAfterDisable, generationAfterEnable, err)
	}
}

func TestChangeChannelsTagStatusSkipsRowChangedAfterQuery(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	const channelID = 7120
	const changedChannelID = 7121
	insertTestChannel(t, &Channel{Id: channelID, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled, Tag: "concurrent-status", Name: "raced", Key: "key", Group: "default", Models: "gpt-5"})
	insertTestChannel(t, &Channel{Id: changedChannelID, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled, Tag: "concurrent-status", Name: "changed", Key: "key", Group: "default", Models: "gpt-5"})
	primeChannelDerivedCaches(t, channelID)
	primeChannelDerivedCaches(t, changedChannelID)

	callbackName := "test:change_tag_status_before_cas:" + t.Name()
	var hookErr error
	fired := false
	if err := DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if fired {
			return
		}
		fired = true
		// The candidate query has completed when its first CAS reaches this hook.
		// Change the durable value immediately before that CAS executes.
		hookErr = tx.Exec("UPDATE channels SET status = ? WHERE id = ?", config.ChannelStatusAutoDisabled, channelID).Error
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DB.Callback().Update().Remove(callbackName) })

	if err := ChangeChannelsTagStatus("concurrent-status", config.ChannelStatusManuallyDisabled); err != nil {
		t.Fatal(err)
	}
	if hookErr != nil {
		t.Fatalf("interleaved status update failed: %v", hookErr)
	}
	var channel Channel
	if err := DB.First(&channel, channelID).Error; err != nil {
		t.Fatal(err)
	}
	if channel.Status != config.ChannelStatusAutoDisabled {
		t.Fatalf("CAS overwrote concurrent status: got %d", channel.Status)
	}
	assertChannelDerivedCachesPresent(t, channelID)
	assertChannelDerivedCachesCleared(t, changedChannelID)
}

func TestChangeChannelsTagStatusSkipsRowMovedAfterQuery(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	const channelID = 7122
	insertTestChannel(t, &Channel{Id: channelID, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled, Tag: "concurrent-tag", Name: "moved", Key: "key", Group: "default", Models: "gpt-5"})
	primeChannelDerivedCaches(t, channelID)

	callbackName := "test:change_tag_status_before_tag_cas:" + t.Name()
	var hookErr error
	fired := false
	if err := DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if fired {
			return
		}
		fired = true
		// Interleave a tag-only change after the candidate query and immediately
		// before its status CAS. The status intentionally remains unchanged.
		hookErr = tx.Exec("UPDATE channels SET tag = ? WHERE id = ?", "concurrent-tag-moved", channelID).Error
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DB.Callback().Update().Remove(callbackName) })

	if err := ChangeChannelsTagStatus("concurrent-tag", config.ChannelStatusManuallyDisabled); err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("interleaving hook did not run")
	}
	if hookErr != nil {
		t.Fatalf("interleaved tag update failed: %v", hookErr)
	}
	var channel Channel
	if err := DB.First(&channel, channelID).Error; err != nil {
		t.Fatal(err)
	}
	if channel.Tag != "concurrent-tag-moved" || channel.Status != config.ChannelStatusEnabled {
		t.Fatalf("CAS changed row moved out of tag: tag=%q status=%d", channel.Tag, channel.Status)
	}
	assertChannelDerivedCachesPresent(t, channelID)
}

func TestChangeChannelsTagStatusUsesOneUpdateForManyChannels(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()

	const channelCount = 250
	channels := make([]Channel, 0, channelCount)
	for i := 0; i < channelCount; i++ {
		channels = append(channels, Channel{
			Id:     7200 + i,
			Type:   config.ChannelTypeOpenAI,
			Status: config.ChannelStatusEnabled,
			Tag:    "bulk-status",
			Name:   fmt.Sprintf("bulk-%d", i),
			Key:    fmt.Sprintf("key-%d", i),
			Group:  "default",
			Models: "gpt-4o",
		})
	}
	if err := DB.Create(&channels).Error; err != nil {
		t.Fatal(err)
	}

	callbackName := "test:count_change_tag_updates:" + t.Name()
	updates := 0
	if err := DB.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "channels" {
			updates++
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DB.Callback().Update().Remove(callbackName) })

	if err := ChangeChannelsTagStatus("bulk-status", config.ChannelStatusManuallyDisabled); err != nil {
		t.Fatal(err)
	}
	if updates != 1 {
		t.Fatalf("channel status UPDATE count = %d, want 1 for %d channels", updates, channelCount)
	}
	var remaining int64
	if err := DB.Model(&Channel{}).
		Where("tag = ? AND status <> ?", "bulk-status", config.ChannelStatusManuallyDisabled).
		Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d bulk channels were not updated", remaining)
	}
}
