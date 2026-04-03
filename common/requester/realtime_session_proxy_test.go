package requester

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type fakeRealtimeSession struct {
	mu           sync.Mutex
	sendCalls    [][]byte
	sendErrors   []error
	recvResults  chan fakeRealtimeRecvResult
	recvBlocked  chan struct{}
	recvCanceled chan struct{}
	recvCancelMu sync.Once
	closeReasons []string
	abortCount   int
	closed       chan struct{}
	closeOnce    sync.Once

	supportsGracefulDetach bool
}

type fakeRealtimeRecvResult struct {
	messageType int
	payload     []byte
	usage       *types.UsageEvent
	err         error
}

type panicRealtimeSession struct{}

func newFakeRealtimeSession(sendErrors ...error) *fakeRealtimeSession {
	return &fakeRealtimeSession{
		sendErrors:  sendErrors,
		recvResults: make(chan fakeRealtimeRecvResult, 8),
		closed:      make(chan struct{}),
	}
}

func (s *fakeRealtimeSession) SendClient(ctx context.Context, mt int, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sendCalls = append(s.sendCalls, append([]byte(nil), payload...))
	if len(s.sendErrors) == 0 {
		return nil
	}

	err := s.sendErrors[0]
	s.sendErrors = s.sendErrors[1:]
	return err
}

func (s *fakeRealtimeSession) Recv(ctx context.Context) (int, []byte, *types.UsageEvent, error) {
	select {
	case result := <-s.recvResults:
		return result.messageType, result.payload, result.usage, result.err
	default:
	}

	select {
	case <-ctx.Done():
		if s.recvCanceled != nil {
			s.recvCancelMu.Do(func() {
				close(s.recvCanceled)
			})
		}
		if s.recvBlocked != nil {
			<-s.recvBlocked
		}
		return 0, nil, nil, ctx.Err()
	case result := <-s.recvResults:
		return result.messageType, result.payload, result.usage, result.err
	case <-s.closed:
		return 0, nil, nil, runtimesession.ErrSessionClosed
	}
}

func (s *fakeRealtimeSession) Detach(reason string) {
	s.mu.Lock()
	s.closeReasons = append(s.closeReasons, reason)
	s.mu.Unlock()
}

func (s *fakeRealtimeSession) Abort(reason string) {
	s.mu.Lock()
	s.closeReasons = append(s.closeReasons, reason)
	s.abortCount++
	s.mu.Unlock()

	s.closeOnce.Do(func() {
		close(s.closed)
	})
}

func (s *fakeRealtimeSession) SetTurnObserverFactory(factory runtimesession.TurnObserverFactory) {
	_ = factory
}

func (s *fakeRealtimeSession) SupportsGracefulDetach() bool {
	return s.supportsGracefulDetach
}

func (s *fakeRealtimeSession) closeReasonCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.closeReasons)
}

func (s *fakeRealtimeSession) lastCloseReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.closeReasons) == 0 {
		return ""
	}
	return s.closeReasons[len(s.closeReasons)-1]
}

func (s *fakeRealtimeSession) sendCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sendCalls)
}

func (s *fakeRealtimeSession) abortCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.abortCount
}

func (s *fakeRealtimeSession) enqueueRecv(messageType int, payload []byte, usage *types.UsageEvent, err error) {
	s.recvResults <- fakeRealtimeRecvResult{
		messageType: messageType,
		payload:     append([]byte(nil), payload...),
		usage:       usage,
		err:         err,
	}
}

func (s *panicRealtimeSession) SendClient(ctx context.Context, mt int, payload []byte) error {
	_ = ctx
	_ = mt
	_ = payload
	return nil
}

func (s *panicRealtimeSession) Recv(ctx context.Context) (int, []byte, *types.UsageEvent, error) {
	<-ctx.Done()
	return 0, nil, nil, ctx.Err()
}

func (s *panicRealtimeSession) Detach(reason string) {
	_ = reason
}

func (s *panicRealtimeSession) Abort(reason string) {
	panic("abort panic: " + reason)
}

func (s *panicRealtimeSession) SetTurnObserverFactory(factory runtimesession.TurnObserverFactory) {
	_ = factory
}

func TestRealtimeSessionProxyKeepsSessionOpenAfterRecoverableSendError(t *testing.T) {
	session := newFakeRealtimeSession(
		types.NewErrorEvent("evt_busy", "invalid_request_error", "session_busy", "execution session already has an inflight response"),
	)

	upgrader := websocket.Upgrader{}
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			close(serverDone)
			return
		}
		defer conn.Close()

		proxy := NewRealtimeSessionProxy(conn, session, time.Second)
		proxy.Start()
		proxy.Wait()

		select {
		case <-proxy.SupplierClosed():
		case <-time.After(2 * time.Second):
			t.Errorf("timed out waiting for supplier side to close")
		}

		close(serverDone)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	if err := clientConn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","event_id":"evt_busy"}`)); err != nil {
		t.Fatalf("failed to write first client event: %v", err)
	}

	_, payload, err := clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read recoverable error payload: %v", err)
	}
	if got := string(payload); !strings.Contains(got, "session_busy") {
		t.Fatalf("expected session_busy payload, got %q", got)
	}

	time.Sleep(100 * time.Millisecond)
	if got := session.closeReasonCount(); got != 0 {
		t.Fatalf("expected recoverable send error to keep session open, got %d close calls", got)
	}

	if err := clientConn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel","event_id":"evt_cancel"}`)); err != nil {
		t.Fatalf("failed to write second client event: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if session.sendCallCount() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := session.sendCallCount(); got != 2 {
		t.Fatalf("expected proxy to continue reading client messages after recoverable error, got %d send calls", got)
	}

	if err := clientConn.Close(); err != nil {
		t.Fatalf("failed to close client websocket: %v", err)
	}

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for proxy shutdown")
	}
}

func TestRealtimeSessionProxyForwardsPayloadBeforeTerminalSessionError(t *testing.T) {
	session := newFakeRealtimeSession()
	quotaErr := types.NewErrorEvent("evt_quota", "system_error", "system_error", "user quota is not enough")
	session.enqueueRecv(
		websocket.TextMessage,
		[]byte(`{"type":"response.completed","response":{"id":"resp_quota","status":"completed"}}`),
		&types.UsageEvent{InputTokens: 3, OutputTokens: 5, TotalTokens: 8},
		runtimesession.NewClientPayloadError(quotaErr, []byte(quotaErr.Error())),
	)

	upgrader := websocket.Upgrader{}
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			close(serverDone)
			return
		}
		defer conn.Close()

		proxy := NewRealtimeSessionProxy(conn, session, time.Second)
		proxy.Start()
		proxy.Wait()
		close(serverDone)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer clientConn.Close()

	_, payload, err := clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read forwarded payload: %v", err)
	}
	if got := string(payload); !strings.Contains(got, `"response.completed"`) {
		t.Fatalf("expected original payload before terminal error, got %q", got)
	}

	_, payload, err = clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read terminal error payload: %v", err)
	}
	if got := string(payload); !strings.Contains(got, "user quota is not enough") {
		t.Fatalf("expected terminal quota error payload, got %q", got)
	}

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for proxy shutdown")
	}

	if got := session.closeReasonCount(); got == 0 {
		t.Fatalf("expected terminal recv error to detach the session")
	}
}

func TestRealtimeSessionProxyDoesNotDuplicateTerminalErrorPayload(t *testing.T) {
	session := newFakeRealtimeSession()
	rawPayload := []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"bad_input","message":"bad request"}}`)
	session.enqueueRecv(websocket.TextMessage, rawPayload, nil, runtimesession.ErrSessionClosed)

	upgrader := websocket.Upgrader{}
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			close(serverDone)
			return
		}
		defer conn.Close()

		proxy := NewRealtimeSessionProxy(conn, session, time.Second)
		proxy.Start()
		proxy.Wait()
		close(serverDone)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer clientConn.Close()

	_, payload, err := clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read terminal error payload: %v", err)
	}
	if got := string(payload); got != string(rawPayload) {
		t.Fatalf("expected raw terminal error payload, got %q", got)
	}

	_ = clientConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, payload, err = clientConn.ReadMessage(); err == nil {
		t.Fatalf("expected proxy to close after single terminal error payload, got unexpected extra frame %q", payload)
	}

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for proxy shutdown after terminal error payload")
	}
}

func TestRealtimeSessionProxyWrapsGenericSendErrorsAsStructuredJSON(t *testing.T) {
	session := newFakeRealtimeSession(errors.New("boom"))

	upgrader := websocket.Upgrader{}
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			close(serverDone)
			return
		}
		defer conn.Close()

		proxy := NewRealtimeSessionProxy(conn, session, time.Second)
		proxy.Start()
		proxy.Wait()
		close(serverDone)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer clientConn.Close()

	if err := clientConn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","event_id":"evt_generic"}`)); err != nil {
		t.Fatalf("failed to write generic client event: %v", err)
	}

	_, payload, err := clientConn.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read structured proxy error: %v", err)
	}

	var event types.Event
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatalf("expected proxy error payload to be valid JSON, got %v", err)
	}
	if event.Type != types.EventTypeError {
		t.Fatalf("expected structured error event, got %q", event.Type)
	}
	if event.ErrorDetail == nil || event.ErrorDetail.Message != "boom" {
		t.Fatalf("expected structured proxy error payload to preserve message, got %+v", event.ErrorDetail)
	}

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for proxy shutdown")
	}
}

func TestRealtimeSessionProxyGracefulClientDisconnectDetachesWithoutAbort(t *testing.T) {
	session := newFakeRealtimeSession()
	session.supportsGracefulDetach = true

	upgrader := websocket.Upgrader{}
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			close(serverDone)
			return
		}
		defer conn.Close()

		proxy := NewRealtimeSessionProxy(conn, session, 500*time.Millisecond)
		proxy.Start()
		proxy.Wait()
		close(serverDone)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	if err := clientConn.Close(); err != nil {
		t.Fatalf("failed to close client websocket: %v", err)
	}

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for proxy shutdown after client disconnect")
	}

	if got := session.lastCloseReason(); got != "user_closed" {
		t.Fatalf("expected graceful client disconnect to detach session with user_closed, got %q", got)
	}
	if got := session.abortCallCount(); got != 0 {
		t.Fatalf("expected graceful client disconnect not to abort session, got %d abort calls", got)
	}
}

func TestRealtimeSessionProxyGracefulClientDisconnectAbortsNonDetachableSession(t *testing.T) {
	session := newFakeRealtimeSession()

	upgrader := websocket.Upgrader{}
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			close(serverDone)
			return
		}
		defer conn.Close()

		proxy := NewRealtimeSessionProxy(conn, session, 500*time.Millisecond)
		proxy.Start()
		proxy.Wait()
		close(serverDone)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	if err := clientConn.Close(); err != nil {
		t.Fatalf("failed to close client websocket: %v", err)
	}

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for proxy shutdown after client disconnect")
	}

	if got := session.lastCloseReason(); got != "user_closed" {
		t.Fatalf("expected graceful client disconnect to abort session with user_closed, got %q", got)
	}
	if got := session.abortCallCount(); got != 1 {
		t.Fatalf("expected graceful client disconnect to abort non-detachable session once, got %d abort calls", got)
	}
}

func TestRealtimeSessionProxyWaitBlocksUntilSessionToUserExits(t *testing.T) {
	session := newFakeRealtimeSession()
	session.supportsGracefulDetach = true
	session.recvBlocked = make(chan struct{})
	session.recvCanceled = make(chan struct{})

	upgrader := websocket.Upgrader{}
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			close(serverDone)
			return
		}
		defer conn.Close()

		proxy := NewRealtimeSessionProxy(conn, session, 500*time.Millisecond)
		proxy.Start()

		waitDone := make(chan struct{})
		go func() {
			proxy.Wait()
			close(waitDone)
		}()

		select {
		case <-session.recvCanceled:
		case <-time.After(2 * time.Second):
			t.Errorf("timed out waiting for session recv cancellation")
			close(session.recvBlocked)
			return
		}

		select {
		case <-waitDone:
			t.Errorf("expected Wait to block until sessionToUser exits")
		case <-time.After(150 * time.Millisecond):
		}

		close(session.recvBlocked)

		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
			t.Errorf("timed out waiting for Wait after sessionToUser exit")
		}

		close(serverDone)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}

	if err := clientConn.Close(); err != nil {
		t.Fatalf("failed to close client websocket: %v", err)
	}

	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		close(session.recvBlocked)
		t.Fatalf("timed out waiting for proxy wait semantics test")
	}
}

func TestRealtimeSessionProxyIdleWatchdogAbortsSession(t *testing.T) {
	session := newFakeRealtimeSession()

	upgrader := websocket.Upgrader{}
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			close(serverDone)
			return
		}
		defer conn.Close()

		proxy := NewRealtimeSessionProxy(conn, session, 150*time.Millisecond)
		proxy.Start()
		proxy.Wait()
		close(serverDone)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer clientConn.Close()

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for idle watchdog shutdown")
	}

	if got := session.lastCloseReason(); got != "idle_timeout" {
		t.Fatalf("expected idle watchdog to abort session with idle_timeout, got %q", got)
	}
	if got := session.abortCallCount(); got != 1 {
		t.Fatalf("expected idle watchdog to abort exactly once, got %d", got)
	}
}

func TestRealtimeSessionProxyCoordinateRecoversSessionAbortPanics(t *testing.T) {
	proxy := NewRealtimeSessionProxy(nil, &panicRealtimeSession{}, time.Second)
	proxy.exitCh <- realtimeProxyExit{source: "idle", err: context.DeadlineExceeded}

	waitDone := make(chan struct{})
	go func() {
		proxy.Wait()
		close(waitDone)
	}()

	go proxy.coordinate()

	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("expected Wait to return after coordinator panic recovery")
	}
}

func TestRealtimeSessionProxyActiveWorkersRecoverSessionAbortPanics(t *testing.T) {
	session := &panicRealtimeSession{}

	upgrader := websocket.Upgrader{}
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			close(serverDone)
			return
		}
		defer conn.Close()

		proxy := NewRealtimeSessionProxy(conn, session, 150*time.Millisecond)
		proxy.Start()
		proxy.Wait()

		select {
		case <-proxy.UserClosed():
		case <-time.After(2 * time.Second):
			t.Errorf("timed out waiting for user worker to close after abort panic recovery")
		}

		select {
		case <-proxy.SupplierClosed():
		case <-time.After(2 * time.Second):
			t.Errorf("timed out waiting for supplier worker to close after abort panic recovery")
		}

		close(serverDone)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer clientConn.Close()

	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("expected proxy to shut down after recovering from session abort panic with active workers")
	}
}
