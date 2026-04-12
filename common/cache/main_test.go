package cache

import (
	"errors"
	"testing"
	"time"

	"one-api/common/config"
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

func TestDeleteCacheMissingKeyIsNoOp(t *testing.T) {
	useTestCacheManager(t)

	if err := DeleteCache("missing-key"); err != nil {
		t.Fatalf("expected deleting a missing cache key to be a no-op, got %v", err)
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
