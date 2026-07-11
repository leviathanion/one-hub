package cache

import (
	"context"
	"errors"
	"one-api/common/config"
	"one-api/common/redis"
	"time"

	"github.com/coocood/freecache"
	cacheM "github.com/eko/gocache/lib/v4/cache"
	"github.com/eko/gocache/lib/v4/marshaler"
	"github.com/eko/gocache/lib/v4/store"
	freecache_store "github.com/eko/gocache/store/freecache/v4"
	redis_store "github.com/eko/gocache/store/redis/v4"
	"golang.org/x/sync/singleflight"
)

const localCacheCapacityBytes = 16 * 1024 * 1024

var (
	cacheClient         *cacheM.Cache[any]
	kvCache             *marshaler.Marshaler
	sfGroup             singleflight.Group
	CacheTimeout        = 1 * time.Second
	CacheNotFound       = errors.New("cache not found")
	CacheNotInitialized = errors.New("cache not initialized")
)

func InitCacheManager() {
	var client *cacheM.Cache[any]
	if config.RedisEnabled {
		redisStore := redis_store.NewRedis(redis.RDB)
		client = cacheM.New[any](redisStore)
	} else {
		freecacheStore := freecache_store.NewFreecache(freecache.NewCache(localCacheCapacityBytes))
		client = cacheM.New[any](freecacheStore)
	}

	cacheClient = client
	kvCache = marshaler.New(client)
}

func GetCache[T any](key string) (T, error) {
	return GetCacheContext[T](context.Background(), key)
}

// GetCacheContext performs a cache read within the caller's operation lifetime.
// A nil context retains the historical background-context behavior.
func GetCacheContext[T any](operationCtx context.Context, key string) (T, error) {
	if operationCtx == nil {
		operationCtx = context.Background()
	}
	if err := operationCtx.Err(); err != nil {
		return *new(T), err
	}
	var val T
	if kvCache == nil {
		return *new(T), CacheNotFound
	}
	_, err := kvCache.Get(operationCtx, key, &val)
	if err != nil {
		if errors.Is(err, store.NotFound{}) {
			return *new(T), CacheNotFound
		}
		return *new(T), err
	}
	return val, nil
}

func SetCache(key string, value any, expiration time.Duration) error {
	return SetCacheContext(context.Background(), key, value, expiration)
}

// SetCacheContext performs a cache write within the caller's operation lifetime.
// A nil context retains the historical background-context behavior.
func SetCacheContext(operationCtx context.Context, key string, value any, expiration time.Duration) error {
	if operationCtx == nil {
		operationCtx = context.Background()
	}
	if err := operationCtx.Err(); err != nil {
		return err
	}
	if kvCache == nil {
		return CacheNotInitialized
	}
	return kvCache.Set(operationCtx, key, value, store.WithExpiration(expiration))
}

func DeleteCache(key string) error {
	return DeleteCacheContext(context.Background(), key)
}

func DeleteCacheContext(operationCtx context.Context, key string) error {
	return DeleteCacheManyContext(operationCtx, []string{key})
}

func DeleteCacheMany(keys []string) error {
	return DeleteCacheManyContext(context.Background(), keys)
}

// DeleteCacheManyContext gives invalidation one bounded operation in Redis and
// one fast in-process pass locally. Delete misses are success in both modes.
func DeleteCacheManyContext(operationCtx context.Context, keys []string) error {
	if operationCtx == nil {
		operationCtx = context.Background()
	}
	if err := operationCtx.Err(); err != nil {
		return err
	}
	if len(keys) == 0 || kvCache == nil || cacheClient == nil {
		return nil
	}

	if config.RedisEnabled && redis.GetRedisClient() != nil {
		deleteCtx, cancel := context.WithTimeout(operationCtx, CacheTimeout)
		defer cancel()
		return redis.GetRedisClient().Del(deleteCtx, keys...).Err()
	}

	for _, key := range keys {
		err := kvCache.Delete(operationCtx, key)
		if err == nil {
			continue
		}

		// freecache reports a generic error for delete-miss. Normalize it without
		// making callers distinguish adapter-specific behavior.
		if _, getErr := cacheClient.Get(operationCtx, key); errors.Is(getErr, store.NotFound{}) {
			continue
		}
		return err
	}
	return nil
}

func GetOrSetCache[T any](key string, expiration time.Duration, fn func() (T, error), timeout time.Duration) (T, error) {
	v, err := GetCache[T](key)
	if err == nil {
		return v, nil
	}

	if !errors.Is(err, CacheNotFound) {
		return *new(T), err
	}

	result := sfGroup.DoChan(key, func() (interface{}, error) {
		v, err := fn()
		if err != nil {
			return nil, err
		}

		SetCache(key, v, expiration)

		return v, nil
	})

	t := time.After(timeout)

	select {
	case r := <-result:
		v, ok := r.Val.(T)
		if !ok {
			return *new(T), errors.New("类型断言失败")
		}
		return v, r.Err
	case <-t:
		return *new(T), errors.New("超时")
	}
}
