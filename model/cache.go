package model

import (
	"context"
	"fmt"
	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/redis"
	"time"
)

var (
	TokenCacheSeconds = 0

	UserTokensKey       = "token:%s"
	UsernameCacheKey    = "user_name:%d"
	UserEnabledCacheKey = "user_enabled:%d"

	OldUserTokensCacheKey = "old_user_tokens_cache"
)

func CacheGetTokenByKey(key string) (*Token, error) {
	if !config.RedisEnabled {
		return GetTokenByKey(key)
	}

	token, err := cache.GetOrSetCache(
		fmt.Sprintf(UserTokensKey, key),
		time.Duration(TokenCacheSeconds)*time.Second,
		func() (*Token, error) {
			return GetTokenByKey(key)
		},
		cache.CacheTimeout)

	return token, err
}

func CacheIsUserEnabled(userId int) (bool, error) {
	if !config.RedisEnabled {
		return IsUserEnabled(userId)
	}

	enabled, err := cache.GetOrSetCache(
		fmt.Sprintf(UserEnabledCacheKey, userId),
		time.Duration(TokenCacheSeconds)*time.Second,
		func() (bool, error) {
			enabled, err := IsUserEnabled(userId)
			if err != nil {
				return false, err
			}
			return enabled, nil
		},
		cache.CacheTimeout)

	return enabled, err
}

func CacheGetUsername(id int) (username string, err error) {
	return CacheGetUsernameWithContext(context.Background(), id)
}

func CacheGetUsernameWithContext(ctx context.Context, id int) (username string, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !config.RedisEnabled {
		return GetUsernameByIdWithContext(ctx, id)
	}
	key := fmt.Sprintf(UsernameCacheKey, id)
	return cache.GetOrSetCacheContext(
		ctx,
		key,
		time.Duration(TokenCacheSeconds)*time.Second,
		func(loaderCtx context.Context) (string, error) {
			username, err := GetUsernameByIdWithContext(loaderCtx, id)
			if err != nil {
				return "", err
			}
			if username == "" {
				return "", fmt.Errorf("user %d not found", id)
			}
			return username, nil
		},
		cache.CacheTimeout,
	)
}

func HandleOldTokenMaxId() {
	oldTokenMaxID := config.GlobalOption.RuntimeSnapshot().Int("OldTokenMaxId", config.OldTokenMaxId)
	if oldTokenMaxID == 0 || !config.RedisEnabled {
		return
	}

	// 检测OldUserTokensCacheKey是否存在
	exists, _ := redis.RedisExists(OldUserTokensCacheKey)
	if exists {
		return
	}
	const batchSize = 1000
	var offset int

	for {
		var tokenKeys []interface{}
		result := DB.Model(&Token{}).
			Where("id <= ?", oldTokenMaxID).
			Limit(batchSize).
			Offset(offset).
			Pluck("key", &tokenKeys)

		if result.Error != nil {
			logger.SysError("查询旧token失败: " + result.Error.Error())
			return
		}

		if len(tokenKeys) == 0 {
			if offset == 0 {
				logger.SysLog("没有找到旧token")
			}
			break
		}

		if err := redis.RedisSAdd(OldUserTokensCacheKey, tokenKeys...); err != nil {
			logger.SysError("添加旧token到Redis失败: " + err.Error())
		}

		logger.SysLog(fmt.Sprintf("已处理 %d 个旧token", offset+len(tokenKeys)))
		offset += batchSize

		time.Sleep(100 * time.Millisecond)
	}
}
