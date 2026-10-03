package model

import (
	"context"
	"errors"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"one-api/common/config"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func setupBatchShutdownTest(t *testing.T, intervals ...int) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "batch.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	originalDB := DB
	DB = db
	t.Cleanup(func() { pool, _ := db.DB(); _ = pool.Close(); DB = originalDB })
	if err := db.AutoMigrate(&User{}, &Channel{}, &Log{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&User{Id: 1, Username: "batch-user", Password: "test"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Channel{Id: 1, Name: "batch-channel"}).Error; err != nil {
		t.Fatal(err)
	}
	oldEnabled, oldInterval := config.BatchUpdateEnabled, config.BatchUpdateInterval
	config.BatchUpdateEnabled = true
	config.BatchUpdateInterval = 3600
	if len(intervals) > 0 {
		config.BatchUpdateInterval = intervals[0]
	}
	batchWorkerMu.Lock()
	if batchWorker != nil {
		t.Fatal("worker already running")
	}
	batchWorkerMu.Unlock()
	InitBatchUpdater()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = StopBatchUpdater(ctx)
		batchWorkerMu.Lock()
		batchWorker = nil
		batchWorkerMu.Unlock()
		batchAdmissionMu.Lock()
		batchAdmissionClosed = false
		batchAdmissionMu.Unlock()
		batchLogLock.Lock()
		batchLogStore = nil
		batchLogLock.Unlock()
		for i := 0; i < BatchUpdateTypeCount; i++ {
			batchUpdateLocks[i].Lock()
			batchUpdateStores[i] = map[int]int{}
			batchUpdateLocks[i].Unlock()
		}
		config.BatchUpdateEnabled = oldEnabled
		config.BatchUpdateInterval = oldInterval
	})
	return db
}
func assertBatchProjection(t *testing.T, db *gorm.DB, logs, count, quota int) {
	t.Helper()
	var n int64
	if err := db.Model(&Log{}).Count(&n).Error; err != nil || int(n) != logs {
		t.Fatal(n, logs, err)
	}
	var user User
	if err := db.First(&user, 1).Error; err != nil || user.RequestCount != count {
		t.Fatal(user.RequestCount, count, err)
	}
	var channel Channel
	if err := db.First(&channel, 1).Error; err != nil || channel.UsedQuota != int64(quota) {
		t.Fatal(channel.UsedQuota, quota, err)
	}
}
func TestBatchShutdownPersistsFinalProjectionAndRejectsLateWrites(t *testing.T) {
	db := setupBatchShutdownTest(t)
	for i := 0; i < 3; i++ {
		if err := AddLogToBatch(&Log{UserId: 1, Content: "final"}); err != nil {
			t.Fatal(err)
		}
		if err := UpdateUserRequestCountWithContext(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		if err := UpdateChannelUsedQuotaWithContext(context.Background(), 1, 7); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := StopBatchUpdater(ctx); err != nil {
		t.Fatal(err)
	}
	assertBatchProjection(t, db, 3, 3, 21)
	if err := StopBatchUpdater(ctx); err != nil {
		t.Fatal(err)
	}
	assertBatchProjection(t, db, 3, 3, 21)
	canceled, cancelAgain := context.WithCancel(context.Background())
	cancelAgain()
	for i := 0; i < 100; i++ {
		if err := StopBatchUpdater(canceled); err != nil {
			t.Fatalf("completed stop with canceled context: %v", err)
		}
	}
	if !errors.Is(AddLogToBatch(&Log{}), ErrBatchUpdaterStopped) || !errors.Is(UpdateUserRequestCountWithContext(ctx, 1), ErrBatchUpdaterStopped) || !errors.Is(UpdateChannelUsedQuotaWithContext(ctx, 1, 1), ErrBatchUpdaterStopped) {
		t.Fatal("late projection accepted")
	}
}
func TestBatchShutdownSerializesInFlightAndFinalFlush(t *testing.T) {
	db := setupBatchShutdownTest(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	if err := db.Callback().Update().Before("gorm:update").Register("test:pause_batch", func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			once.Do(func() { close(entered); <-release })
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := addNewRecord(BatchUpdateTypeRequestCount, 1, 2); err != nil {
		t.Fatal(err)
	}
	flushDone := make(chan error, 1)
	go func() { flushDone <- flushBatchContext(context.Background()) }()
	<-entered
	if err := addNewRecord(BatchUpdateTypeRequestCount, 1, 3); err != nil {
		t.Fatal(err)
	}
	if err := AddLogToBatch(&Log{UserId: 1}); err != nil {
		t.Fatal(err)
	}
	stopDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { stopDone <- StopBatchUpdater(ctx) }()
	select {
	case err := <-stopDone:
		t.Fatalf("shutdown failed to wait: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-flushDone; err != nil {
		t.Fatal(err)
	}
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	assertBatchProjection(t, db, 1, 5, 0)
}
func TestBatchShutdownReportsDatabaseFailureWithoutReplay(t *testing.T) {
	db := setupBatchShutdownTest(t)
	dbErr := errors.New("database unavailable")
	if err := AddLogToBatch(&Log{UserId: 1}); err != nil {
		t.Fatal(err)
	}
	if err := addNewRecord(BatchUpdateTypeRequestCount, 1, 3); err != nil {
		t.Fatal(err)
	}
	db.Callback().Create().Before("gorm:create").Register("test:fail_batch_create", func(tx *gorm.DB) { tx.AddError(dbErr) })
	db.Callback().Update().Before("gorm:update").Register("test:fail_batch_update", func(tx *gorm.DB) { tx.AddError(dbErr) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := StopBatchUpdater(ctx); !errors.Is(err, dbErr) {
		t.Fatal(err)
	}
	db.Callback().Create().Remove("test:fail_batch_create")
	db.Callback().Update().Remove("test:fail_batch_update")
	if err := StopBatchUpdater(ctx); !errors.Is(err, dbErr) {
		t.Fatal(err)
	}
	assertBatchProjection(t, db, 0, 0, 0)
}
func TestBatchShutdownHonorsDeadlineWaitingForInFlight(t *testing.T) {
	_ = setupBatchShutdownTest(t)
	batchFlushPermit <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := StopBatchUpdater(ctx)
	<-batchFlushPermit
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatal(err, time.Since(started))
	}
	batchWorkerMu.Lock()
	worker := batchWorker
	batchWorkerMu.Unlock()
	select {
	case <-worker.done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after deadline")
	}
}
