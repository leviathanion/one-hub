package model

import (
	"context"
	"errors"
	"fmt"
	"one-api/common/config"
	"one-api/common/logger"
	"sync"
	"time"

	"gorm.io/gorm"
)

const (
	BatchUpdateTypeChannelUsedQuota = iota
	BatchUpdateTypeRequestCount
	BatchUpdateTypeCount // if you add a new type, you need to add a new map and a new lock
)

var batchUpdateStores []map[int]int
var batchUpdateLocks []sync.Mutex

var batchLogStore []*Log
var batchLogLock sync.Mutex

func init() {
	for i := 0; i < BatchUpdateTypeCount; i++ {
		batchUpdateStores = append(batchUpdateStores, make(map[int]int))
		batchUpdateLocks = append(batchUpdateLocks, sync.Mutex{})
	}
}

const batchWriteTimeout = 5 * time.Second

// Bounds retained entries, not bytes per log. Rejection is explicit; these
// best-effort projections must never exhaust memory while SQL is unavailable.
const maxPendingBatchEntries = 10000

var ErrBatchQueueFull = errors.New("batch projection queue is full")

var ErrBatchUpdaterStopped = errors.New("batch updater is stopped")
var batchAdmissionMu sync.RWMutex
var batchAdmissionClosed bool
var batchWorkerMu sync.Mutex
var batchWorker *batchUpdaterWorker
var batchFlushPermit = make(chan struct{}, 1)

type batchUpdaterWorker struct {
	stop     chan context.Context
	done     chan struct{}
	cancel   context.CancelFunc
	stopOnce sync.Once
	err      error // read only after done closes
}

func InitBatchUpdater() {
	batchWorkerMu.Lock()
	defer batchWorkerMu.Unlock()
	if batchWorker != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker := &batchUpdaterWorker{stop: make(chan context.Context, 1), done: make(chan struct{}), cancel: cancel}
	batchWorker = worker
	interval := time.Duration(config.BatchUpdateInterval) * time.Second
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		defer close(worker.done)
		defer cancel()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var firstError error
		failedFlushes := 0
		for {
			select {
			case shutdownCtx := <-worker.stop:
				finalErr := flushBatchContext(shutdownCtx)
				if firstError != nil {
					firstError = fmt.Errorf("%d earlier batch flushes failed (not replayed): %w", failedFlushes, firstError)
				}
				worker.err = errors.Join(firstError, finalErr)
				return
			case <-ticker.C:
				// Periodic I/O has its own budget; shutdown waits rather than replaying it.
				err := flushBatchContext(ctx)
				if err != nil {
					failedFlushes++
					if firstError == nil {
						firstError = err
					}
					logger.SysError("batch flush incomplete: " + err.Error())
				}
			}
		}
	}()
}

// StopBatchUpdater closes admission after producers drain, waits for a periodic
// flush, and runs one final flush within the caller's budget.
// Failed/ambiguous writes are reported, never replayed as another increment.
func StopBatchUpdater(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	batchWorkerMu.Lock()
	worker := batchWorker
	batchWorkerMu.Unlock()
	if worker == nil {
		return nil
	}
	batchAdmissionMu.Lock()
	batchAdmissionClosed = true
	batchAdmissionMu.Unlock()
	worker.stopOnce.Do(func() { worker.stop <- ctx })
	select {
	case <-worker.done:
		return worker.err
	default:
	}
	select {
	case <-worker.done:
		return worker.err
	case <-ctx.Done():
		select {
		case <-worker.done:
			return worker.err
		default:
		}
		worker.cancel()
		return fmt.Errorf("batch shutdown incomplete (worker or final flush pending): %w", ctx.Err())
	}
}

func AddLogToBatch(log *Log) error {
	batchAdmissionMu.RLock()
	defer batchAdmissionMu.RUnlock()
	if batchAdmissionClosed {
		return ErrBatchUpdaterStopped
	}
	batchLogLock.Lock()
	defer batchLogLock.Unlock()
	if len(batchLogStore) >= maxPendingBatchEntries {
		return ErrBatchQueueFull
	}
	batchLogStore = append(batchLogStore, log)
	return nil
}

func addNewRecord(type_ int, id int, value int) error {
	batchAdmissionMu.RLock()
	defer batchAdmissionMu.RUnlock()
	if batchAdmissionClosed {
		return ErrBatchUpdaterStopped
	}
	batchUpdateLocks[type_].Lock()
	defer batchUpdateLocks[type_].Unlock()
	if _, exists := batchUpdateStores[type_][id]; !exists && len(batchUpdateStores[type_]) >= maxPendingBatchEntries {
		return ErrBatchQueueFull
	}
	batchUpdateStores[type_][id] += value
	return nil
}

func withBatchFlush(ctx context.Context, flush func(context.Context) error) error {
	select {
	case batchFlushPermit <- struct{}{}:
		defer func() { <-batchFlushPermit }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return flush(ctx)
}
func flushBatchContext(ctx context.Context) error {
	return withBatchFlush(ctx, func(ctx context.Context) error {
		// Each projection gets a fresh I/O budget. During final shutdown,
		// reserve at least half the remaining time for logs even if statistics stall.
		statisticBudget := batchWriteTimeout
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline)/2 < statisticBudget {
			statisticBudget = time.Until(deadline) / 2
		}
		statisticsCtx, cancelStatistics := context.WithTimeout(ctx, statisticBudget)
		statisticsErr := batchUpdateContext(statisticsCtx)
		cancelStatistics()
		logsCtx, cancelLogs := context.WithTimeout(ctx, batchWriteTimeout)
		defer cancelLogs()
		return errors.Join(statisticsErr, flushBatchLogsContext(logsCtx))
	})
}
func flushBatchLogs() error {
	ctx, cancel := context.WithTimeout(context.Background(), batchWriteTimeout)
	defer cancel()
	return withBatchFlush(ctx, flushBatchLogsContext)
}
func flushBatchLogsContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	batchLogLock.Lock()
	logs := batchLogStore
	batchLogStore = nil
	batchLogLock.Unlock()
	if len(logs) == 0 {
		return nil
	}
	// One transaction owns this detached batch; an uncertain commit is not retried.
	if err := DB.WithContext(ctx).CreateInBatches(logs, 200).Error; err != nil {
		return fmt.Errorf("%d consume logs not confirmed persisted: %w", len(logs), err)
	}
	return nil
}
func batchUpdate() error {
	ctx, cancel := context.WithTimeout(context.Background(), batchWriteTimeout)
	defer cancel()
	return withBatchFlush(ctx, batchUpdateContext)
}
func batchUpdateContext(ctx context.Context) error {
	var errs []error
	for i := 0; i < BatchUpdateTypeCount; i++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		batchUpdateLocks[i].Lock()
		store := batchUpdateStores[i]
		batchUpdateStores[i] = make(map[int]int)
		batchUpdateLocks[i].Unlock()
		for key, value := range store {
			var result *gorm.DB
			switch i {
			case BatchUpdateTypeRequestCount:
				result = DB.WithContext(ctx).Model(&User{}).Where("id = ?", key).Update("request_count", gorm.Expr("request_count + ?", value))
			case BatchUpdateTypeChannelUsedQuota:
				result = DB.WithContext(ctx).Model(&Channel{}).Where("id = ?", key).Update("used_quota", gorm.Expr("used_quota + ?", value))
			}
			if result.Error != nil {
				errs = append(errs, fmt.Errorf("batch statistic type=%d id=%d delta=%d not confirmed persisted: %w", i, key, value, result.Error))
			}
		}
	}
	return errors.Join(errs...)
}

func BatchInsertStrict[T any](db *gorm.DB, data []T) error {
	batchSize := 200
	for i := 0; i < len(data); i += batchSize {
		end := i + batchSize
		if end > len(data) {
			end = len(data)
		}
		if err := batchInsertWithRetry(db, data[i:end]); err != nil {
			return err
		}
	}
	return nil
}

// batchInsertWithRetry 使用二分法进行容错插入
// 当批量插入失败时，将数据二分后分别尝试插入，递归直到单条记录
func batchInsertWithRetry[T any](db *gorm.DB, data []T) error {
	if len(data) == 0 {
		return nil
	}

	// 尝试批量插入
	err := db.Create(data).Error
	if err == nil {
		return nil
	}

	// 如果只有一条记录且失败，记录错误并跳过这条记录
	if len(data) == 1 {
		logger.SysError(fmt.Sprintf("failed to insert single record: %s", err.Error()))
		return err
	}

	// 二分继续尝试
	mid := len(data) / 2
	logger.SysLog(fmt.Sprintf("batch insert failed, splitting %d records into two halves", len(data)))

	// 分别插入两半，即使一半失败也继续插入另一半
	err1 := batchInsertWithRetry(db, data[:mid])
	err2 := batchInsertWithRetry(db, data[mid:])

	// 返回第一个错误（如果有的话）
	if err1 != nil {
		return err1
	}
	return err2
}
