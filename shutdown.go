package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"one-api/common/wsconn"
	"one-api/controller"
	"one-api/internal/lifecycle"
	"one-api/model"
	"one-api/payment"
	"one-api/relay"
	"one-api/relay/task"
)

type shutdownOperations struct {
	http, connections, tasks, requests, background func(context.Context) error
	observers                                      func(context.Context) error
	payments                                       func()
	batches                                        func(context.Context) error
}

func gracefulShutdown(ctx context.Context, server *http.Server, requests *lifecycle.Group, stopBackground func(context.Context) error) error {
	requests.Close()
	return gracefulShutdownSteps(ctx, shutdownOperations{
		http: server.Shutdown, connections: wsconn.ShutdownActive, tasks: task.StopTask,
		requests: requests.Wait, background: stopBackground,
		observers: func(ctx context.Context) error {
			if err := controller.StopChannelProbes(ctx); err != nil {
				return err
			}
			return relay.StopBackgroundObservers(ctx)
		},
		payments: payment.Resources.Close, batches: model.StopBatchUpdater,
	})
}

// Independent producers begin draining together; a long HTTP request must not
// postpone the signal to a WebSocket or a background poll until the budget ends.
// On an incomplete drain the caller exits unsuccessfully without claiming a
// final flush or retiring a database that may still have business users.
func gracefulShutdownSteps(ctx context.Context, ops shutdownOperations) error {
	var errs []error
	results := make(chan error, 5)
	count := 0
	for _, step := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"http shutdown", ops.http}, {"websocket connections", ops.connections}, {"task progress", ops.tasks}, {"request business", ops.requests}, {"background runtime", ops.background},
	} {
		if step.run == nil {
			continue
		}
		count++
		go func() {
			err := step.run(ctx)
			if err != nil {
				err = fmt.Errorf("%s: %w", step.name, err)
			}
			results <- err
		}()
	}
	for i := 0; i < count; i++ {
		select {
		case err := <-results:
			errs = append(errs, err)
		case <-ctx.Done():
			return errors.Join(append(errs, fmt.Errorf("business drain incomplete: %w", ctx.Err()))...)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if ops.observers != nil {
		if err := ops.observers(ctx); err != nil {
			return fmt.Errorf("background observation drain: %w", err)
		}
	}
	if ops.payments != nil {
		done := make(chan struct{})
		go func() { defer close(done); ops.payments() }()
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("payment drain incomplete: %w", ctx.Err())
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("final batch budget exhausted: %w", err)
	}
	if ops.batches != nil {
		if err := ops.batches(ctx); err != nil {
			return fmt.Errorf("batch shutdown: %w", err)
		}
	}
	return nil
}
