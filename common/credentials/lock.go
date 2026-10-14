package credentials

import (
	"context"
	"sync"
)

type channelRefreshLock struct {
	gate chan struct{}
	refs int
}

var channelRefreshLocks = struct {
	mu    sync.Mutex
	locks map[int]*channelRefreshLock
}{locks: make(map[int]*channelRefreshLock)}

// AcquireChannelRefreshLock 只协调同一进程；允许刷新旧 token 的事实仍是数据库 claim。
// 最后一个持有者或等待者离开时回收条目，不保存空闲渠道状态。
func AcquireChannelRefreshLock(ctx context.Context, channelID int) (func(), error) {
	if channelID <= 0 {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	channelRefreshLocks.mu.Lock()
	lock := channelRefreshLocks.locks[channelID]
	if lock == nil {
		lock = &channelRefreshLock{gate: make(chan struct{}, 1)}
		lock.gate <- struct{}{}
		channelRefreshLocks.locks[channelID] = lock
	}
	lock.refs++
	channelRefreshLocks.mu.Unlock()
	cleanup := func() {
		channelRefreshLocks.mu.Lock()
		defer channelRefreshLocks.mu.Unlock()
		lock.refs--
		if lock.refs == 0 && channelRefreshLocks.locks[channelID] == lock {
			delete(channelRefreshLocks.locks, channelID)
		}
	}
	var once sync.Once
	release := func() { once.Do(func() { lock.gate <- struct{}{}; cleanup() }) }
	// 保留 Codex 原有快路径：无等待时调用者仍负责在 I/O 前检查取消。
	select {
	case <-lock.gate:
		return release, nil
	default:
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return nil, err
	}
	select {
	case <-lock.gate:
		if err := ctx.Err(); err != nil {
			lock.gate <- struct{}{}
			cleanup()
			return nil, err
		}
		return release, nil
	case <-ctx.Done():
		cleanup()
		return nil, ctx.Err()
	}
}
