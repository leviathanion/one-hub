package requester

import (
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

type recordingWSWriterConn struct {
	mu          sync.Mutex
	deadlineErr error
	writeErr    error
	controlErr  error
	deadlines   []time.Time
	writes      [][]byte
	controls    [][]byte
	closeCount  int
}

type recordingWSReadLimitConn struct {
	limit int64
}

func (c *recordingWSReadLimitConn) SetReadLimit(limit int64) {
	c.limit = limit
}

func (c *recordingWSWriterConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadlines = append(c.deadlines, deadline)
	return c.deadlineErr
}

func (c *recordingWSWriterConn) WriteMessage(_ int, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writeErr != nil {
		return c.writeErr
	}
	c.writes = append(c.writes, append([]byte(nil), payload...))
	return nil
}

func (c *recordingWSWriterConn) WriteControl(_ int, payload []byte, _ time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.controlErr != nil {
		return c.controlErr
	}
	c.controls = append(c.controls, append([]byte(nil), payload...))
	return nil
}

func (c *recordingWSWriterConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeCount++
	return nil
}

func TestSafeWSCloseReasonTruncatesWithoutBreakingUTF8(t *testing.T) {
	reason := strings.Repeat("你", 50)
	got := SafeWSCloseReason(reason)
	if len(got) > wsCloseReasonMaxBytes {
		t.Fatalf("expected close reason to fit control frame budget, got %d bytes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("expected valid UTF-8 close reason, got %q", got)
	}
	if len([]rune(got)) != 41 {
		t.Fatalf("expected 41 three-byte runes to fit in 123 bytes, got %d", len([]rune(got)))
	}
}

func TestSafeWSCloseReasonDropsInvalidUTF8(t *testing.T) {
	got := SafeWSCloseReason(string([]byte{'o', 'k', 0xff, 'x'}))
	if got != "okx" {
		t.Fatalf("expected invalid UTF-8 byte to be dropped, got %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("expected valid UTF-8 close reason, got %q", got)
	}
}

func TestWSClientWriterReturnsDeadlineErrorWithoutWriting(t *testing.T) {
	deadlineErr := errors.New("deadline failed")
	conn := &recordingWSWriterConn{deadlineErr: deadlineErr}
	writer := NewWSClientWriter(conn, func() time.Duration { return time.Second })

	err := writer.WriteMessage(websocket.TextMessage, []byte("payload"))
	if !errors.Is(err, deadlineErr) {
		t.Fatalf("expected deadline error, got %v", err)
	}
	if len(conn.writes) != 0 {
		t.Fatalf("expected deadline failure not to write, got %d writes", len(conn.writes))
	}
}

func TestWSClientWriterHandlesTypedNilConn(t *testing.T) {
	var conn *websocket.Conn
	writer := NewWSClientWriter(conn, func() time.Duration { return time.Second })

	if err := writer.Close(); err != nil {
		t.Fatalf("expected typed nil close to be ignored, got %v", err)
	}
	if err := writer.WriteMessage(websocket.TextMessage, []byte("payload")); err == nil {
		t.Fatal("expected typed nil writer to reject data writes")
	}
}

func TestWSClientWriterWriteCloseUsesSafeReason(t *testing.T) {
	conn := &recordingWSWriterConn{}
	writer := NewWSClientWriter(conn, func() time.Duration { return time.Second })

	if err := writer.WriteClose(websocket.CloseNormalClosure, strings.Repeat("你", 50)); err != nil {
		t.Fatalf("expected close control write to succeed, got %v", err)
	}
	if len(conn.controls) != 1 {
		t.Fatalf("expected one close control frame, got %d", len(conn.controls))
	}
	payload := conn.controls[0]
	if len(payload) > 125 {
		t.Fatalf("expected close control payload to fit 125-byte limit, got %d", len(payload))
	}
	if code := int(binary.BigEndian.Uint16(payload[:2])); code != websocket.CloseNormalClosure {
		t.Fatalf("expected close code %d, got %d", websocket.CloseNormalClosure, code)
	}
	if !utf8.Valid(payload[2:]) {
		t.Fatalf("expected close reason payload to be valid UTF-8, got %q", payload[2:])
	}
}

func TestWSClientWriterRejectsWritesAfterClose(t *testing.T) {
	conn := &recordingWSWriterConn{}
	writer := NewWSClientWriter(conn, func() time.Duration { return time.Second })

	if err := writer.Close(); err != nil {
		t.Fatalf("expected close to succeed, got %v", err)
	}
	if err := writer.WriteMessage(websocket.TextMessage, []byte("payload")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("expected closed data write to return net.ErrClosed, got %v", err)
	}
	if err := writer.WriteControl(websocket.PingMessage, nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("expected closed control write to return net.ErrClosed, got %v", err)
	}
	if len(conn.writes) != 0 || len(conn.controls) != 0 {
		t.Fatalf("expected closed writer not to touch conn, writes=%d controls=%d", len(conn.writes), len(conn.controls))
	}
}

func TestWSClientWriterCloseInterruptsWithoutWaitingForWriteMutex(t *testing.T) {
	conn := &recordingWSWriterConn{}
	writer := NewWSClientWriter(conn, func() time.Duration { return time.Second })

	writer.mu.Lock()
	done := make(chan error, 1)
	go func() {
		done <- writer.Close()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected close to succeed while write mutex is held, got %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		writer.mu.Unlock()
		t.Fatal("expected close not to wait for the write mutex")
	}
	writer.mu.Unlock()

	conn.mu.Lock()
	closeCount := conn.closeCount
	conn.mu.Unlock()
	if closeCount != 1 {
		t.Fatalf("expected close to reach underlying websocket exactly once, got %d", closeCount)
	}
}

func TestApplyWSReadLimitUsesConfiguredLimitAndFallback(t *testing.T) {
	conn := &recordingWSReadLimitConn{}

	if got := ApplyWSReadLimit(conn, func() int64 { return 4096 }); got != 4096 || conn.limit != 4096 {
		t.Fatalf("expected configured read limit 4096, got return=%d applied=%d", got, conn.limit)
	}

	if got := ApplyWSReadLimit(conn, func() int64 { return 0 }); got != 16<<20 || conn.limit != 16<<20 {
		t.Fatalf("expected fallback read limit, got return=%d applied=%d", got, conn.limit)
	}
}

func TestWSActiveCounterGuardReleaseIsIdempotent(t *testing.T) {
	releases := 0
	guard := NewWSActiveCounterGuard(func() {
		releases++
	})

	if !guard.Release() {
		t.Fatal("expected first release to run")
	}
	if guard.Release() {
		t.Fatal("expected second release to be ignored")
	}
	if releases != 1 {
		t.Fatalf("expected one release, got %d", releases)
	}
	if !guard.Released() {
		t.Fatal("expected guard to report released")
	}
}

func TestWriteWSLocalErrorUsesSharedWriterWithoutEmptyFrame(t *testing.T) {
	conn := &recordingWSWriterConn{}
	writer := NewWSClientWriter(conn, func() time.Duration { return time.Second })

	if err := WriteWSLocalError(writer, nil); err != nil {
		t.Fatalf("expected empty local error payload to be ignored, got %v", err)
	}
	if len(conn.writes) != 0 {
		t.Fatalf("expected empty payload not to write, got %d writes", len(conn.writes))
	}

	if err := WriteWSLocalError(writer, []byte(`{"type":"error"}`)); err != nil {
		t.Fatalf("expected local error frame to write, got %v", err)
	}
	if len(conn.writes) != 1 || string(conn.writes[0]) != `{"type":"error"}` {
		t.Fatalf("expected one local error frame, got %#v", conn.writes)
	}
}
