package requester

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type blockingWSControlFrameConn struct {
	writeStarted chan struct{}
	release      chan struct{}
	block        bool
	err          error
	writeCount   int32
	closeCount   int32
}

func newBlockingWSControlFrameConn(block bool, err error) *blockingWSControlFrameConn {
	return &blockingWSControlFrameConn{
		writeStarted: make(chan struct{}, 1),
		release:      make(chan struct{}),
		block:        block,
		err:          err,
	}
}

func (c *blockingWSControlFrameConn) WriteControl(int, []byte, time.Time) error {
	atomic.AddInt32(&c.writeCount, 1)
	select {
	case c.writeStarted <- struct{}{}:
	default:
	}
	if c.block {
		<-c.release
	}
	return c.err
}

func (c *blockingWSControlFrameConn) Close() error {
	atomic.AddInt32(&c.closeCount, 1)
	return nil
}

func waitWSControlFrameWriterDone(t *testing.T, writer *WSControlFrameWriter) {
	t.Helper()
	select {
	case <-writer.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for websocket control frame writer to stop")
	}
}

func TestWSControlFrameWriterEnqueueDoesNotWaitForBlockedWrite(t *testing.T) {
	conn := newBlockingWSControlFrameConn(true, nil)
	writer := NewWSControlFrameWriter(conn, WSControlFrameWriterOptions{QueueSize: 2})

	if err := writer.EnqueuePong("first"); err != nil {
		t.Fatalf("expected first control frame to enqueue, got %v", err)
	}
	select {
	case <-conn.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first control frame write to start")
	}

	done := make(chan error, 1)
	go func() {
		done <- writer.EnqueuePong("second")
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected second control frame to enqueue while first write is blocked, got %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("control frame enqueue waited for blocked WriteControl")
	}

	writer.Stop()
	close(conn.release)
	waitWSControlFrameWriterDone(t, writer)
}

func TestWSControlFrameWriterQueueFullReturnsError(t *testing.T) {
	conn := newBlockingWSControlFrameConn(true, nil)
	writer := NewWSControlFrameWriter(conn, WSControlFrameWriterOptions{QueueSize: 1})

	if err := writer.Enqueue(websocket.PongMessage, []byte("first")); err != nil {
		t.Fatalf("expected first control frame to enqueue, got %v", err)
	}
	select {
	case <-conn.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first control frame write to start")
	}
	if err := writer.Enqueue(websocket.PongMessage, []byte("second")); err != nil {
		t.Fatalf("expected second control frame to fill queue, got %v", err)
	}
	if err := writer.Enqueue(websocket.PongMessage, []byte("third")); !errors.Is(err, ErrWSControlFrameQueueFull) {
		t.Fatalf("expected queue full error, got %v", err)
	}

	writer.Stop()
	close(conn.release)
	waitWSControlFrameWriterDone(t, writer)
}

func TestWSControlFrameWriterClosesConnOnWriteFailure(t *testing.T) {
	writeErr := errors.New("write failed")
	conn := newBlockingWSControlFrameConn(false, writeErr)
	writer := NewWSControlFrameWriter(conn, WSControlFrameWriterOptions{QueueSize: 1})

	if err := writer.EnqueuePong("ping"); err != nil {
		t.Fatalf("expected control frame to enqueue, got %v", err)
	}
	waitWSControlFrameWriterDone(t, writer)
	if got := atomic.LoadInt32(&conn.closeCount); got != 1 {
		t.Fatalf("expected failed control writer to close conn once, got %d", got)
	}
}
