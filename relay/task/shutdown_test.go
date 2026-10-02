package task

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestProgressorStopsBeforeAnotherTickAndWaitsForInFlightWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	go func() {
		defer close(done)
		runTask(ctx, func(context.Context) { calls.Add(1); close(entered); <-release })
	}()
	<-entered
	cancel()
	select {
	case <-done:
		t.Fatal("did not wait for active settlement")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}
func TestStopTaskHonorsBudgetAndCanWaitAgain(t *testing.T) {
	original := taskWorker
	defer func() { taskWorker = original }()
	canceled := make(chan struct{})
	done := make(chan struct{})
	taskWorker = &progressWorker{cancel: func() {
		select {
		case <-canceled:
		default:
			close(canceled)
		}
	}, done: done}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := StopTask(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	default:
		t.Fatal("poll not canceled")
	}
	close(done)
	if err := StopTask(ctx); err != nil {
		t.Fatal(err)
	}
}
