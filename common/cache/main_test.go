package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	commonredis "one-api/common/redis"
	"one-api/internal/testutil/fakeredis"
)

type doneObservedContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *doneObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

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

func TestGetOrSetCacheContextReturnsLoaderValueWhenCacheIsUnavailable(t *testing.T) {
	originalCacheClient, originalKVCache := cacheClient, kvCache
	cacheClient, kvCache = nil, nil
	t.Cleanup(func() {
		cacheClient, kvCache = originalCacheClient, originalKVCache
	})

	value, err := GetOrSetCacheContext(context.Background(), "unavailable-cache", time.Minute, func(context.Context) (string, error) {
		return "database-value", nil
	}, time.Second)
	if err != nil || value != "database-value" {
		t.Fatalf("expected the authoritative loader value despite cache failure, got %q, %v", value, err)
	}
}

func TestGetOrSetCacheContextSingleflightsConcurrentMisses(t *testing.T) {
	originalCacheClient, originalKVCache := cacheClient, kvCache
	cacheClient, kvCache = nil, nil
	t.Cleanup(func() {
		cacheClient, kvCache = originalCacheClient, originalKVCache
	})

	var calls atomic.Int32
	loaderStarted := make(chan struct{})
	releaseLoader := make(chan struct{})
	loader := func(context.Context) (string, error) {
		if calls.Add(1) == 1 {
			close(loaderStarted)
		}
		<-releaseLoader
		return "database-value", nil
	}
	type result struct {
		value string
		err   error
	}
	results := make(chan result, 2)
	go func() {
		value, err := GetOrSetCacheContext(context.Background(), "singleflight-unavailable-cache", time.Minute, loader, time.Second)
		results <- result{value: value, err: err}
	}()
	<-loaderStarted
	secondWaiting := make(chan struct{})
	secondCtx := &doneObservedContext{Context: context.Background(), observed: secondWaiting}
	go func() {
		value, err := GetOrSetCacheContext(secondCtx, "singleflight-unavailable-cache", time.Minute, loader, time.Second)
		results <- result{value: value, err: err}
	}()
	// GetOrSet evaluates Done only after DoChan has registered this waiter.
	<-secondWaiting
	close(releaseLoader)
	for range 2 {
		got := <-results
		if got.err != nil || got.value != "database-value" {
			t.Fatalf("unexpected shared loader result: %+v", got)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected one authoritative loader call, got %d", got)
	}
}

func TestGetOrSetCacheContextCallerCancellationDoesNotPoisonSharedLoad(t *testing.T) {
	originalCacheClient, originalKVCache := cacheClient, kvCache
	cacheClient, kvCache = nil, nil
	t.Cleanup(func() {
		cacheClient, kvCache = originalCacheClient, originalKVCache
	})

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	var calls atomic.Int32
	loaderStarted := make(chan struct{})
	releaseLoader := make(chan struct{})
	loader := func(ctx context.Context) (string, error) {
		if calls.Add(1) == 1 {
			close(loaderStarted)
		}
		select {
		case <-releaseLoader:
			return "database-value", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	firstResult := make(chan error, 1)
	go func() {
		_, err := GetOrSetCacheContext(firstCtx, "detached-singleflight-loader", time.Minute, loader, time.Second)
		firstResult <- err
	}()
	<-loaderStarted

	secondWaiting := make(chan struct{})
	secondCtx := &doneObservedContext{Context: context.Background(), observed: secondWaiting}
	secondResult := make(chan struct {
		value string
		err   error
	}, 1)
	go func() {
		value, err := GetOrSetCacheContext(secondCtx, "detached-singleflight-loader", time.Minute, loader, time.Second)
		secondResult <- struct {
			value string
			err   error
		}{value: value, err: err}
	}()
	// GetOrSet evaluates Done only after DoChan has registered this waiter.
	<-secondWaiting
	cancelFirst()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled caller to return its own cancellation, got %v", err)
	}
	close(releaseLoader)
	got := <-secondResult
	if got.err != nil || got.value != "database-value" {
		t.Fatalf("expected healthy caller to receive the shared loader result, got %+v", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected both callers to share one detached loader, got %d calls", calls.Load())
	}
}

func TestDeleteCacheMissingKeyIsNoOp(t *testing.T) {
	useTestCacheManager(t)

	if err := DeleteCache("missing-key"); err != nil {
		t.Fatalf("expected deleting a missing cache key to be a no-op, got %v", err)
	}
}

func TestDeleteCacheContextHandlesRedisMissCancellationAndExistingKey(t *testing.T) {
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

	const key = "redis-single"
	if err := SetCache(key, "value", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := DeleteCacheContext(context.Background(), "redis-missing"); err != nil {
		t.Fatalf("expected Redis delete miss to succeed, got %v", err)
	}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := DeleteCacheContext(canceledCtx, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled Redis deletion to stop, got %v", err)
	}
	if got, err := GetCache[string](key); err != nil || got != "value" {
		t.Fatalf("canceled Redis deletion changed the value: got=%q err=%v", got, err)
	}
	if err := DeleteCacheContext(context.Background(), key); err != nil {
		t.Fatalf("delete existing Redis key: %v", err)
	}
	if _, err := GetCache[string](key); !errors.Is(err, CacheNotFound) {
		t.Fatalf("expected Redis key to be deleted, got %v", err)
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
