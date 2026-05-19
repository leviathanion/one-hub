package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/logger"
	commonredis "one-api/common/redis"
	"one-api/internal/testutil/fakeredis"

	"go.uber.org/zap"
)

func TestRepairQueuedUserQuotaCachesOnceConsumesSuccessfulEntries(t *testing.T) {
	logger.Logger = zap.NewNop()

	previousQueue := userQuotaCacheRepairQueue
	previousRedisEnabled := config.RedisEnabled
	userQuotaCacheRepairQueue = map[int]userQuotaCacheRepairJob{}
	config.RedisEnabled = false
	t.Cleanup(func() {
		userQuotaCacheRepairQueue = previousQueue
		config.RedisEnabled = previousRedisEnabled
	})

	EnqueueUserQuotaCacheRepair(42, "rollback_refresh_failed")
	EnqueueUserQuotaCacheRepair(0, "ignored")

	if repaired := RepairQueuedUserQuotaCachesOnce(context.Background()); repaired != 1 {
		t.Fatalf("expected one queued quota cache repair to be consumed, got %d", repaired)
	}
	if repaired := RepairQueuedUserQuotaCachesOnce(context.Background()); repaired != 0 {
		t.Fatalf("expected successful repair to delete queue entry, got %d repeated repairs", repaired)
	}
}

func TestRepairQueuedUserQuotaCachesOnceReplaysRealtimeDelta(t *testing.T) {
	logger.Logger = zap.NewNop()

	server, err := fakeredis.Start()
	if err != nil {
		t.Fatalf("expected fake redis server to start, got %v", err)
	}
	defer server.Close()
	registerRealtimeQuotaTestScript(server)

	previousQueue := userQuotaCacheRepairQueue
	previousRedisEnabled := config.RedisEnabled
	previousRedisClient := commonredis.RDB
	userQuotaCacheRepairQueue = map[int]userQuotaCacheRepairJob{}
	config.RedisEnabled = true
	commonredis.RDB = server.Client()
	t.Cleanup(func() {
		userQuotaCacheRepairQueue = previousQueue
		config.RedisEnabled = previousRedisEnabled
		commonredis.RDB = previousRedisClient
	})

	key := fmt.Sprintf(UserRealtimeQuotaKey, 42)
	server.SetRaw(key, "9")
	EnqueueUserRealtimeQuotaCacheDecreaseRepair(42, 4, "cleanup_failed")

	if repaired := RepairQueuedUserQuotaCachesOnce(context.Background()); repaired != 1 {
		t.Fatalf("expected queued realtime quota repair to be consumed, got %d", repaired)
	}
	if got, _ := server.GetRaw(key); got != "5" {
		t.Fatalf("expected realtime quota repair to replay the queued delta, got %q", got)
	}
	if repaired := RepairQueuedUserQuotaCachesOnce(context.Background()); repaired != 0 {
		t.Fatalf("expected successful realtime quota repair to clear the queue, got %d", repaired)
	}
}

func TestCacheDecreaseUserRealtimeQuotaMissingKeyReturnsError(t *testing.T) {
	logger.Logger = zap.NewNop()

	server, err := fakeredis.Start()
	if err != nil {
		t.Fatalf("expected fake redis server to start, got %v", err)
	}
	defer server.Close()
	registerRealtimeQuotaTestScript(server)

	previousRedisEnabled := config.RedisEnabled
	previousRedisClient := commonredis.RDB
	config.RedisEnabled = true
	commonredis.RDB = server.Client()
	t.Cleanup(func() {
		config.RedisEnabled = previousRedisEnabled
		commonredis.RDB = previousRedisClient
	})

	if _, err := CacheDecreaseUserRealtimeQuota(42, 4); err == nil {
		t.Fatal("expected missing realtime quota cache key to return an explicit error")
	}
}

func TestRepairQueuedUserQuotaCachesOnceBacksOffAndMergesRealtimeDelta(t *testing.T) {
	logger.Logger = zap.NewNop()

	server, err := fakeredis.Start()
	if err != nil {
		t.Fatalf("expected fake redis server to start, got %v", err)
	}
	defer server.Close()
	registerRealtimeQuotaTestScript(server)

	previousQueue := userQuotaCacheRepairQueue
	previousRedisEnabled := config.RedisEnabled
	previousRedisClient := commonredis.RDB
	userQuotaCacheRepairQueue = map[int]userQuotaCacheRepairJob{}
	config.RedisEnabled = true
	commonredis.RDB = server.Client()
	t.Cleanup(func() {
		userQuotaCacheRepairQueue = previousQueue
		config.RedisEnabled = previousRedisEnabled
		commonredis.RDB = previousRedisClient
	})

	key := fmt.Sprintf(UserRealtimeQuotaKey, 42)
	server.SetRaw(key, "10")
	EnqueueUserRealtimeQuotaCacheDecreaseRepair(42, 3, "cleanup_failed")
	server.FailNext("EVALSHA", "forced evalsha failure")

	attemptStarted := time.Now()
	if repaired := RepairQueuedUserQuotaCachesOnce(context.Background()); repaired != 0 {
		t.Fatalf("expected failed repair to stay queued, got %d repaired entries", repaired)
	}
	if got, _ := server.GetRaw(key); got != "10" {
		t.Fatalf("expected failed repair not to mutate realtime quota, got %q", got)
	}

	userQuotaCacheRepairQueueMu.Lock()
	failed := userQuotaCacheRepairQueue[42]
	userQuotaCacheRepairQueueMu.Unlock()
	if failed.RealtimeQuotaDelta != 3 || failed.RetryCount != 1 || failed.NextAttemptAt.Before(attemptStarted) {
		t.Fatalf("expected failed repair to requeue with backoff, got %+v", failed)
	}

	EnqueueUserRealtimeQuotaCacheDecreaseRepair(42, 2, "second_cleanup_failed")
	if repaired := RepairQueuedUserQuotaCachesOnce(context.Background()); repaired != 0 {
		t.Fatalf("expected backoff window to suppress immediate retry, got %d repaired entries", repaired)
	}
	if got, _ := server.GetRaw(key); got != "10" {
		t.Fatalf("expected backoff-suppressed repair not to mutate realtime quota, got %q", got)
	}

	userQuotaCacheRepairQueueMu.Lock()
	merged := userQuotaCacheRepairQueue[42]
	userQuotaCacheRepairQueueMu.Unlock()
	if merged.RealtimeQuotaDelta != 5 || merged.RetryCount != 1 || merged.LastReason != "second_cleanup_failed" || merged.NextAttemptAt.IsZero() {
		t.Fatalf("expected queued repair to merge delta and preserve retry state, got %+v", merged)
	}
}

func registerRealtimeQuotaTestScript(server *fakeredis.Server) {
	server.RegisterLuaScript(`
		local key = KEYS[1]
		local increment = tonumber(ARGV[1])
		local expiration = tonumber(ARGV[2])

		local exists = redis.call("EXISTS", key)
		if exists == 0 then
			if increment < 0 then
				return 0
			end
			redis.call("SET", key, "0", "EX", expiration)
		end

		local newValue = redis.call("INCRBY", key, increment)
		redis.call("EXPIRE", key, expiration)

		return newValue
	`, func(keys, args []string) int64 {
		currentRaw, exists := server.GetRaw(keys[0])
		currentValue := int64(0)
		if exists {
			fmt.Sscanf(currentRaw, "%d", &currentValue)
		}
		var increment int64
		fmt.Sscanf(args[0], "%d", &increment)
		if !exists && increment < 0 {
			return 0
		}
		newValue := currentValue + increment
		server.SetRaw(keys[0], fmt.Sprintf("%d", newValue))
		return newValue
	})
}
