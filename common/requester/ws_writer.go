package requester

import (
	"errors"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type wsWriterConn interface {
	SetWriteDeadline(time.Time) error
	WriteMessage(int, []byte) error
	WriteControl(messageType int, data []byte, deadline time.Time) error
	Close() error
}

type wsWriteDeadlineConn interface {
	SetWriteDeadline(time.Time) error
}

// WSClientWriter serializes writes to a downstream WebSocket connection.
//
// gorilla/websocket requires that no two goroutines invoke Write* concurrently
// on the same connection. This writer enforces that invariant with an internal
// mutex, applies a configured write deadline before each frame, and exposes an
// idempotent Close.
type WSClientWriter struct {
	conn         wsWriterConn
	writeTimeout func() time.Duration

	mu        sync.Mutex
	closed    atomic.Bool
	closeOnce sync.Once
}

// NewWSClientWriter wraps conn with single-writer semantics. writeTimeout is
// consulted before each frame; pass nil to use a default of 5 seconds.
func NewWSClientWriter(conn wsWriterConn, writeTimeout func() time.Duration) *WSClientWriter {
	if writeTimeout == nil {
		writeTimeout = defaultWSWriteTimeout
	}
	if isNilWSWriterConn(conn) {
		conn = nil
	}
	return &WSClientWriter{conn: conn, writeTimeout: writeTimeout}
}

func defaultWSWriteTimeout() time.Duration {
	return 5 * time.Second
}

func isNilWSWriterConn(conn wsWriterConn) bool {
	if conn == nil {
		return true
	}
	value := reflect.ValueOf(conn)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// WithWSWriteDeadline applies the configured websocket write deadline around a
// single gorilla write operation. The write callback's error is returned
// without transport-level classification.
func WithWSWriteDeadline(conn *websocket.Conn, writeTimeout func() time.Duration, write func() error) error {
	return withWSWriteDeadline(conn, writeTimeout, write)
}

func withWSWriteDeadline(conn wsWriteDeadlineConn, writeTimeout func() time.Duration, write func() error) error {
	if conn == nil {
		return errors.New("websocket write deadline conn is not configured")
	}
	if writeTimeout == nil {
		writeTimeout = defaultWSWriteTimeout
	}
	timeout := writeTimeout()
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	var err error
	if write != nil {
		err = write()
	}
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

// WriteMessage writes a data frame under the write mutex with the configured
// deadline applied. Returns the raw underlying error; classification is the
// caller's responsibility.
func (w *WSClientWriter) WriteMessage(messageType int, payload []byte) error {
	if w == nil || w.conn == nil {
		return errors.New("ws client writer is not configured")
	}
	if messageType == websocket.CloseMessage || messageType == websocket.PingMessage || messageType == websocket.PongMessage {
		return w.WriteControl(messageType, payload)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed.Load() {
		return net.ErrClosed
	}
	return withWSWriteDeadline(w.conn, w.writeTimeout, func() error {
		return w.conn.WriteMessage(messageType, payload)
	})
}

func (w *WSClientWriter) WriteControl(messageType int, payload []byte) error {
	if w == nil || w.conn == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed.Load() {
		return net.ErrClosed
	}
	timeout := w.writeTimeout()
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	return w.conn.WriteControl(messageType, payload, deadline)
}

// WriteClose writes a close control frame with a UTF-8 safe reason. Returns
// any error from the underlying control write so callers can log; the writer
// is not implicitly closed and Close must still be invoked to release the
// connection.
func (w *WSClientWriter) WriteClose(code int, reason string) error {
	return w.WriteControl(websocket.CloseMessage, SafeWSCloseMessage(code, reason))
}

// Close idempotently closes the underlying connection. Subsequent Write*
// invocations return net.ErrClosed without touching the connection.
func (w *WSClientWriter) Close() error {
	if w == nil {
		return nil
	}
	var err error
	w.closeOnce.Do(func() {
		w.closed.Store(true)
		if w.conn != nil {
			err = w.conn.Close()
		}
	})
	return err
}

// Closed reports whether Close has been invoked.
func (w *WSClientWriter) Closed() bool {
	if w == nil {
		return true
	}
	return w.closed.Load()
}
