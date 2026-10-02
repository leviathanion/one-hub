package model

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBatchWorkerShutdownCancelsPeriodicDatabaseWorkWithinBudget(t *testing.T) {
	db := setupBatchShutdownTest(t, 1)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	if err := db.Callback().Update().Before("gorm:update").Register("test:periodic_context_block", func(tx *gorm.DB) {
		if tx.Statement.Table != "users" {
			return
		}
		calls.Add(1)
		once.Do(func() {
			close(entered)
			select {
			case <-tx.Statement.Context.Done():
				close(canceled)
				tx.AddError(tx.Statement.Context.Err())
			case <-release:
				tx.AddError(errors.New("test released blocked database callback"))
			}
		})
	}); err != nil {
		t.Fatal(err)
	}
	// Release the callback on every test failure, before fixture cleanup stops its worker.
	defer close(release)
	if err := addNewRecord(BatchUpdateTypeRequestCount, 1, 3); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("actual ticker never started database update")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := StopBatchUpdater(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("shutdown failed budget: err=%v duration=%s", err, time.Since(started))
	}
	select {
	case <-canceled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("periodic database context was not canceled by shutdown")
	}
	batchWorkerMu.Lock()
	worker := batchWorker
	batchWorkerMu.Unlock()
	select {
	case <-worker.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("actual periodic worker did not finish after canceled database work")
	}
	if calls.Load() != 1 {
		t.Fatalf("uncertain detached update replayed %d times", calls.Load())
	}
	if err := StopBatchUpdater(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("periodic canceled write not reported: %v", err)
	}
	assertBatchProjection(t, db, 0, 0, 0)
}
