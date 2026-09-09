package realtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestByteBudgetAcquireReleaseAndWait(t *testing.T) {
	budget := NewByteBudget(8)
	first, ok := budget.TryAcquire(8)
	if !ok || budget.Used() != 8 {
		t.Fatalf("expected first credit to own the budget, ok=%v used=%d", ok, budget.Used())
	}
	if _, ok := budget.TryAcquire(1); ok {
		t.Fatal("budget admitted bytes beyond its limit")
	}

	result := make(chan *ByteCredit, 1)
	go func() {
		credit, err := budget.Acquire(context.Background(), 4)
		if err == nil {
			result <- credit
		}
	}()
	select {
	case <-result:
		t.Fatal("blocking acquire completed before source credit release")
	case <-time.After(10 * time.Millisecond):
	}
	first.Release()
	first.Release()
	select {
	case second := <-result:
		if budget.Used() != 4 {
			t.Fatalf("expected replacement credit to own four bytes, got %d", budget.Used())
		}
		second.Release()
	case <-time.After(time.Second):
		t.Fatal("blocking acquire did not wake after release")
	}
	if budget.Used() != 0 {
		t.Fatalf("credit release leaked bytes: %d", budget.Used())
	}
}

func TestByteBudgetRejectsOversizeAndHonorsCancellation(t *testing.T) {
	budget := NewByteBudget(4)
	if _, err := budget.Acquire(context.Background(), 5); !errors.Is(err, ErrByteBudgetExceeded) {
		t.Fatalf("expected oversize acquisition error, got %v", err)
	}
	credit, ok := budget.TryAcquire(4)
	if !ok {
		t.Fatal("expected full budget credit")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := budget.Acquire(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled wait, got %v", err)
	}
	credit.Release()
}
