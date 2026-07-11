package cache

import (
	"context"
	"errors"
	"testing"
	"time"

	"one-api/common/config"
	commonredis "one-api/common/redis"
	"one-api/internal/testutil/fakeredis"
)

func useTestCacheManager(t *testing.T) {
	t.Helper()

	originalRedisEnabled := config.RedisEnabled
	originalCacheClient := cacheClient
	originalKVCache := kvCache

	config.RedisEnabled = false
	InitCacheManager()

	t.Cleanup(func() {
		config.RedisEnabled = originalRedisEnabled
		cacheClient = originalCacheClient
		kvCache = originalKVCache
	})
}

func TestLocalCacheCapacitySupportsBackgroundUsagePreviews(t *testing.T) {
	if localCacheCapacityBytes < 16*1024*1024 {
		t.Fatalf("expected at least 16 MiB of local cache, got %d bytes", localCacheCapacityBytes)
	}
}

func TestGetCacheContextHandlesNilAndCanceledLocalContext(t *testing.T) {
	useTestCacheManager(t)
	if err := SetCache("context-local", "value", time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, err := GetCacheContext[string](nil, "context-local"); err != nil || got != "value" {
		t.Fatalf("nil context read = %q, %v", got, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if _, err := GetCacheContext[string](ctx, "context-local"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled local read returned %v", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("canceled local read took %s", elapsed)
	}
}

func TestGetCacheContextCanceledRedisReadReturnsImmediately(t *testing.T) {
	server, err := fakeredis.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	originalRedisEnabled, originalRedis := config.RedisEnabled, commonredis.RDB
	originalCacheClient, originalKVCache := cacheClient, kvCache
	config.RedisEnabled, commonredis.RDB = true, server.Client()
	InitCacheManager()
	t.Cleanup(func() {
		config.RedisEnabled, commonredis.RDB = originalRedisEnabled, originalRedis
		cacheClient, kvCache = originalCacheClient, originalKVCache
	})
	if err := SetCache("context-redis", "value", time.Minute); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if _, err := GetCacheContext[string](ctx, "context-redis"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Redis read returned %v", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("canceled Redis read took %s", elapsed)
	}
}

func TestSetAndDeleteCacheContextHandleNilAndCancellation(t *testing.T) {
	useTestCacheManager(t)
	if err := SetCacheContext(nil, "context-write", "value", time.Minute); err != nil {
		t.Fatalf("nil-context write failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := SetCacheContext(ctx, "canceled-write", "value", time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write returned %v", err)
	}
	if _, err := GetCache[string]("canceled-write"); !errors.Is(err, CacheNotFound) {
		t.Fatalf("canceled write published a value: %v", err)
	}
	if err := DeleteCacheContext(ctx, "context-write"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled delete returned %v", err)
	}
	if got, err := GetCache[string]("context-write"); err != nil || got != "value" {
		t.Fatalf("canceled delete changed cache: %q, %v", got, err)
	}
	if err := DeleteCacheContext(nil, "context-write"); err != nil {
		t.Fatalf("nil-context delete failed: %v", err)
	}
}

func TestSetCacheReportsUninitializedManager(t *testing.T) {
	originalKVCache := kvCache
	kvCache = nil
	t.Cleanup(func() {
		kvCache = originalKVCache
	})

	if err := SetCache("key", "value", time.Minute); !errors.Is(err, CacheNotInitialized) {
		t.Fatalf("expected cache-not-initialized error, got %v", err)
	}
}

func TestDeleteCacheMissingKeyIsNoOp(t *testing.T) {
	useTestCacheManager(t)

	if err := DeleteCache("missing-key"); err != nil {
		t.Fatalf("expected deleting a missing cache key to be a no-op, got %v", err)
	}
}

func TestDeleteCacheManyUsesRedisBatchDeletionBehavior(t *testing.T) {
	server, err := fakeredis.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	originalRedisEnabled, originalRedis := config.RedisEnabled, commonredis.RDB
	originalCacheClient, originalKVCache := cacheClient, kvCache
	config.RedisEnabled, commonredis.RDB = true, server.Client()
	InitCacheManager()
	t.Cleanup(func() {
		config.RedisEnabled, commonredis.RDB = originalRedisEnabled, originalRedis
		cacheClient, kvCache = originalCacheClient, originalKVCache
	})

	for _, key := range []string{"redis-batch-a", "redis-batch-b"} {
		if err := SetCache(key, "value", time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if err := DeleteCacheMany([]string{"redis-batch-a", "redis-missing", "redis-batch-b"}); err != nil {
		t.Fatalf("expected Redis batch deletion to normalize misses, got %v", err)
	}
	for _, key := range []string{"redis-batch-a", "redis-batch-b"} {
		if _, err := GetCache[string](key); !errors.Is(err, CacheNotFound) {
			t.Fatalf("expected Redis key %s to be deleted, got %v", key, err)
		}
	}
}

func TestDeleteCacheManyRemovesExistingKeysAndNormalizesMisses(t *testing.T) {
	useTestCacheManager(t)

	for _, key := range []string{"batch-a", "batch-b"} {
		if err := SetCache(key, "value", time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if err := DeleteCacheMany([]string{"batch-a", "missing", "batch-b"}); err != nil {
		t.Fatalf("expected mixed batch deletion to succeed, got %v", err)
	}
	for _, key := range []string{"batch-a", "batch-b"} {
		if _, err := GetCache[string](key); !errors.Is(err, CacheNotFound) {
			t.Fatalf("expected %s to be deleted, got %v", key, err)
		}
	}
	if err := DeleteCacheMany([]string{"batch-a", "missing", "batch-b"}); err != nil {
		t.Fatalf("expected repeated batch deletion to be idempotent, got %v", err)
	}
}

func TestDeleteCacheRemovesExistingKeyAndStaysIdempotent(t *testing.T) {
	useTestCacheManager(t)

	const key = "existing-key"
	if err := SetCache(key, "value", time.Minute); err != nil {
		t.Fatalf("expected cache set to succeed, got %v", err)
	}

	if err := DeleteCache(key); err != nil {
		t.Fatalf("expected cache delete to succeed, got %v", err)
	}

	if _, err := GetCache[string](key); !errors.Is(err, CacheNotFound) {
		t.Fatalf("expected deleted cache key to be missing, got %v", err)
	}

	if err := DeleteCache(key); err != nil {
		t.Fatalf("expected repeated cache delete to stay idempotent, got %v", err)
	}
}
