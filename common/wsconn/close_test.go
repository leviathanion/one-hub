package wsconn

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

func TestSanitizeWireCloseCode(t *testing.T) {
	tests := map[int]CloseCode{
		3000: 3000,
		4408: 4408,
		4999: 4999,
		999:  CloseInternalServerErr,
		1004: CloseInternalServerErr,
		1005: CloseInternalServerErr,
		1006: CloseInternalServerErr,
		1015: CloseInternalServerErr,
		5000: CloseInternalServerErr,
	}
	for input, want := range tests {
		if got := SanitizeWireCloseCode(input); got != want {
			t.Fatalf("SanitizeWireCloseCode(%d)=%d, want %d", input, got, want)
		}
	}
}

func TestSafeCloseReasonUTF8AndLimit(t *testing.T) {
	got := SafeCloseReason(string([]byte{0xff, 0xfe}) + strings.Repeat("界", 80))
	if len(got) > closeReasonMaxBytes {
		t.Fatalf("reason length=%d, want <= %d", len(got), closeReasonMaxBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("reason is not valid utf8: %q", got)
	}
}

func TestSafeCloseReasonTruncatesWithoutBreakingUTF8(t *testing.T) {
	reason := strings.Repeat("你", 50)
	got := SafeCloseReason(reason)
	if len(got) > closeReasonMaxBytes {
		t.Fatalf("expected close reason to fit control frame budget, got %d bytes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("expected valid UTF-8 close reason, got %q", got)
	}
	if len([]rune(got)) != 41 {
		t.Fatalf("expected 41 three-byte runes to fit in 123 bytes, got %d", len([]rune(got)))
	}
}

func TestSafeCloseReasonDropsInvalidUTF8(t *testing.T) {
	got := SafeCloseReason(string([]byte{'o', 'k', 0xff, 'x'}))
	if got != "okx" {
		t.Fatalf("expected invalid UTF-8 byte to be dropped, got %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("expected valid UTF-8 close reason, got %q", got)
	}
}

func TestSafeCloseMessageOmitsPayloadForNoStatus(t *testing.T) {
	if payload := SafeCloseMessage(CloseNoStatusReceived, "ignored"); len(payload) != 0 {
		t.Fatalf("expected close-no-status payload to be empty, got %v", payload)
	}
}

func TestSafeCloseMessageFormatsCodeAndReason(t *testing.T) {
	payload := SafeCloseMessage(CloseNormalClosure, "ok")
	if len(payload) != 4 {
		t.Fatalf("expected code plus reason payload, got %v", payload)
	}
	if got := CloseCode(binary.BigEndian.Uint16(payload[:2])); got != CloseNormalClosure {
		t.Fatalf("expected close code %d, got %d", CloseNormalClosure, got)
	}
	if got := string(payload[2:]); got != "ok" {
		t.Fatalf("expected close reason to survive formatting, got %q", got)
	}
}

type recordingReadLimitConn struct {
	limit int64
}

func (c *recordingReadLimitConn) SetReadLimit(limit int64) {
	c.limit = limit
}

func TestReadLimitApplicationUsesConfiguredLimitAndFallback(t *testing.T) {
	conn := &recordingReadLimitConn{}

	if got := applyReadLimit(conn, func() int64 { return 4096 }); got != 4096 || conn.limit != 4096 {
		t.Fatalf("expected configured read limit 4096, got return=%d applied=%d", got, conn.limit)
	}

	if got := applyReadLimit(conn, func() int64 { return 0 }); got != defaultReadLimit || conn.limit != defaultReadLimit {
		t.Fatalf("expected fallback read limit, got return=%d applied=%d", got, conn.limit)
	}

	if got := applyReadLimit(nil, func() int64 { return 4096 }); got != 0 {
		t.Fatalf("expected nil conn to return 0, got %d", got)
	}
}

func TestCloseFirstWriteWinsAndLosersReturnFast(t *testing.T) {
	client, server := managedPairForTest(t)
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		client.Close(CloseInfo{Kind: CloseKindNormal, Reason: "first"})
	}()
	loserDone := make(chan time.Duration, 1)
	go func() {
		defer wg.Done()
		time.Sleep(time.Millisecond)
		start := time.Now()
		client.Close(CloseInfo{Kind: CloseKindAbort, Reason: "second"})
		loserDone <- time.Since(start)
	}()
	wg.Wait()
	if elapsed := <-loserDone; elapsed > time.Millisecond {
		t.Fatalf("losing Close blocked for %s", elapsed)
	}
	<-client.Done()
	info := client.CloseInfo()
	if info.Kind != CloseKindNormal || info.Reason != "first" {
		t.Fatalf("CloseInfo=%+v, want first normal close", info)
	}
}

func TestWriteMessageInvalidType(t *testing.T) {
	client, server := managedPairForTest(t)
	defer client.Close(CloseInfo{Kind: CloseKindAbort})
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	if err := client.WriteMessage(MessageType(9), []byte("x")); !errors.Is(err, ErrInvalidMessageType) {
		t.Fatalf("WriteMessage invalid type err=%v, want ErrInvalidMessageType", err)
	}
}

func TestWriteMessageAfterClose(t *testing.T) {
	client, server := managedPairForTest(t)
	defer server.Close(CloseInfo{Kind: CloseKindAbort})
	client.Close(CloseInfo{Kind: CloseKindAbort})
	<-client.Done()
	if err := client.WriteMessage(TextMessage, []byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("WriteMessage after close err=%v, want net.ErrClosed", err)
	}
}

func TestWriteMessageErrorClosesWithoutDeadlock(t *testing.T) {
	client, server := managedPairForTest(t)
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	if err := client.raw.Close(); err != nil {
		t.Fatalf("close raw client conn: %v", err)
	}
	if err := client.WriteMessage(TextMessage, []byte("payload")); err == nil {
		t.Fatalf("WriteMessage err=nil, want write failure")
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for cleanup after write failure")
	}
	if info := client.CloseInfo(); info.Kind != CloseKindWriteError {
		t.Fatalf("CloseInfo.Kind=%v, want CloseKindWriteError", info.Kind)
	}
}

func TestRuntimeNegativeTimeoutFallbacks(t *testing.T) {
	conn := &ManagedConn{
		cfg: Config{
			WriteTimeout: func() time.Duration { return -time.Millisecond },
			InboundActivityTimeout: func() time.Duration {
				return -time.Millisecond
			},
		},
	}
	if got := conn.runtimeWriteTimeout(); got != defaultWriteTimeout {
		t.Fatalf("runtimeWriteTimeout=%s, want default %s", got, defaultWriteTimeout)
	}
	if got := conn.runtimeInboundActivityTimeout(); got != 0 {
		t.Fatalf("runtimeInboundActivityTimeout=%s, want disabled", got)
	}
}

func TestOnCloseCannotDeadlockCloseOrWrite(t *testing.T) {
	client, server := managedPairForTest(t)
	defer server.Close(CloseInfo{Kind: CloseKindAbort})

	closed := make(chan struct{})
	go Pump{
		Conn: client,
		OnClose: func(info CloseInfo) {
			start := time.Now()
			client.Close(CloseInfo{Kind: CloseKindAbort, Reason: "from_on_close"})
			if elapsed := time.Since(start); elapsed > time.Millisecond {
				t.Fatalf("Close in OnClose blocked for %s", elapsed)
			}
			if err := client.WriteMessage(TextMessage, []byte("x")); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("WriteMessage in OnClose err=%v, want net.ErrClosed", err)
			}
			close(closed)
		},
	}.Run(context.Background())

	server.Close(CloseInfo{Kind: CloseKindNormal, Code: CloseNormalClosure, Reason: "bye"})
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for OnClose")
	}
}

func TestCloseKindDefaultWireCodes(t *testing.T) {
	tests := []struct {
		name string
		kind CloseKind
		want CloseCode
	}{
		{name: "normal", kind: CloseKindNormal, want: CloseNormalClosure},
		{name: "graceful shutdown", kind: CloseKindGracefulShutdown, want: CloseGoingAway},
		{name: "inbound idle", kind: CloseKindInboundIdle, want: CloseGoingAway},
		{name: "backpressure", kind: CloseKindBackpressure, want: CloseTryAgainLater},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, server := managedPairForTest(t)
			defer server.Close(CloseInfo{Kind: CloseKindAbort})

			client.Close(CloseInfo{Kind: tt.kind, Reason: tt.name})
			_, _, err := server.ReadInitial(context.Background())
			var closeErr *CloseError
			if !errors.As(err, &closeErr) {
				t.Fatalf("server ReadInitial err=%T %v, want *CloseError", err, err)
			}
			if closeErr.Code != tt.want {
				t.Fatalf("close code=%d, want %d", closeErr.Code, tt.want)
			}
		})
	}
}

func TestCloseSanitizesReservedWireCodes(t *testing.T) {
	tests := []struct {
		name string
		code CloseCode
	}{
		{name: "no status received", code: CloseNoStatusReceived},
		{name: "abnormal closure", code: CloseAbnormalClosure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, server := managedPairForTest(t)
			defer server.Close(CloseInfo{Kind: CloseKindAbort})

			client.Close(CloseInfo{Kind: CloseKindNormal, Code: tt.code, Reason: tt.name})
			_, _, err := server.ReadInitial(context.Background())
			var closeErr *CloseError
			if !errors.As(err, &closeErr) {
				t.Fatalf("server ReadInitial err=%T %v, want *CloseError", err, err)
			}
			if closeErr.Code != CloseInternalServerErr {
				t.Fatalf("close code=%d, want sanitized 1011", closeErr.Code)
			}
		})
	}
}

func TestCloseFrameBestEffortOnBrokenSocketStillCompletes(t *testing.T) {
	tests := []CloseKind{
		CloseKindInboundIdle,
		CloseKindBackpressure,
	}
	for _, kind := range tests {
		t.Run(string(kind), func(t *testing.T) {
			client, server := managedPairForTest(t)
			defer server.Close(CloseInfo{Kind: CloseKindAbort})

			if err := client.raw.Close(); err != nil {
				t.Fatalf("close raw client conn: %v", err)
			}
			client.Close(CloseInfo{Kind: kind, Reason: string(kind)})
			select {
			case <-client.Done():
			case <-time.After(time.Second):
				t.Fatalf("timed out waiting for cleanup after %s on broken socket", kind)
			}
			if info := client.CloseInfo(); info.Kind != kind {
				t.Fatalf("CloseInfo=%+v, want kind %s preserved", info, kind)
			}
		})
	}
}

func TestCleanupControlWriterWaitUsesFakeClock(t *testing.T) {
	clock := newManualClock(time.Unix(900, 0))
	conn := &ManagedConn{
		cfg:   Config{Clock: clock, WriteTimeout: func() time.Duration { return 5 * time.Millisecond }},
		clock: clock,
		done:  make(chan struct{}),
		control: &controlWriter{
			stop: make(chan struct{}),
			done: make(chan struct{}),
		},
	}

	conn.Close(CloseInfo{Kind: CloseKindAbort, Reason: "test"})
	clock.waitTimers(t, 1)
	select {
	case <-conn.Done():
		t.Fatalf("cleanup completed before fake cleanup wait window elapsed")
	default:
	}

	clock.Advance(5 * time.Millisecond)
	select {
	case <-conn.Done():
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for cleanup after fake cleanup wait window")
	}
	if info := conn.CloseInfo(); !info.At.Equal(time.Unix(900, 0)) {
		t.Fatalf("CloseInfo.At=%s, want fake clock start", info.At)
	}
}

func TestAbortLikeCloseKindsDoNotSendCloseFrame(t *testing.T) {
	tests := []CloseKind{
		CloseKindAbort,
		CloseKindPongMiss,
		CloseKindWriteError,
		CloseKindHandlerPanic,
		CloseKindReadError,
	}
	for _, kind := range tests {
		t.Run(string(kind), func(t *testing.T) {
			client, server := managedPairForTest(t)
			client.Close(CloseInfo{Kind: kind, Reason: string(kind)})
			_, _, err := server.ReadInitial(context.Background())
			var closeErr *CloseError
			if errors.As(err, &closeErr) && closeErr.Code != CloseAbnormalClosure {
				t.Fatalf("server got close frame %+v, want no close frame", closeErr)
			}
			server.Close(CloseInfo{Kind: CloseKindAbort})
		})
	}
}

func TestPeerCloseWritesExactlyOneCloseFrame(t *testing.T) {
	server, client, recorder, cleanup := managedServerWithDeadlineRecorder(t, Config{})
	defer cleanup()
	defer client.Close()

	recorder.resetCloseFrameWrites()
	closed := make(chan CloseInfo, 1)
	go Pump{
		Conn:    server,
		OnClose: func(info CloseInfo) { closed <- info },
	}.Run(context.Background())

	if err := client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("client write close: %v", err)
	}
	_, _, _ = client.ReadMessage()

	select {
	case info := <-closed:
		if info.Kind != CloseKindPeerClose || info.Code != CloseNormalClosure || info.Reason != "bye" {
			t.Fatalf("CloseInfo=%+v, want peer close 1000 bye", info)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for peer close")
	}
	if got := recorder.closeFrameWrites(); got != 1 {
		t.Fatalf("server close frame writes=%d, want exactly 1 gorilla close reply and no wsconn duplicate", got)
	}
}

func TestReadLimitWritesExactlyOneMessageTooBigCloseFrame(t *testing.T) {
	server, client, recorder, cleanup := managedServerWithDeadlineRecorder(t, Config{ReadLimit: 4})
	defer cleanup()
	defer client.Close()

	recorder.resetCloseFrameWrites()
	closed := make(chan CloseInfo, 1)
	go Pump{
		Conn:    server,
		OnClose: func(info CloseInfo) { closed <- info },
	}.Run(context.Background())

	if err := client.WriteMessage(websocket.TextMessage, []byte("12345")); err != nil {
		t.Fatalf("client write oversized frame: %v", err)
	}
	_, _, err := client.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("client ReadMessage err=%T %v, want websocket close error", err, err)
	}
	if closeErr.Code != websocket.CloseMessageTooBig {
		t.Fatalf("client close code=%d, want 1009", closeErr.Code)
	}

	select {
	case info := <-closed:
		if info.Kind != CloseKindReadError || info.Code != CloseMessageTooBig {
			t.Fatalf("CloseInfo=%+v, want read_error/message_too_big", info)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for read-limit close")
	}
	if got := recorder.closeFrameWrites(); got != 1 {
		t.Fatalf("server close frame writes=%d, want exactly 1 gorilla 1009 close and no wsconn duplicate", got)
	}
}

func TestReadInitialRawIOErrorDoesNotWriteCloseFrame(t *testing.T) {
	server, client, recorder, cleanup := managedServerWithDeadlineRecorder(t, Config{})
	defer cleanup()

	recorder.resetCloseFrameWrites()
	if err := client.UnderlyingConn().Close(); err != nil {
		t.Fatalf("close client network conn: %v", err)
	}
	_, _, err := server.ReadInitial(context.Background())
	if err == nil {
		t.Fatalf("ReadInitial err=nil, want raw IO error")
	}
	var closeErr *CloseError
	if errors.As(err, &closeErr) && closeErr.Code != CloseAbnormalClosure {
		t.Fatalf("ReadInitial close err=%+v, want abnormal or non-close IO error", closeErr)
	}
	server.Close(CloseInfo{Kind: CloseKindAbort})
	<-server.Done()
	if got := recorder.closeFrameWrites(); got != 0 {
		t.Fatalf("server close frame writes=%d, want none for raw IO ReadInitial failure", got)
	}
}

func TestWriteTimeoutAppliesToDataControlAndCloseFrames(t *testing.T) {
	timeout := 123 * time.Millisecond
	tests := []struct {
		name  string
		write func(*ManagedConn) error
		wait  func(*ManagedConn)
	}{
		{
			name: "data",
			write: func(conn *ManagedConn) error {
				return conn.WriteMessage(TextMessage, []byte("payload"))
			},
		},
		{
			name: "control",
			write: func(conn *ManagedConn) error {
				return conn.control.EnqueuePing([]byte("probe"))
			},
		},
		{
			name: "close",
			write: func(conn *ManagedConn) error {
				conn.Close(CloseInfo{Kind: CloseKindNormal, Reason: "done"})
				return nil
			},
			wait: func(conn *ManagedConn) { <-conn.Done() },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, client, recorder, cleanup := managedServerWithDeadlineRecorder(t, Config{
				WriteTimeout: func() time.Duration { return timeout },
			})
			defer cleanup()
			defer client.Close()
			defer server.Close(CloseInfo{Kind: CloseKindAbort})

			before := time.Now()
			if err := tt.write(server); err != nil {
				t.Fatalf("write %s: %v", tt.name, err)
			}
			if tt.wait != nil {
				tt.wait(server)
			}

			deadline := recorder.waitForNonZeroWriteDeadline(t)
			min := before.Add(timeout)
			max := time.Now().Add(timeout)
			if deadline.Before(min) || deadline.After(max) {
				t.Fatalf("write deadline=%s, want between %s and %s", deadline, min, max)
			}
		})
	}
}

func managedPairForTest(t *testing.T) (client, server *ManagedConn) {
	return managedPairForTestWithConfig(t, Config{})
}

func managedPairForTestWithConfig(t *testing.T, cfg Config) (client, server *ManagedConn) {
	return managedPairForTestWithConfigs(t, cfg, cfg)
}

func managedPairForTestWithConfigs(t *testing.T, clientCfg, serverCfg Config) (client, server *ManagedConn) {
	t.Helper()
	accepted := make(chan *ManagedConn, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := AcceptManaged(w, r, serverCfg, AcceptOptions{
			CheckOrigin: func(*http.Request) bool { return true },
		})
		if err != nil {
			t.Errorf("AcceptManaged: %v", err)
			return
		}
		accepted <- conn
	}))
	t.Cleanup(ts.Close)

	rawURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	client, err := DialManaged(t.Context(), rawURL, nil, clientCfg,
		WithDialSecurityPolicy(DialSecurityPolicy{
			AllowInsecureWS: true,
			AllowPrivateIP:  true,
			HostFilter:      func(string, []net.IP) bool { return true },
		}),
	)
	if err != nil {
		t.Fatalf("DialManaged: %v", err)
	}
	select {
	case server = <-accepted:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for accepted conn")
	}
	return client, server
}

type staticClock struct {
	now time.Time
}

func (c staticClock) Now() time.Time { return c.now }

func (staticClock) NewTimer(d time.Duration) Timer {
	return realClock{}.NewTimer(d)
}

func (staticClock) AfterFunc(time.Duration, func()) Timer {
	panic("staticClock.AfterFunc called")
}

func managedServerWithDeadlineRecorder(t *testing.T, cfg Config) (*ManagedConn, *websocket.Conn, *deadlineRecordingConn, func()) {
	t.Helper()
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	listener := &deadlineRecordingListener{Listener: base, accepted: make(chan *deadlineRecordingConn, 1)}
	accepted := make(chan *ManagedConn, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := AcceptManaged(w, r, cfg, AcceptOptions{CheckOrigin: func(*http.Request) bool { return true }})
		if err != nil {
			t.Errorf("AcceptManaged: %v", err)
			return
		}
		accepted <- conn
	})}
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	}()
	cleanup := func() {
		_ = srv.Close()
		_ = listener.Close()
	}

	rawURL := "ws://" + listener.Addr().String()
	client, _, err := websocket.DefaultDialer.Dial(rawURL, nil)
	if err != nil {
		cleanup()
		t.Fatalf("dial: %v", err)
	}
	var server *ManagedConn
	select {
	case server = <-accepted:
	case <-time.After(time.Second):
		client.Close()
		cleanup()
		t.Fatalf("timed out waiting for accepted managed conn")
	}
	var recorder *deadlineRecordingConn
	select {
	case recorder = <-listener.accepted:
	case <-time.After(time.Second):
		client.Close()
		cleanup()
		t.Fatalf("timed out waiting for recorded conn")
	}
	return server, client, recorder, cleanup
}

type deadlineRecordingListener struct {
	net.Listener
	accepted chan *deadlineRecordingConn
}

func (l *deadlineRecordingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	recorded := &deadlineRecordingConn{Conn: conn}
	select {
	case l.accepted <- recorded:
	default:
	}
	return recorded, nil
}

type deadlineRecordingConn struct {
	net.Conn
	mu             sync.Mutex
	writeDeadlines []time.Time
	closeWrites    int
	countWrites    bool
}

func (c *deadlineRecordingConn) Write(p []byte) (int, error) {
	c.recordCloseFrames(p)
	return c.Conn.Write(p)
}

func (c *deadlineRecordingConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadlines = append(c.writeDeadlines, t)
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}

func (c *deadlineRecordingConn) resetCloseFrameWrites() {
	c.mu.Lock()
	c.countWrites = true
	c.closeWrites = 0
	c.mu.Unlock()
}

func (c *deadlineRecordingConn) closeFrameWrites() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeWrites
}

func (c *deadlineRecordingConn) recordCloseFrames(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.countWrites {
		return
	}
	for i := 0; i < len(p); {
		if len(p)-i < 2 {
			return
		}
		first := p[i]
		second := p[i+1]
		opcode := first & 0x0f
		payloadLen := int(second & 0x7f)
		headerLen := 2
		switch payloadLen {
		case 126:
			if len(p)-i < headerLen+2 {
				return
			}
			payloadLen = int(p[i+2])<<8 | int(p[i+3])
			headerLen += 2
		case 127:
			return
		}
		if second&0x80 != 0 {
			headerLen += 4
		}
		if len(p)-i < headerLen+payloadLen {
			return
		}
		if opcode == websocket.CloseMessage {
			c.closeWrites++
		}
		i += headerLen + payloadLen
	}
}

func (c *deadlineRecordingConn) waitForNonZeroWriteDeadline(t *testing.T) time.Time {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		c.mu.Lock()
		for _, d := range c.writeDeadlines {
			if !d.IsZero() {
				c.mu.Unlock()
				return d
			}
		}
		c.mu.Unlock()
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for non-zero write deadline")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}
