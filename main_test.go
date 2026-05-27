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
	)
	if err != nil {
		t.Fatalf("gracefulShutdownSteps err=%v", err)
	}
	if len(order) != 2 || order[0] != "http" || order[1] != "ws" {
		t.Fatalf("shutdown order=%v, want HTTP shutdown before websocket drain", order)
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
