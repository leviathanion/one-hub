package model

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"gorm.io/gorm"
)

func firstClosureTask(t *testing.T, family string) *Task {
	t.Helper()
	var task *Task
	if family == TaskPlatformOpenAIResponsesBackground {
		backgroundTaskDB(t)
		task = createBackgroundTask(t)
		acceptBackground(t, task, "resp_first_closure")
	} else {
		batchTestDB(t)
		task = newBatchTestTask(t)
		slots := batchTestSlots(t, task)
		if result, err := AcceptOpenAIBatchSubmission(context.Background(), task, "batch_first_closure", slots[0].ID, []uint64{slots[1].ID, slots[2].ID}); err != nil || result.Outcome != TaskMutationApplied {
			t.Fatalf("accept batch: %+v %v", result, err)
		}
	}
	task.Status = TaskStatusSuccess
	return task
}

func finalizeFirstClosure(task *Task) (BillingBalanceResult, error) {
	if task.Platform == TaskPlatformOpenAIResponsesBackground {
		return FinalizeBackgroundResponseBillingOwner(context.Background(), task, 25, "confirm", task.Data)
	}
	return FinalizeOpenAIBatchBillingOwner(context.Background(), task, 25, "confirm", task.Data)
}

func TestTaskFirstOwnerClosureFirstAndRepeated(t *testing.T) {
	for _, family := range []string{TaskPlatformOpenAIResponsesBackground, TaskPlatformOpenAIBatch} {
		t.Run(family, func(t *testing.T) {
			task := firstClosureTask(t, family)
			first, err := finalizeFirstClosure(task)
			if err != nil || first.Outcome != BillingBalanceCommitted || !first.FirstOwnerClosure {
				t.Fatalf("first closure: %+v %v", first, err)
			}
			repeated, err := finalizeFirstClosure(task)
			if err != nil || repeated.Outcome != BillingBalanceCommitted || repeated.FirstOwnerClosure {
				t.Fatalf("repeated closure: %+v %v", repeated, err)
			}
		})
	}
}

func TestTaskFirstOwnerClosureConcurrentFinalizers(t *testing.T) {
	for _, family := range []string{TaskPlatformOpenAIResponsesBackground, TaskPlatformOpenAIBatch} {
		t.Run(family, func(t *testing.T) {
			task := firstClosureTask(t, family)
			sqlDB, err := DB.DB()
			if err != nil {
				t.Fatal(err)
			}
			// SQLite 使用单连接排队事务，两个真实并发调用共享相同的旧版本。
			sqlDB.SetMaxOpenConns(1)
			var wg sync.WaitGroup
			start := make(chan struct{})
			type outcome struct {
				result BillingBalanceResult
				err    error
			}
			results := make(chan outcome, 2)
			for i := 0; i < 2; i++ {
				candidate := *task
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					result, err := finalizeFirstClosure(&candidate)
					results <- outcome{result, err}
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			firstCount := 0
			for got := range results {
				if got.err != nil || got.result.Outcome != BillingBalanceCommitted {
					t.Fatalf("concurrent result: %+v %v", got.result, got.err)
				}
				if got.result.FirstOwnerClosure {
					firstCount++
				}
			}
			if firstCount != 1 {
				t.Fatalf("first closure count=%d", firstCount)
			}
			var user User
			if err := DB.First(&user, 1).Error; err != nil {
				t.Fatal(err)
			}
			if user.Quota != 975 || user.UsedQuota != 25 {
				t.Fatalf("balances changed twice: quota=%d used=%d", user.Quota, user.UsedQuota)
			}
		})
	}
}

// 包装真实事务：SQL 已提交，但调用者丢失成功确认。回读可以恢复余额事实，
// 不能据此再次触发最佳努力消费日志或统计投影。
type firstClosureLostCommitPool struct {
	gorm.ConnPool
	beginner gorm.TxBeginner
}

func (p firstClosureLostCommitPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.beginner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &firstClosureLostCommitTx{Tx: tx}, nil
}

type firstClosureLostCommitTx struct{ *sql.Tx }

func (tx firstClosureLostCommitTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	return errors.New("commit succeeded but acknowledgment was lost")
}

func TestTaskFirstOwnerClosureCommitRecoveryDoesNotProject(t *testing.T) {
	for _, family := range []string{TaskPlatformOpenAIResponsesBackground, TaskPlatformOpenAIBatch} {
		t.Run(family, func(t *testing.T) {
			task := firstClosureTask(t, family)
			original := DB
			pool := original.Statement.ConnPool
			beginner, ok := pool.(gorm.TxBeginner)
			if !ok {
				t.Fatalf("fixture pool %T cannot begin SQL transaction", pool)
			}
			fault := original.Session(&gorm.Session{NewDB: true, Context: context.Background()})
			fault.Statement.ConnPool = firstClosureLostCommitPool{ConnPool: pool, beginner: beginner}
			DB = fault
			t.Cleanup(func() { DB = original })
			result, err := finalizeFirstClosure(task)
			if err != nil || result.Outcome != BillingBalanceCommitted || !result.CommitAttempted || result.FirstOwnerClosure {
				t.Fatalf("commit recovery: %+v %v", result, err)
			}
			if task.ProviderState != TaskProviderStateClosed || task.ChargedQuota == nil || *task.ChargedQuota != 25 {
				t.Fatalf("closure not recovered: %+v", task)
			}
			var user User
			if err := DB.First(&user, 1).Error; err != nil {
				t.Fatal(err)
			}
			if user.Quota != 975 || user.UsedQuota != 25 {
				t.Fatalf("recovered balance: quota=%d used=%d", user.Quota, user.UsedQuota)
			}
		})
	}
}
