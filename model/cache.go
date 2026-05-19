package model

import (
	"context"
	"fmt"
	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/redis"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	TokenCacheSeconds           = 0
	UserGroupCacheKey           = "user_group:%d"
	UserTokensKey               = "token:%s"
	UsernameCacheKey            = "user_name:%d"
	UserQuotaCacheKey           = "user_quota:%d"
	UserEnabledCacheKey         = "user_enabled:%d"
	UserRealtimeQuotaKey        = "user_realtime_quota:%d"
	UserRealtimeQuotaExpiration = 24 * time.Hour

	OldUserTokensCacheKey = "old_user_tokens_cache"
)

var userQuotaCacheRepairQueue = map[int]userQuotaCacheRepairJob{}
var userQuotaCacheRepairQueueMu sync.Mutex
var userQuotaCacheRepairWorkerOnce sync.Once

const (
	userQuotaCacheRepairInitialBackoff = time.Minute
	userQuotaCacheRepairMaxBackoff     = 30 * time.Minute
)

type userQuotaCacheRepairJob struct {
	RefreshUserQuota   bool
	RealtimeQuotaDelta int
	LastReason         string
	RetryCount         int
	NextAttemptAt      time.Time
}

func EnqueueUserQuotaCacheRepair(userID int, reason string) {
	if userID <= 0 {
		return
	}
	enqueueUserQuotaCacheRepairJob(userID, userQuotaCacheRepairJob{
		RefreshUserQuota: true,
		LastReason:       strings.TrimSpace(reason),
	})
}

func EnqueueUserRealtimeQuotaCacheDecreaseRepair(userID int, delta int, reason string) {
	if userID <= 0 || delta <= 0 {
		return
	}
	enqueueUserQuotaCacheRepairJob(userID, userQuotaCacheRepairJob{
		RealtimeQuotaDelta: delta,
		LastReason:         strings.TrimSpace(reason),
	})
}

func enqueueUserQuotaCacheRepairJob(userID int, job userQuotaCacheRepairJob) {
	if userID <= 0 {
		return
	}
	userQuotaCacheRepairQueueMu.Lock()
	defer userQuotaCacheRepairQueueMu.Unlock()
	if userQuotaCacheRepairQueue == nil {
		userQuotaCacheRepairQueue = map[int]userQuotaCacheRepairJob{}
	}

	existing := userQuotaCacheRepairQueue[userID]
	existing.RefreshUserQuota = existing.RefreshUserQuota || job.RefreshUserQuota
	existing.RealtimeQuotaDelta += job.RealtimeQuotaDelta
	if reason := strings.TrimSpace(job.LastReason); reason != "" {
		existing.LastReason = reason
	}
	if job.RetryCount > existing.RetryCount {
		existing.RetryCount = job.RetryCount
	}
	if existing.NextAttemptAt.IsZero() || (!job.NextAttemptAt.IsZero() && job.NextAttemptAt.After(existing.NextAttemptAt)) {
		existing.NextAttemptAt = job.NextAttemptAt
	}
	if !existing.RefreshUserQuota && existing.RealtimeQuotaDelta <= 0 {
		delete(userQuotaCacheRepairQueue, userID)
		return
	}
	userQuotaCacheRepairQueue[userID] = existing
}

func StartUserQuotaCacheRepairWorker(ctx context.Context, interval time.Duration) {
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = time.Minute
	}
	userQuotaCacheRepairWorkerOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					RepairQueuedUserQuotaCachesOnce(ctx)
				}
			}
		}()
	})
}

func RepairQueuedUserQuotaCachesOnce(ctx context.Context) int {
	if ctx == nil {
		ctx = context.Background()
	}
	repaired := 0

	userQuotaCacheRepairQueueMu.Lock()
	jobs := userQuotaCacheRepairQueue
	userQuotaCacheRepairQueue = map[int]userQuotaCacheRepairJob{}
	userQuotaCacheRepairQueueMu.Unlock()

	for userID, job := range jobs {
		if userID <= 0 {
			continue
		}
		now := time.Now()
		if !job.NextAttemptAt.IsZero() && now.Before(job.NextAttemptAt) {
			enqueueUserQuotaCacheRepairJob(userID, job)
			continue
		}
		failed := userQuotaCacheRepairJob{LastReason: job.LastReason}
		if job.RefreshUserQuota {
			if err := CacheUpdateUserQuota(userID); err != nil {
				logger.LogError(ctx, fmt.Sprintf("error repair user quota cache user=%d reason=%s: %v", userID, job.LastReason, err))
				failed.RefreshUserQuota = true
			}
		}
		if job.RealtimeQuotaDelta > 0 {
			if _, err := CacheDecreaseUserRealtimeQuota(userID, job.RealtimeQuotaDelta); err != nil {
				logger.LogError(ctx, fmt.Sprintf("error repair realtime quota cache user=%d delta=%d reason=%s: %v", userID, job.RealtimeQuotaDelta, job.LastReason, err))
				failed.RealtimeQuotaDelta = job.RealtimeQuotaDelta
			}
		}
		if failed.RefreshUserQuota || failed.RealtimeQuotaDelta > 0 {
			failed.RetryCount = job.RetryCount + 1
			failed.NextAttemptAt = now.Add(userQuotaCacheRepairBackoff(failed.RetryCount))
			enqueueUserQuotaCacheRepairJob(userID, failed)
			continue
		}
		repaired++
	}
	return repaired
}

func userQuotaCacheRepairBackoff(retryCount int) time.Duration {
	if retryCount <= 1 {
		return userQuotaCacheRepairInitialBackoff
	}
	backoff := userQuotaCacheRepairInitialBackoff
	for i := 1; i < retryCount; i++ {
		if backoff >= userQuotaCacheRepairMaxBackoff/2 {
			return userQuotaCacheRepairMaxBackoff
		}
		backoff *= 2
	}
	if backoff > userQuotaCacheRepairMaxBackoff {
		return userQuotaCacheRepairMaxBackoff
	}
	return backoff
}

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

func CacheGetUserGroup(id int) (group string, err error) {
	if !config.RedisEnabled {
		return GetUserGroup(id)
	}

	group, err = cache.GetOrSetCache(
		fmt.Sprintf(UserGroupCacheKey, id),
		time.Duration(TokenCacheSeconds)*time.Second,
		func() (string, error) {
			groupId, err := GetUserGroup(id)
			if err != nil {
				return "", err
			}
			return groupId, nil
		},
		cache.CacheTimeout)

	return group, err
}

func CacheGetUserQuota(id int) (quota int, err error) {
	if !config.RedisEnabled {
		return GetUserQuota(id)
	}
	quotaString, err := redis.RedisGet(fmt.Sprintf(UserQuotaCacheKey, id))
	if err != nil {
		quota, err = GetUserQuota(id)
		if err != nil {
			return 0, err
		}
		err = redis.RedisSet(fmt.Sprintf(UserQuotaCacheKey, id), fmt.Sprintf("%d", quota), time.Duration(TokenCacheSeconds)*time.Second)
		if err != nil {
			logger.SysError("Redis set user quota error: " + err.Error())
		}
		return quota, err
	}
	quota, err = strconv.Atoi(quotaString)
	return quota, err
}

func CacheUpdateUserQuota(id int) error {
	if !config.RedisEnabled {
		return nil
	}
	quota, err := GetUserQuota(id)
	if err != nil {
		return err
	}
	err = redis.RedisSet(fmt.Sprintf(UserQuotaCacheKey, id), fmt.Sprintf("%d", quota), time.Duration(TokenCacheSeconds)*time.Second)
	return err
}

func CacheInvalidateUserQuota(id int) error {
	if !config.RedisEnabled {
		return nil
	}
	return redis.RedisDel(fmt.Sprintf(UserQuotaCacheKey, id))
}

func CacheDecreaseUserQuota(id int, quota int) error {
	if !config.RedisEnabled {
		return nil
	}
	err := redis.RedisDecrease(fmt.Sprintf(UserQuotaCacheKey, id), int64(quota))
	return err
}

var (
	decreaseUserQuotaIfPresentScript = redis.NewScript(`
		local key = KEYS[1]
		local delta = tonumber(ARGV[1])
		if redis.call("EXISTS", key) == 0 then
			return 0
		end
		redis.call("DECRBY", key, delta)
		return 1
	`)
)

func CacheDecreaseUserQuotaIfPresent(id int, quota int) (bool, error) {
	if !config.RedisEnabled {
		return false, nil
	}
	key := fmt.Sprintf(UserQuotaCacheKey, id)
	updated, err := decreaseUserQuotaIfPresentScript.Run(context.Background(), redis.GetRedisClient(), []string{key}, quota).Int64()
	if err != nil {
		return false, err
	}
	return updated == 1, nil
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
	if !config.RedisEnabled {
		return GetUsernameById(id), nil
	}

	username, err = cache.GetOrSetCache(
		fmt.Sprintf(UsernameCacheKey, id),
		time.Duration(TokenCacheSeconds)*time.Second,
		func() (string, error) {
			username := GetUsernameById(id)
			if username == "" {
				return "", fmt.Errorf("user %d not found", id)
			}

			return username, nil
		},
		cache.CacheTimeout)

	return username, err
}

func CacheDecreaseUserRealtimeQuota(id int, quota int) (int64, error) {
	if !config.RedisEnabled {
		return 0, nil
	}
	if quota > 0 {
		key := fmt.Sprintf(UserRealtimeQuotaKey, id)
		exists, err := redis.RedisExists(key)
		if err != nil {
			return 0, fmt.Errorf("检查用户实时配额缓存失败: %w", err)
		}
		if !exists {
			return 0, fmt.Errorf("用户实时配额缓存不存在: user_id=%d", id)
		}
	}
	return CacheUpdateUserRealtimeQuota(id, -quota)
}

func CacheIncreaseUserRealtimeQuota(id int, quota int) (int64, error) {
	if !config.RedisEnabled {
		return 0, nil
	}
	return CacheUpdateUserRealtimeQuota(id, quota)
}

var (
	updateQuotaScript = redis.NewScript(`
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
	`)
)

func CacheUpdateUserRealtimeQuota(id int, quota int) (int64, error) {
	if !config.RedisEnabled {
		return 0, nil
	}
	key := fmt.Sprintf(UserRealtimeQuotaKey, id)

	newValue, err := updateQuotaScript.Run(context.Background(), redis.GetRedisClient(), []string{key}, quota, int(UserRealtimeQuotaExpiration.Seconds())).Int64()
	if err != nil {
		return 0, fmt.Errorf("更新用户配额失败: %w", err)
	}
	if quota < 0 && newValue == 0 {
		exists, existsErr := redis.RedisExists(key)
		if existsErr != nil {
			return 0, fmt.Errorf("检查用户实时配额缓存失败: %w", existsErr)
		}
		if !exists {
			return 0, fmt.Errorf("用户实时配额缓存不存在: user_id=%d", id)
		}
	}

	return newValue, nil
}

func HandleOldTokenMaxId() {
	if config.OldTokenMaxId == 0 || !config.RedisEnabled {
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
			Where("id <= ?", config.OldTokenMaxId).
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
