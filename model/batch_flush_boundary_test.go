package model

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBatchFlushPermitHonorsCanceledWait(t *testing.T) {
	batchFlushPermit <- struct{}{}
	released := false
	defer func() {
		if !released {
			<-batchFlushPermit
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- withBatchFlush(ctx, func(context.Context) error { return errors.New("callback must not execute") })
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wait error=%v", err)
		}
	case <-time.After(200 * time.Millisecond):
		<-batchFlushPermit
		released = true
		<-done
		t.Fatal("permit wait ignored deadline")
	}
}

func TestBatchFlushPermitSerializesCallbacks(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- withBatchFlush(context.Background(), func(context.Context) error { close(entered); <-release; return nil })
	}()
	<-entered
	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- withBatchFlush(context.Background(), func(context.Context) error { close(secondEntered); return nil })
	}()
	overlapped := false
	select {
	case <-secondEntered:
		overlapped = true
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if overlapped {
		t.Fatal("flush callbacks overlapped")
	}
}
