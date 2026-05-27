package wsconn

import (
	"errors"
	"fmt"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"one-api/common/logger"

	"github.com/gorilla/websocket"
)

const controlQueueSize = 8

var (
	errControlWriterClosed = errors.New("wsconn: control writer closed")
	errControlQueueFull    = errors.New("wsconn: control writer queue full")
)

type controlFrame struct {
	messageType int
	payload     []byte
}

type controlWriter struct {
	conn *ManagedConn

	queue   chan controlFrame
	stop    chan struct{}
	done    chan struct{}
	writeFn func(controlFrame) error
	once    sync.Once
	mu      sync.Mutex
	stopped bool
}

func newControlWriter(conn *ManagedConn) *controlWriter {
	w := &controlWriter{
		conn:  conn,
		queue: make(chan controlFrame, controlQueueSize),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	w.writeFn = w.write
	go w.run()
	return w
}

func (w *controlWriter) EnqueuePing(payload []byte) error {
	return w.enqueue(websocket.PingMessage, payload)
}

func (w *controlWriter) EnqueuePong(payload []byte) error {
	return w.enqueue(websocket.PongMessage, payload)
}

func (w *controlWriter) enqueue(mt int, payload []byte) error {
	if w == nil {
		return errControlWriterClosed
	}
	frame := controlFrame{messageType: mt, payload: append([]byte(nil), payload...)}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return errControlWriterClosed
	}
	select {
	case <-w.done:
		return errControlWriterClosed
	default:
	}
	select {
	case <-w.done:
		return errControlWriterClosed
	case w.queue <- frame:
		return nil
	default:
		return errControlQueueFull
	}
}

func (w *controlWriter) Stop() {
	if w == nil {
		return
	}
	w.once.Do(func() {
		w.mu.Lock()
		w.stopped = true
		close(w.stop)
		w.mu.Unlock()
	})
}

func (w *controlWriter) Wait() {
	if w == nil {
		return
	}
	<-w.done
}

func (w *controlWriter) run() {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.SysError(fmt.Sprintf("wsconn control writer panic: %v", recovered))
			logger.SysError(fmt.Sprintf("stacktrace from panic: %s", string(debug.Stack())))
			if w != nil && w.conn != nil {
				w.conn.Close(CloseInfo{Kind: CloseKindWriteError, Reason: "control_writer_panic", Err: fmt.Errorf("%v", recovered)})
			}
		}
		close(w.done)
	}()
	for {
		select {
		case <-w.stop:
			return
		case frame := <-w.queue:
			write := w.write
			if w.writeFn != nil {
				write = w.writeFn
			}
			if err := write(frame); err != nil {
				w.conn.Close(CloseInfo{Kind: CloseKindWriteError, Reason: "control_write_failed", Err: err})
				return
			}
		}
	}
}

func (w *controlWriter) write(frame controlFrame) error {
	c := w.conn
	if c == nil || c.raw == nil {
		return net.ErrClosed
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closeStarted.Load() {
		return net.ErrClosed
	}
	timeout := c.runtimeWriteTimeout()
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	return c.raw.WriteControl(frame.messageType, frame.payload, deadline)
}
