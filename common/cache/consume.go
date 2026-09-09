package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"one-api/common/config"
	commonredis "one-api/common/redis"

	"github.com/eko/gocache/lib/v4/store"
	"github.com/redis/go-redis/v9"
	"github.com/vmihailenco/msgpack/v5"
)

// 本地写入、删除与消费共享固定锁，避免读出旧值后删除并发写入的新值。
var localCacheMutationMu sync.Mutex

const consumeCacheScript = `
local value = redis.call('GET', KEYS[1])
if value then
  redis.call('DEL', KEYS[1])
end
return value
`

// ConsumeCache 只在成功取出并删除时返回值，用于 OAuth 等一次性状态。
func ConsumeCache[T any](key string) (T, error) {
	return ConsumeCacheContext[T](context.Background(), key)
}

func ConsumeCacheContext[T any](operationCtx context.Context, key string) (T, error) {
	var zero T
	if operationCtx == nil {
		operationCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(operationCtx, CacheTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if kvCache == nil || cacheClient == nil {
		return zero, CacheNotInitialized
	}

	var raw any
	var err error
	if config.RedisEnabled {
		backend := commonredis.GetRedisClient()
		if backend == nil {
			return zero, CacheNotInitialized
		}
		// 普通缓存保留现有重试策略；一次性消费使用同一后端配置的临时连接，
		// 禁止丢失回复后的重放，且在操作结束时关闭连接。
		options := *backend.Options()
		options.MaxRetries = -1
		options.ContextTimeoutEnabled = true
		options.PoolSize, options.MaxActiveConns = 1, 1
		options.MinIdleConns, options.MaxIdleConns = 0, 1
		client := redis.NewClient(&options)
		defer client.Close()
		raw, err = client.Eval(ctx, consumeCacheScript, []string{key}).Result()
		if errors.Is(err, redis.Nil) {
			err = CacheNotFound
		}
	} else {
		raw, err = consumeLocalCache(ctx, key)
	}
	if err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	var payload []byte
	switch value := raw.(type) {
	case []byte:
		payload = value
	case string:
		payload = []byte(value)
	default:
		return zero, fmt.Errorf("无法解码已消费的缓存值类型 %T", raw)
	}
	var value T
	if err := msgpack.Unmarshal(payload, &value); err != nil {
		return zero, err
	}
	return value, nil
}

func consumeLocalCache(ctx context.Context, key string) (any, error) {
	localCacheMutationMu.Lock()
	defer localCacheMutationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := cacheClient.Get(ctx, key)
	if err != nil {
		if errors.Is(err, store.NotFound{}) {
			return nil, CacheNotFound
		}
		return nil, err
	}
	if err := cacheClient.Delete(ctx, key); err != nil {
		return nil, err
	}
	return raw, nil
}
