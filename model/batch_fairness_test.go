package model

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"testing"
	"time"
)

func TestSlowStatisticsLeaveBudgetForLogs(t *testing.T) {
	db := setupBatchShutdownTest(t)
	if err := db.Callback().Update().Before("gorm:update").Register("test:slow_statistics", func(tx *gorm.DB) {
		if tx.Statement.Table == "channels" {
			<-tx.Statement.Context.Done()
			tx.AddError(tx.Statement.Context.Err())
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Update().Remove("test:slow_statistics")
	for round := 1; round <= 3; round++ {
		if err := addNewRecord(BatchUpdateTypeChannelUsedQuota, 1, 7); err != nil {
			t.Fatal(err)
		}
		if err := AddLogToBatch(&Log{UserId: 1, Content: "must advance"}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		err := flushBatchContext(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		assertBatchProjection(t, db, round, 0, 0)
	}
}
func TestBatchQueuesRejectExcessEntriesWithoutDiscardingAcceptedWork(t *testing.T) {
	setupBatchShutdownTest(t)
	// Only queue admission is under test; restore the stores before fixture flush.
	for i := 0; i < maxPendingBatchEntries; i++ {
		if err := AddLogToBatch(&Log{}); err != nil {
			t.Fatal(err)
		}
		if err := addNewRecord(BatchUpdateTypeChannelUsedQuota, i, 1); err != nil {
			t.Fatal(err)
		}
	}
	if !errors.Is(AddLogToBatch(&Log{}), ErrBatchQueueFull) {
		t.Fatal("unbounded log queue")
	}
	if !errors.Is(addNewRecord(BatchUpdateTypeChannelUsedQuota, maxPendingBatchEntries, 1), ErrBatchQueueFull) {
		t.Fatal("unbounded statistic keys")
	}
	if err := addNewRecord(BatchUpdateTypeChannelUsedQuota, 1, 2); err != nil {
		t.Fatal("existing statistic key rejected", err)
	}
	batchLogStore = nil
	batchUpdateStores[BatchUpdateTypeChannelUsedQuota] = map[int]int{}
}
