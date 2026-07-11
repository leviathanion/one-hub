package model

import (
	"context"
	"errors"
	"testing"

	"one-api/common/config"

	"gorm.io/gorm"
)

func keepRound19SQLiteDatabaseAlive(t *testing.T) {
	t.Helper()
	sqlDB, err := DB.DB()
	if err != nil {
		t.Fatalf("get sqlite pool: %v", err)
	}
	keeper, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("keep sqlite database alive: %v", err)
	}
	t.Cleanup(func() { _ = keeper.Close() })
}

func TestBatchDelModelChannelsWithContextCancellationRollsBack(t *testing.T) {
	useTestChannelDB(t)
	keepRound19SQLiteDatabaseAlive(t)
	insertTestChannel(t, &Channel{
		Id: 19001, Type: config.ChannelTypeOpenAI, Name: "round19-batch",
		Key: "sk-round19", Group: "default", Models: "gpt-4o,gpt-5",
	})

	ctx, cancel := context.WithCancel(context.Background())
	callback := "test:cancel_batch_del_model:" + t.Name()
	if err := DB.Callback().Update().After("gorm:update").Register(callback, func(*gorm.DB) {
		cancel()
	}); err != nil {
		t.Fatalf("register cancellation callback: %v", err)
	}
	t.Cleanup(func() { _ = DB.Callback().Update().Remove(callback) })

	count, err := BatchDelModelChannelsWithContext(ctx, &BatchDelModelChannelsParams{
		Ids: []int{19001}, Value: "gpt-5",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got count=%d err=%v", count, err)
	}
	if count != 0 {
		t.Fatalf("rolled-back batch reported count %d", count)
	}

	var channel Channel
	if err := DB.First(&channel, 19001).Error; err != nil {
		t.Fatalf("reload channel: %v", err)
	}
	if channel.Models != "gpt-4o,gpt-5" {
		t.Fatalf("canceled transaction was committed: models=%q", channel.Models)
	}
}

func TestChangeChannelsTagStatusWithContextCancellationRollsBack(t *testing.T) {
	useTestChannelDB(t)
	keepRound19SQLiteDatabaseAlive(t)
	insertTestChannel(t, &Channel{
		Id: 19002, Type: config.ChannelTypeOpenAI, Name: "round19-tag",
		Key: "sk-round19", Group: "default", Tag: "round19", Models: "gpt-4o",
		Status: config.ChannelStatusEnabled,
	})

	ctx, cancel := context.WithCancel(context.Background())
	callback := "test:cancel_tag_status:" + t.Name()
	if err := DB.Callback().Update().After("gorm:update").Register(callback, func(*gorm.DB) {
		cancel()
	}); err != nil {
		t.Fatalf("register cancellation callback: %v", err)
	}
	t.Cleanup(func() { _ = DB.Callback().Update().Remove(callback) })

	err := ChangeChannelsTagStatusWithContext(ctx, "round19", config.ChannelStatusManuallyDisabled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}

	var channel Channel
	if err := DB.First(&channel, 19002).Error; err != nil {
		t.Fatalf("reload channel: %v", err)
	}
	if channel.Status != config.ChannelStatusEnabled {
		t.Fatalf("canceled transaction was committed: status=%d", channel.Status)
	}
}
