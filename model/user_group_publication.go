package model

import (
	"context"
	"errors"
	"fmt"
	"time"

	"one-api/common/limit"
	"one-api/common/logger"

	"gorm.io/gorm"
)

const userGroupPublicationWatchInterval = 5 * time.Second

type UserGroupPublicationStatus struct {
	PublishedVersion int64  `json:"published_version"`
	DatabaseHead     int64  `json:"database_head"`
	LastSyncError    string `json:"last_sync_error,omitempty"`
}

func (groups *UserGroupRatio) PublicationStatus() UserGroupPublicationStatus {
	groups.RLock()
	defer groups.RUnlock()
	return UserGroupPublicationStatus{groups.publishedVersion, groups.databaseHead, groups.lastSyncError}
}

func (groups *UserGroupRatio) Load() error {
	ctx, cancel := context.WithTimeout(context.Background(), publicationCommitProbeDeadline)
	defer cancel()
	return groups.SyncPublication(ctx)
}

func SyncUserGroupPublication(ctx context.Context) error {
	return GlobalUserGroupRatio.SyncPublication(ctx)
}

// 新工作先确认 SQL head，落后时同步；旧快照仅供已经开始的决策继续使用。
func EnsureUserGroupPolicyAvailable(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, publicationCommitProbeDeadline)
	defer cancel()
	return SyncUserGroupPublication(probeCtx)
}

func (groups *UserGroupRatio) SyncPublication(ctx context.Context) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err = lockPublicationReload(ctx, &groups.reloadMu); err != nil {
		groups.Lock()
		groups.lastSyncError = errorText(err)
		groups.Unlock()
		return err
	}
	defer groups.reloadMu.Unlock()
	defer func() {
		groups.Lock()
		groups.lastSyncError = errorText(err)
		groups.Unlock()
	}()

	// 与 Options 一样，用前后版本校验保证完整查询对应同一已提交版本。
	for attempt := 0; attempt < 2; attempt++ {
		head, readErr := ReadPublicationVersion(ctx, DB, PublicationOwnerUserGroup)
		if readErr != nil {
			return readErr
		}
		groups.Lock()
		groups.databaseHead = head
		current := groups.publishedVersion
		groups.Unlock()
		if head == current {
			return nil
		}
		if head < current {
			return fmt.Errorf("user group publication version %d exceeds database head %d", current, head)
		}
		var rows []*UserGroup
		if err := DB.WithContext(ctx).Where("enable = ?", true).Order("id").Find(&rows).Error; err != nil {
			return err
		}
		confirmedHead, err := ReadPublicationVersion(ctx, DB, PublicationOwnerUserGroup)
		if err != nil {
			return err
		}
		groups.Lock()
		groups.databaseHead = confirmedHead
		groups.Unlock()
		if head != confirmedHead {
			continue
		}
		groups.replacePublication(head, rows)
		return nil
	}
	return ErrPublicationVersionConflict
}

func (groups *UserGroupRatio) replacePublication(version int64, rows []*UserGroup) {
	policies := make(map[string]*UserGroup, len(rows))
	limiters := make(map[string]limit.RateLimiter, len(rows))
	publicGroups := make([]string, 0, len(rows))
	retired := make(map[string]limit.RateLimiter)

	groups.Lock()
	for _, row := range rows {
		policies[row.Symbol] = row
		old := groups.UserGroup[row.Symbol]
		if old != nil && old.APIRate == row.APIRate && groups.APILimiter[row.Symbol] != nil {
			limiters[row.Symbol] = groups.APILimiter[row.Symbol]
		} else {
			limiters[row.Symbol] = limit.NewAPILimiter(row.APIRate)
		}
		if row.Public {
			publicGroups = append(publicGroups, row.Symbol)
		}
	}
	for symbol, limiter := range groups.APILimiter {
		if limiters[symbol] != limiter {
			retired[symbol] = limiter
		}
	}
	groups.UserGroup = policies
	groups.APILimiter = limiters
	groups.PublicGroup = publicGroups
	groups.publishedVersion = version
	groups.databaseHead = version
	groups.Unlock()
	stopUserGroupAPILimiters(retired)
}

func WatchUserGroupPublication(ctx context.Context) {
	GlobalUserGroupRatio.watchPublication(ctx, userGroupPublicationWatchInterval)
}

func (groups *UserGroupRatio) watchPublication(ctx context.Context, interval time.Duration) {
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probeCtx, cancel := context.WithTimeout(ctx, publicationCommitProbeDeadline)
			err := groups.SyncPublication(probeCtx)
			cancel()
			if err != nil && logger.Logger != nil {
				logger.SysError("用户组策略同步失败: " + err.Error())
			}
		}
	}
}

func mutateUserGroupPolicy(mutate func(*gorm.DB) error) error {
	if DB == nil {
		return errors.New("database is required")
	}
	var version int64
	err := DB.Transaction(func(tx *gorm.DB) error {
		head, err := ReadPublicationVersion(tx.Statement.Context, tx, PublicationOwnerUserGroup)
		if err != nil {
			return err
		}
		version, err = BumpPublicationVersionCAS(tx.Statement.Context, tx, PublicationOwnerUserGroup, head)
		if err != nil {
			return err
		}
		return mutate(tx)
	})
	if err != nil {
		return err
	}
	// SQL 已提交，返回成功；本地加载失败由发布状态、readiness 和准入显式处理。
	if err := GlobalUserGroupRatio.Load(); err != nil && logger.Logger != nil {
		logger.SysError(fmt.Sprintf("用户组策略版本 %d 已提交，本地发布失败: %v", version, err))
	}
	return nil
}
