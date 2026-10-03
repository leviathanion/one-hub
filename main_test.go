package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestGracefulShutdownWaitsForAllBusinessBeforeRetiringResources(t *testing.T) {
	entered := make(chan string, 4)
	release := make(chan struct{})
	done := make(chan error, 1)
	producer := func(name string) func(context.Context) error {
		return func(context.Context) error { entered <- name; <-release; return nil }
	}
	var stage atomic.Int32
	go func() {
		done <- gracefulShutdownSteps(context.Background(), shutdownOperations{
			http: producer("http"), connections: producer("connections"), tasks: producer("tasks"), requests: producer("requests"),
			observers: func(context.Context) error {
				if !stage.CompareAndSwap(0, 1) {
					t.Error("observation order")
				}
				return nil
			},
			payments: func() {
				if !stage.CompareAndSwap(1, 2) {
					t.Error("payment order")
				}
			},
			batches: func(context.Context) error {
				if !stage.CompareAndSwap(2, 3) {
					t.Error("batch order")
				}
				return nil
			},
		})
	}()
	for range 4 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("producer stop was serialized")
		}
	}
	if stage.Load() != 0 {
		t.Fatal("retired resources before producers finished")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if stage.Load() != 3 {
		t.Fatal("missing final flush")
	}
}

func TestGracefulShutdownReportsIncompleteBusinessWithoutFinalFlush(t *testing.T) {
	failure := errors.New("business still active")
	for _, kind := range []string{"requests", "tasks", "observations"} {
		t.Run(kind, func(t *testing.T) {
			ops := shutdownOperations{payments: func() { t.Error("payment resources retired") }, batches: func(context.Context) error { t.Error("premature batch stop"); return nil }}
			fail := func(context.Context) error { return failure }
			switch kind {
			case "requests":
				ops.requests = fail
			case "tasks":
				ops.tasks = fail
			default:
				ops.observers = fail
			}
			if err := gracefulShutdownSteps(context.Background(), ops); !errors.Is(err, failure) {
				t.Fatal(err)
			}
		})
	}
}

func TestGracefulShutdownBoundsWholeDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	done := make(chan error, 1)
	go func() {
		done <- gracefulShutdownSteps(ctx, shutdownOperations{requests: func(context.Context) error { close(entered); <-release; return nil }, batches: func(context.Context) error { t.Error("flushed with live producer"); return nil }})
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("ignored global budget")
	}
}

func TestGracefulShutdownReturnsFinalFlushFailure(t *testing.T) {
	failure := errors.New("database unavailable")
	err := gracefulShutdownSteps(context.Background(), shutdownOperations{batches: func(context.Context) error { return failure }})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
}
