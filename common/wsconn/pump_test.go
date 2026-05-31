package wsconn

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"one-api/common/logger"

	"github.com/gorilla/websocket"
)

func TestReadInitialNormalThenPumpReadsSecondFrame(t *testing.T) {
	client, server := managedPairForTest(t)
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	if err := server.WriteMessage(TextMessage, []byte("first")); err != nil {
		t.Fatalf("server write first: %v", err)
	}
	mt, payload, err := client.ReadInitial(context.Background())
	if err != nil {
		t.Fatalf("ReadInitial err=%v", err)
	}
	if mt != TextMessage || string(payload) != "first" {
		t.Fatalf("ReadInitial=(%v,%q), want first text", mt, payload)
	}

	got := make(chan string, 1)
	go Pump{
		Conn: client,
		Handle: func(_ context.Context, _ MessageType, payload []byte) {
			got <- string(payload)
		},
	}.Run(context.Background())
	if err := server.WriteMessage(TextMessage, []byte("second")); err != nil {
		t.Fatalf("server write second: %v", err)
	}
	select {
	case msg := <-got:
		if msg != "second" {
			t.Fatalf("Pump got %q, want second", msg)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for pump frame")
	}
}

func TestReadInitialContextCancelAfterSuccessDoesNotInterruptPump(t *testing.T) {
	client, server := managedPairForTest(t)
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	readCtx, cancelRead := context.WithCancel(context.Background())
	if err := server.WriteMessage(TextMessage, []byte("first")); err != nil {
		t.Fatalf("server write first: %v", err)
	}
	if _, _, err := client.ReadInitial(readCtx); err != nil {
		t.Fatalf("ReadInitial err=%v", err)
	}
	cancelRead()

	got := make(chan string, 1)
	go Pump{
		Conn: client,
		Handle: func(_ context.Context, _ MessageType, payload []byte) {
			got <- string(payload)
		},
	}.Run(context.Background())
	if err := server.WriteMessage(TextMessage, []byte("second")); err != nil {
		t.Fatalf("server write second: %v", err)
	}
	select {
	case msg := <-got:
		if msg != "second" {
			t.Fatalf("Pump got %q, want second", msg)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for pump frame after canceling ReadInitial context")
	}
}

func TestReadInitialWatcherGoroutineDoesNotLeak(t *testing.T) {
	client, server := managedPairForTest(t)
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readDone := make(chan error, 1)
	go func() {
		_, _, err := client.ReadInitial(ctx)
		readDone <- err
	}()
	waitForGoroutineStackContains(t, "applyTemporaryReadDeadline")

	if err := server.WriteMessage(TextMessage, []byte("first")); err != nil {
		t.Fatalf("server write first: %v", err)
	}
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("ReadInitial err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for ReadInitial to return")
	}
	waitForNoGoroutineStackContains(t, "applyTemporaryReadDeadline")
}

func TestReadInitialOversizedReturnsSentinelAndAllowsWrite(t *testing.T) {
	client, server := managedPairForTestWithConfigs(t, Config{ReadLimit: 4}, Config{})
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	if err := server.WriteMessage(TextMessage, []byte("12345")); err != nil {
		t.Fatalf("server write oversized: %v", err)
	}
	_, _, err := client.ReadInitial(context.Background())
	if !errors.Is(err, ErrFirstFrameTooLarge) {
		t.Fatalf("ReadInitial err=%v, want ErrFirstFrameTooLarge", err)
	}
	if err := client.WriteMessage(TextMessage, []byte(`{"error":"too_large"}`)); err != nil {
		t.Fatalf("client should remain writable after oversized first frame: %v", err)
	}
	mt, payload, err := server.ReadInitial(context.Background())
	if err != nil {
		t.Fatalf("server ReadInitial err=%v, want business error frame", err)
	}
	if mt != TextMessage || string(payload) != `{"error":"too_large"}` {
		t.Fatalf("server ReadInitial=(%v,%q), want text business error", mt, payload)
	}
}

func TestReadInitialPeerCloseReturnsCloseError(t *testing.T) {
	client, server := managedPairForTest(t)
	defer client.Close(CloseInfo{Kind: CloseKindAbort})

	server.Close(CloseInfo{Kind: CloseKindNormal, Code: CloseNormalClosure, Reason: "bye"})
	_, _, err := client.ReadInitial(context.Background())
	var closeErr *CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("ReadInitial err=%T %v, want *CloseError", err, err)
	}
	if closeErr.Code != CloseNormalClosure || closeErr.Reason != "bye" {
		t.Fatalf("CloseError=%+v, want code=1000 reason=bye", closeErr)
	}
}

func TestReadInitialContextDeadlineExceeded(t *testing.T) {
	client, server := managedPairForTest(t)
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	defer cancel()
	_, _, err := client.ReadInitial(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReadInitial err=%v, want context.DeadlineExceeded", err)
	}
}

func TestReadInitialContextCanceled(t *testing.T) {
	client, server := managedPairForTest(t)
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := client.ReadInitial(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadInitial err=%v, want context.Canceled", err)
	}
}

func TestReadInitialIOErrorThenPumpPanics(t *testing.T) {
	client, server := managedPairForTest(t)
	defer client.Close(CloseInfo{Kind: CloseKindAbort})

	if err := server.raw.Close(); err != nil {
		t.Fatalf("close raw server conn: %v", err)
	}
	_, _, err := client.ReadInitial(context.Background())
	if err == nil {
		t.Fatalf("ReadInitial err=nil, want IO error")
	}
	var closeErr *CloseError
	if errors.As(err, &closeErr) && closeErr.Code != CloseAbnormalClosure {
		t.Fatalf("ReadInitial close err=%+v, want abnormal or non-close IO error", closeErr)
	}
	assertPanicContains(t, "Pump.Run invalid read state", func() {
		Pump{Conn: client}.Run(context.Background())
	})
}

func TestPumpReadLimitClosesWithoutHandle(t *testing.T) {
	client, server := managedPairForTestWithConfig(t, Config{ReadLimit: 4})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	handled := make(chan struct{}, 1)
	closed := make(chan CloseInfo, 1)
	go Pump{
		Conn: client,
		Handle: func(context.Context, MessageType, []byte) {
			handled <- struct{}{}
		},
		OnClose: func(info CloseInfo) {
			closed <- info
		},
	}.Run(context.Background())

	if err := server.WriteMessage(TextMessage, []byte("12345")); err != nil {
		t.Fatalf("server write oversized: %v", err)
	}
	select {
	case info := <-closed:
		if info.Kind != CloseKindReadError || info.Code != CloseMessageTooBig {
			t.Fatalf("CloseInfo=%+v, want read_error/message_too_big", info)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for pump close")
	}
	select {
	case <-handled:
		t.Fatalf("Handle called for oversized frame")
	default:
	}
}

func TestPumpDefaultReadLimitClosesOversizedFrame(t *testing.T) {
	client, server := managedPairForTestWithConfig(t, Config{})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	handled := make(chan struct{}, 1)
	closed := make(chan CloseInfo, 1)
	go Pump{
		Conn: client,
		Handle: func(context.Context, MessageType, []byte) {
			handled <- struct{}{}
		},
		OnClose: func(info CloseInfo) {
			closed <- info
		},
	}.Run(context.Background())

	_ = server.WriteMessage(BinaryMessage, bytes.Repeat([]byte{0x01}, int(defaultReadLimit)+1))
	select {
	case info := <-closed:
		if info.Kind != CloseKindReadError || info.Code != CloseMessageTooBig {
			t.Fatalf("CloseInfo=%+v, want read_error/message_too_big", info)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for default read limit close")
	}
	select {
	case <-handled:
		t.Fatalf("Handle called for oversized default-limit frame")
	default:
	}
}

func TestConcurrentPumpRunPanics(t *testing.T) {
	client, server := managedPairForTest(t)
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	entered := make(chan struct{})
	go Pump{
		Conn: client,
		Handle: func(context.Context, MessageType, []byte) {
			close(entered)
		},
	}.Run(context.Background())
	if err := server.WriteMessage(TextMessage, []byte("frame")); err != nil {
		t.Fatalf("server write frame: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for first pump")
	}
	assertPanicContains(t, "Pump.Run invalid read state", func() {
		Pump{Conn: client}.Run(context.Background())
	})
}

func TestPumpPeerCloseClassifiedBeforeReturn(t *testing.T) {
	client, server := managedPairForTest(t)

	closed := make(chan CloseInfo, 1)
	go Pump{
		Conn: client,
		OnClose: func(info CloseInfo) {
			closed <- info
		},
	}.Run(context.Background())
	server.Close(CloseInfo{Kind: CloseKindNormal, Code: CloseNormalClosure, Reason: "bye"})

	select {
	case info := <-closed:
		if info.Kind != CloseKindPeerClose || info.Code != CloseNormalClosure || info.Reason != "bye" {
			t.Fatalf("CloseInfo=%+v, want peer close 1000 bye", info)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for peer close classification")
	}
}

func TestPumpPeerCloseMarksInboundActivity(t *testing.T) {
	now := time.Unix(1234, 0)
	activity := make(chan time.Time, 1)
	client, server := managedPairForTestWithConfigs(t, Config{
		Clock: staticClock{now: now},
		OnActivity: func(at time.Time) {
			activity <- at
		},
	}, Config{})

	closed := make(chan CloseInfo, 1)
	go Pump{
		Conn:    client,
		OnClose: func(info CloseInfo) { closed <- info },
	}.Run(context.Background())
	server.Close(CloseInfo{Kind: CloseKindNormal, Code: CloseNormalClosure, Reason: "bye"})

	select {
	case at := <-activity:
		if !at.Equal(now) {
			t.Fatalf("activity=%s, want %s", at, now)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for close activity")
	}
	select {
	case info := <-closed:
		if info.Kind != CloseKindPeerClose {
			t.Fatalf("CloseInfo=%+v, want peer close", info)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for peer close")
	}
}

func TestPumpReadErrorClassifiedBeforeReturn(t *testing.T) {
	client, server := managedPairForTest(t)
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	closed := make(chan CloseInfo, 1)
	go Pump{
		Conn: client,
		OnClose: func(info CloseInfo) {
			closed <- info
		},
	}.Run(context.Background())
	if err := server.raw.Close(); err != nil {
		t.Fatalf("close raw server conn: %v", err)
	}

	select {
	case info := <-closed:
		if info.Kind != CloseKindReadError {
			t.Fatalf("CloseInfo=%+v, want read error", info)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for read error classification")
	}
}

func TestZeroConfigDoesNotStartLivenessAndEOFExitsReadPath(t *testing.T) {
	client, server := managedPairForTestWithConfigs(t, Config{}, Config{})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	closed := make(chan CloseInfo, 1)
	go Pump{
		Conn: client,
		OnClose: func(info CloseInfo) {
			closed <- info
		},
	}.Run(context.Background())
	if err := server.raw.Close(); err != nil {
		t.Fatalf("close raw server conn: %v", err)
	}

	select {
	case info := <-closed:
		if info.Kind != CloseKindReadError {
			t.Fatalf("CloseInfo=%+v, want EOF/read error", info)
		}
		if info.Kind == CloseKindPongMiss || info.Kind == CloseKindInboundIdle {
			t.Fatalf("zero liveness config should not trigger watchdog close: %+v", info)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for zero-config EOF close")
	}
}

func TestPumpHandlerPanicClassifiedBeforeReturn(t *testing.T) {
	client, server := managedPairForTest(t)
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	closed := make(chan CloseInfo, 1)
	go Pump{
		Conn: client,
		Handle: func(context.Context, MessageType, []byte) {
			panic("boom")
		},
		OnClose: func(info CloseInfo) {
			closed <- info
		},
	}.Run(context.Background())
	if err := server.WriteMessage(TextMessage, []byte("frame")); err != nil {
		t.Fatalf("server write frame: %v", err)
	}

	select {
	case info := <-closed:
		if info.Kind != CloseKindHandlerPanic || info.Reason != "handler_panic" {
			t.Fatalf("CloseInfo=%+v, want handler panic", info)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for handler panic classification")
	}
}

func TestPumpContextCancelClosesAbortWithoutFallbackOverride(t *testing.T) {
	client, server := managedPairForTest(t)
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	ctx, cancel := context.WithCancel(context.Background())
	closed := make(chan CloseInfo, 1)
	go Pump{
		Conn: client,
		OnClose: func(info CloseInfo) {
			closed <- info
		},
	}.Run(ctx)
	cancel()

	select {
	case info := <-closed:
		if info.Kind != CloseKindAbort || info.Reason != "ctx_done" || !errors.Is(info.Err, context.Canceled) {
			t.Fatalf("CloseInfo=%+v, want ctx_done abort with context.Canceled", info)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for pump close after ctx cancel")
	}
}

func TestPumpContextWatcherGoroutineDoesNotLeakOnNormalExit(t *testing.T) {
	client, server := managedPairForTest(t)
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closed := make(chan struct{})
	go Pump{
		Conn: client,
		OnClose: func(CloseInfo) {
			close(closed)
		},
	}.Run(ctx)
	waitForGoroutineStackContains(t, "Pump.Run")

	server.Close(CloseInfo{Kind: CloseKindNormal, Code: CloseNormalClosure, Reason: "bye"})
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for pump close")
	}
	waitForNoGoroutineStackContains(t, "Pump.Run")
}

func TestOnActivityDataMessageOnlyDuringPump(t *testing.T) {
	activityAt := make(chan time.Time, 1)
	wantTime := time.Unix(123, 0)
	client, server := managedPairForTestWithConfigs(t, Config{
		Clock: staticClock{now: wantTime},
		OnActivity: func(at time.Time) {
			activityAt <- at
		},
	}, Config{})
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	if err := server.WriteMessage(TextMessage, []byte("first")); err != nil {
		t.Fatalf("server write first: %v", err)
	}
	if _, _, err := client.ReadInitial(context.Background()); err != nil {
		t.Fatalf("ReadInitial err=%v", err)
	}
	select {
	case at := <-activityAt:
		t.Fatalf("ReadInitial triggered OnActivity at %s", at)
	default:
	}

	handled := make(chan struct{}, 1)
	go Pump{
		Conn: client,
		Handle: func(context.Context, MessageType, []byte) {
			handled <- struct{}{}
		},
	}.Run(context.Background())
	if err := server.WriteMessage(TextMessage, []byte("second")); err != nil {
		t.Fatalf("server write second: %v", err)
	}
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for pump handle")
	}
	select {
	case got := <-activityAt:
		if !got.Equal(wantTime) {
			t.Fatalf("OnActivity time=%s, want %s", got, wantTime)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for OnActivity")
	}
}

func TestPumpSlowHandleObservationUsesFakeClock(t *testing.T) {
	clock := newManualClock(time.Unix(800, 0))
	client, server := managedPairForTestWithConfigs(t, Config{Clock: clock}, Config{})
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	recorder := &recordingSlowHandleObserver{
		observed:    make(chan time.Duration, 1),
		ctxObserved: make(chan context.Context, 1),
	}
	slowHandleObserverMu.Lock()
	previousRecorder := slowHandleRecorder
	slowHandleRecorder = recorder
	slowHandleObserverMu.Unlock()
	t.Cleanup(func() {
		slowHandleObserverMu.Lock()
		slowHandleRecorder = previousRecorder
		slowHandleObserverMu.Unlock()
	})

	handled := make(chan struct{}, 1)
	pumpCtx := context.WithValue(context.Background(), logger.RequestIdKey, "req-slow-handle")
	go Pump{
		Conn: client,
		Handle: func(context.Context, MessageType, []byte) {
			clock.Advance(5 * time.Millisecond)
			handled <- struct{}{}
		},
	}.Run(pumpCtx)

	if err := server.WriteMessage(TextMessage, []byte("slow")); err != nil {
		t.Fatalf("server write slow frame: %v", err)
	}
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for slow handle")
	}
	select {
	case elapsed := <-recorder.observed:
		if elapsed != 5*time.Millisecond {
			t.Fatalf("observed elapsed=%s, want 5ms", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for slow handle observation")
	}
	select {
	case ctx := <-recorder.ctxObserved:
		if got := ctx.Value(logger.RequestIdKey); got != "req-slow-handle" {
			t.Fatalf("observed request id=%v, want req-slow-handle", got)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for slow handle context")
	}
}

func TestPumpInboundPingTriggersActivityAndQueuedPong(t *testing.T) {
	activityAt := make(chan time.Time, 1)
	wantTime := time.Unix(789, 0)
	client, server := managedPairForTestWithConfigs(t, Config{
		Clock: staticClock{now: wantTime},
		OnActivity: func(at time.Time) {
			activityAt <- at
		},
	}, Config{})
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	pongReceived := make(chan string, 1)
	server.raw.SetPongHandler(func(appData string) error {
		pongReceived <- appData
		return nil
	})
	readDone := make(chan error, 1)
	go func() {
		_ = server.raw.SetReadDeadline(time.Now().Add(time.Second))
		_, _, err := server.raw.ReadMessage()
		readDone <- err
	}()

	go Pump{Conn: client}.Run(context.Background())
	if err := server.raw.WriteControl(websocket.PingMessage, []byte("probe"), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("server write ping: %v", err)
	}

	select {
	case got := <-activityAt:
		if !got.Equal(wantTime) {
			t.Fatalf("OnActivity time=%s, want %s", got, wantTime)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for ping activity")
	}
	select {
	case got := <-pongReceived:
		if got != "probe" {
			t.Fatalf("pong payload=%q, want probe", got)
		}
	case err := <-readDone:
		t.Fatalf("server read ended before pong: %v", err)
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for queued pong")
	}
}

func TestPumpInboundPingPongEnqueueFailureClassifiedAsWriteError(t *testing.T) {
	client, server := managedPairForTest(t)
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	closed := make(chan CloseInfo, 1)
	go Pump{
		Conn:    client,
		OnClose: func(info CloseInfo) { closed <- info },
	}.Run(context.Background())
	client.control.Stop()

	if err := server.raw.WriteControl(websocket.PingMessage, []byte("probe"), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("server write ping: %v", err)
	}

	select {
	case info := <-closed:
		if info.Kind != CloseKindWriteError || info.Reason != "enqueue_pong_failed" {
			t.Fatalf("CloseInfo=%+v, want write_error/enqueue_pong_failed", info)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for enqueue pong write error")
	}
}

func TestPumpInboundPongTriggersActivity(t *testing.T) {
	activityAt := make(chan time.Time, 1)
	wantTime := time.Unix(790, 0)
	client, server := managedPairForTestWithConfigs(t, Config{
		Clock: staticClock{now: wantTime},
		OnActivity: func(at time.Time) {
			activityAt <- at
		},
	}, Config{})
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	go Pump{Conn: client}.Run(context.Background())
	if err := server.raw.WriteControl(websocket.PongMessage, []byte("probe"), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("server write pong: %v", err)
	}

	select {
	case got := <-activityAt:
		if !got.Equal(wantTime) {
			t.Fatalf("OnActivity time=%s, want %s", got, wantTime)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for pong activity")
	}
}

func TestPumpInboundPongObservesGeneration(t *testing.T) {
	clock := newManualClock(time.Unix(791, 0))
	client, server := managedPairForTestWithConfigs(t, Config{Clock: clock}, Config{})
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	client.pong.mu.Lock()
	client.pong.awaiting = true
	client.pong.outstandingGen = 7
	client.pong.outstandingTimer = clock.AfterFunc(time.Hour, func() {
		t.Error("unexpected pong miss after matching inbound pong")
	})
	client.pong.mu.Unlock()

	go Pump{Conn: client}.Run(context.Background())
	payload := make([]byte, 8)
	binary.BigEndian.PutUint64(payload, 7)
	if err := server.raw.WriteControl(websocket.PongMessage, payload, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("server write pong: %v", err)
	}

	deadline := time.After(time.Second)
	for {
		client.pong.mu.Lock()
		awaiting := client.pong.awaiting
		outstandingGen := client.pong.outstandingGen
		outstandingTimer := client.pong.outstandingTimer
		client.pong.mu.Unlock()
		if !awaiting && outstandingGen == 0 && outstandingTimer == nil {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for inbound pong generation observation: awaiting=%v outstanding=%d timer=%v", awaiting, outstandingGen, outstandingTimer)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestPumpInboundMismatchedPongTriggersActivityWithoutClearingOutstanding(t *testing.T) {
	activityAt := make(chan time.Time, 1)
	wantTime := time.Unix(792, 0)
	client, server := managedPairForTestWithConfigs(t, Config{
		Clock: staticClock{now: wantTime},
		OnActivity: func(at time.Time) {
			activityAt <- at
		},
	}, Config{})
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	client.pong.mu.Lock()
	client.pong.awaiting = true
	client.pong.outstandingGen = 7
	client.pong.outstandingTimer = nil
	client.pong.mu.Unlock()

	go Pump{Conn: client}.Run(context.Background())
	payload := make([]byte, 8)
	binary.BigEndian.PutUint64(payload, 99)
	if err := server.raw.WriteControl(websocket.PongMessage, payload, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("server write pong: %v", err)
	}

	select {
	case got := <-activityAt:
		if !got.Equal(wantTime) {
			t.Fatalf("OnActivity time=%s, want %s", got, wantTime)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for mismatched pong activity")
	}
	client.pong.mu.Lock()
	defer client.pong.mu.Unlock()
	if !client.pong.awaiting || client.pong.outstandingGen != 7 {
		t.Fatalf("mismatched pong cleared outstanding state: awaiting=%v outstanding=%d", client.pong.awaiting, client.pong.outstandingGen)
	}
}

func TestReadInitialDoesNotStartLivenessTimers(t *testing.T) {
	client, server := managedPairForTestWithConfigs(t, Config{
		Clock:                  staticClock{now: time.Unix(456, 0)},
		PingInterval:           time.Second,
		PongMissTimeout:        time.Second,
		InboundActivityTimeout: func() time.Duration { return time.Second },
	}, Config{})
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	if err := server.WriteMessage(TextMessage, []byte("first")); err != nil {
		t.Fatalf("server write first: %v", err)
	}
	if _, _, err := client.ReadInitial(context.Background()); err != nil {
		t.Fatalf("ReadInitial err=%v", err)
	}
}

func TestReadInitialRepeatedCallPanics(t *testing.T) {
	client, server := managedPairForTest(t)
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	if err := server.WriteMessage(TextMessage, []byte("first")); err != nil {
		t.Fatalf("server write first: %v", err)
	}
	if _, _, err := client.ReadInitial(context.Background()); err != nil {
		t.Fatalf("ReadInitial err=%v", err)
	}
	assertPanicContains(t, "ReadInitial invalid read state", func() {
		_, _, _ = client.ReadInitial(context.Background())
	})
}

func TestPumpRunThenReadInitialPanics(t *testing.T) {
	client, server := managedPairForTest(t)
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	entered := make(chan struct{})
	go Pump{
		Conn: client,
		Handle: func(context.Context, MessageType, []byte) {
			close(entered)
		},
	}.Run(context.Background())
	if err := server.WriteMessage(TextMessage, []byte("frame")); err != nil {
		t.Fatalf("server write frame: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for pump handle")
	}
	assertPanicContains(t, "ReadInitial invalid read state", func() {
		_, _, _ = client.ReadInitial(context.Background())
	})
}

func TestReadInitialFailureThenPumpPanics(t *testing.T) {
	client, server := managedPairForTest(t)
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := client.ReadInitial(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadInitial err=%v, want context.Canceled", err)
	}
	assertPanicContains(t, "Pump.Run invalid read state", func() {
		Pump{Conn: client}.Run(context.Background())
	})
}

func TestPumpRunDuringReadInitialPanics(t *testing.T) {
	client, server := managedPairForTest(t)
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	ctx, cancel := context.WithCancel(context.Background())
	readDone := make(chan error, 1)
	go func() {
		_, _, err := client.ReadInitial(ctx)
		readDone <- err
	}()
	deadline := time.After(time.Second)
	for client.readState.Load() != readInitialActive {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for ReadInitial to become active")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	assertPanicContains(t, "Pump.Run invalid read state", func() {
		Pump{Conn: client}.Run(context.Background())
	})
	cancel()
	select {
	case err := <-readDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ReadInitial err=%v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for ReadInitial to exit")
	}
}

func TestPumpExitThenReadLifecyclePanics(t *testing.T) {
	client, server := managedPairForTest(t)
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	closed := make(chan struct{})
	go Pump{
		Conn: client,
		OnClose: func(CloseInfo) {
			close(closed)
		},
	}.Run(context.Background())
	server.Close(CloseInfo{Kind: CloseKindNormal, Code: CloseNormalClosure, Reason: "bye"})
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for pump close")
	}
	secondClose := make(chan CloseInfo, 1)
	Pump{
		Conn: client,
		OnClose: func(info CloseInfo) {
			secondClose <- info
		},
	}.Run(context.Background())
	select {
	case info := <-secondClose:
		if info.Kind != CloseKindPeerClose || info.Code != CloseNormalClosure {
			t.Fatalf("second Pump.Run close info=%+v, want prior peer close", info)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for already-closed pump close")
	}
	assertPanicContains(t, "ReadInitial invalid read state", func() {
		_, _, _ = client.ReadInitial(context.Background())
	})
}

func assertPanicContains(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q", want)
		}
		if !strings.Contains(r.(string), want) {
			t.Fatalf("panic=%q, want substring %q", r, want)
		}
	}()
	fn()
}

func waitForGoroutineStackContains(t testing.TB, needle string) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		if goroutineStackContains(needle) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for goroutine stack containing %q", needle)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func waitForNoGoroutineStackContains(t testing.TB, needle string) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		if !goroutineStackContains(needle) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for goroutine stack containing %q to exit", needle)
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func goroutineStackContains(needle string) bool {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Contains(string(buf[:n]), needle)
}

type recordingSlowHandleObserver struct {
	observed    chan time.Duration
	ctxObserved chan context.Context
}

func (r *recordingSlowHandleObserver) Observe(ctx context.Context, elapsed time.Duration) {
	r.observed <- elapsed
	r.ctxObserved <- ctx
}
