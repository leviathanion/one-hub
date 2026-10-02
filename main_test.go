package main

import (
	"context"
	"errors"
	"testing"
)

func TestGracefulShutdownStopsHTTPBeforeWebSocketDrain(t *testing.T) {
	var order []string
	err := gracefulShutdownSteps(
		context.Background(),
		func(context.Context) error {
			order = append(order, "http")
			return nil
		},
		func(context.Context) error {
			order = append(order, "ws")
			return nil
		},
		func() { order = append(order, "payments") },
		nil,
	)
	if err != nil {
		t.Fatalf("gracefulShutdownSteps err=%v", err)
	}
	if len(order) != 3 || order[0] != "http" || order[1] != "ws" || order[2] != "payments" {
		t.Fatalf("shutdown order=%v, want HTTP shutdown and websocket drain before payment resource retirement", order)
	}
}

func TestGracefulShutdownDrainsWebSocketsWhenHTTPShutdownFails(t *testing.T) {
	httpErr := errors.New("http shutdown failed")
	wsErr := errors.New("websocket drain failed")
	var drained bool

	err := gracefulShutdownSteps(
		context.Background(),
		func(context.Context) error {
			return httpErr
		},
		func(context.Context) error {
			drained = true
			return wsErr
		},
		func() { t.Fatal("payment resources retired while HTTP requests may still be running") },
		nil,
	)
	if err == nil {
		t.Fatal("gracefulShutdownSteps err=nil, want joined shutdown errors")
	}
	if !drained {
		t.Fatal("expected websocket drain to run after HTTP shutdown error")
	}
	if !errors.Is(err, httpErr) || !errors.Is(err, wsErr) {
		t.Fatalf("err=%v, want HTTP shutdown and websocket drain errors", err)
	}
}

func TestGracefulShutdownRetiresPaymentsAfterHTTPDrainDespiteWebSocketError(t *testing.T) {
	wsErr := errors.New("websocket drain failed")
	var retired bool
	err := gracefulShutdownSteps(context.Background(), func(context.Context) error { return nil },
		func(context.Context) error { return wsErr }, func() { retired = true }, nil)
	if !retired || !errors.Is(err, wsErr) {
		t.Fatalf("payment retirement=%v, shutdown error=%v", retired, err)
	}
}

func TestGracefulShutdownFlushesBatchesAfterProducersAndJoinsErrors(t *testing.T) {
	var order []string
	flushErr := errors.New("final batch unavailable")
	err := gracefulShutdownSteps(context.Background(), func(context.Context) error { order = append(order, "http"); return nil }, func(context.Context) error { order = append(order, "ws"); return nil }, func() { order = append(order, "payments") }, func(context.Context) error { order = append(order, "batch"); return flushErr })
	if !errors.Is(err, flushErr) || len(order) != 4 || order[0] != "http" || order[1] != "ws" || order[2] != "payments" || order[3] != "batch" {
		t.Fatal(order, err)
	}
}
func TestGracefulShutdownStillStopsBatchWorkerAfterProducerErrors(t *testing.T) {
	httpErr := errors.New("HTTP drain failed")
	wsErr := errors.New("WS drain failed")
	batchErr := errors.New("batch flush failed")
	err := gracefulShutdownSteps(context.Background(), func(context.Context) error { return httpErr }, func(context.Context) error { return wsErr }, func() { t.Fatal("payments must not close before HTTP drains") }, func(context.Context) error { return batchErr })
	if !errors.Is(err, httpErr) || !errors.Is(err, wsErr) || !errors.Is(err, batchErr) {
		t.Fatal(err)
	}
}
