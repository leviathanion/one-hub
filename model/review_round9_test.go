package model

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/logger"
)

func assertNoCodexGeneration(t *testing.T, channelID int) {
	t.Helper()
	key := fmt.Sprintf("%s:%d", codexUsageGenerationCacheKeyPrefix, channelID)
	if value, err := cache.GetCache[string](key); !errors.Is(err, cache.CacheNotFound) {
		t.Fatalf("non-Codex channel %d created generation %q: %v", channelID, value, err)
	}
}

func TestGetChannelsByTypeAndStatusWithContextHonorsCancellation(t *testing.T) {
	useTestChannelDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := GetChannelsByTypeAndStatusWithContext(ctx, config.ChannelTypeCodex, config.ChannelStatusEnabled); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled channel load, got %v", err)
	}
}

func TestNonCodexMutationsDoNotCreateCodexGeneration(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	for _, channel := range []*Channel{
		{Id: 7201, Type: config.ChannelTypeOpenAI, Status: config.ChannelStatusEnabled, Name: "update", Key: "key", Group: "default", Models: "gpt-4o"},
		{Id: 7202, Type: config.ChannelTypeAnthropic, Status: config.ChannelStatusEnabled, Name: "status", Key: "key", Group: "default", Models: "claude-3"},
		{Id: 7203, Type: config.ChannelTypeOpenAI, Status: config.ChannelStatusEnabled, Name: "model", Key: "key", Group: "default", Models: "gpt-4o,shared"},
	} {
		insertTestChannel(t, channel)
	}

	if err := (&Channel{Id: 7201, Type: config.ChannelTypeOpenAI, Name: "updated"}).UpdateRaw(false); err != nil {
		t.Fatal(err)
	}
	UpdateChannelStatusById(7202, config.ChannelStatusManuallyDisabled)
	if count, err := BatchDelModelChannels(&BatchDelModelChannelsParams{Ids: []int{7203}, Value: "shared"}); err != nil || count != 1 {
		t.Fatalf("batch model mutation failed: count=%d err=%v", count, err)
	}

	for _, id := range []int{7201, 7202, 7203} {
		assertNoCodexGeneration(t, id)
	}
}

func TestChannelTypeTransitionsRotateCodexGenerationBothDirections(t *testing.T) {
	useTestChannelDB(t)
	cache.InitCacheManager()
	logger.SetupLogger()

	const channelID = 7210
	insertTestChannel(t, &Channel{Id: channelID, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled, Name: "switch", Key: "key", Group: "default", Models: "gpt-5"})
	before, err := cache.GetOrInitCodexUsageGeneration(channelID)
	if err != nil {
		t.Fatal(err)
	}

	leaving := &Channel{Id: channelID, Type: config.ChannelTypeOpenAI, Status: config.ChannelStatusEnabled, Name: "switch", Key: "key", Group: "default", Models: "gpt-4o"}
	if err := leaving.UpdateRaw(true); err != nil {
		t.Fatal(err)
	}
	afterLeave, err := cache.GetOrInitCodexUsageGeneration(channelID)
	if err != nil || afterLeave == before {
		t.Fatalf("leaving Codex must rotate generation: before=%q after=%q err=%v", before, afterLeave, err)
	}

	returning := &Channel{Id: channelID, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled, Name: "switch", Key: "key", Group: "default", Models: "gpt-5"}
	if err := returning.UpdateRaw(true); err != nil {
		t.Fatal(err)
	}
	afterReturn, err := cache.GetOrInitCodexUsageGeneration(channelID)
	if err != nil || afterReturn == afterLeave {
		t.Fatalf("returning to Codex must rotate generation: before=%q after=%q err=%v", afterLeave, afterReturn, err)
	}
}
