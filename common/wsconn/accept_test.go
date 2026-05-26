package wsconn

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestAcceptManagedRejectsOriginAndUsesErrorCallback(t *testing.T) {
	var callbackStatus int
	var callbackReason error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = AcceptManaged(w, r, Config{}, AcceptOptions{
			CheckOrigin: func(*http.Request) bool { return false },
			Error: func(w http.ResponseWriter, r *http.Request, status int, reason error) {
				callbackStatus = status
				callbackReason = reason
				http.Error(w, "custom reject", status)
			},
		})
	}))
	defer server.Close()

	_, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), http.Header{
		"Origin": []string{"https://blocked.example"},
	})
	if err == nil {
		t.Fatalf("Dial err=nil, want origin rejection")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("response=%v, want 403", resp)
	}
	if callbackStatus != http.StatusForbidden || callbackReason == nil {
		t.Fatalf("callback status=%d reason=%v, want 403 with reason", callbackStatus, callbackReason)
	}
}

func TestAcceptManagedNilCheckOriginUsesGorillaDefaultPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := AcceptManaged(w, r, Config{}, AcceptOptions{})
		if err == nil {
			conn.Close(CloseInfo{Kind: CloseKindAbort, Reason: "test_cleanup"})
		}
	}))
	defer server.Close()

	_, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), http.Header{
		"Origin": []string{"https://blocked.example"},
	})
	if err == nil {
		t.Fatal("Dial err=nil, want default origin rejection")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("response=%v, want 403", resp)
	}
}

func TestAcceptManagedUpgradeFailureDoesNotLeakAcceptedResource(t *testing.T) {
	var managedAccepted atomic.Bool
	handlerReturned := make(chan struct{}, 1)
	connClosed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := AcceptManaged(w, r, Config{}, AcceptOptions{
			CheckOrigin: func(*http.Request) bool { return false },
		})
		if err == nil {
			t.Errorf("AcceptManaged err=nil, want upgrade failure")
		}
		if conn != nil {
			managedAccepted.Store(true)
			conn.Close(CloseInfo{Kind: CloseKindAbort})
		}
		handlerReturned <- struct{}{}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case connClosed <- struct{}{}:
			default:
			}
		}
	}
	server.Start()
	defer server.Close()

	_, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), http.Header{
		"Origin": []string{"https://blocked.example"},
	})
	if err == nil {
		t.Fatalf("Dial err=nil, want origin rejection")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("response=%v, want 403", resp)
	}
	if resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	select {
	case <-handlerReturned:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for rejected handler")
	}
	if managedAccepted.Load() {
		t.Fatalf("AcceptManaged returned ManagedConn on upgrade failure")
	}
	select {
	case <-connClosed:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for failed handshake connection to close")
	}
}

func TestAcceptManagedResponseHeaderSubprotocolAndCompression(t *testing.T) {
	accepted := make(chan *ManagedConn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := AcceptManaged(w, r, Config{}, AcceptOptions{
			CheckOrigin:       func(*http.Request) bool { return true },
			ResponseHeader:    http.Header{"X-Accept-Test": []string{"ok"}},
			ReadBufferSize:    1024,
			WriteBufferSize:   2048,
			EnableCompression: true,
			Subprotocols:      []string{"chosen-proto"},
		})
		if err != nil {
			t.Errorf("AcceptManaged: %v", err)
			return
		}
		accepted <- conn
	}))
	defer server.Close()

	dialer := websocket.Dialer{
		Subprotocols:      []string{"chosen-proto"},
		EnableCompression: true,
	}
	raw, resp, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer raw.Close()
	if got := resp.Header.Get("X-Accept-Test"); got != "ok" {
		t.Fatalf("response header=%q, want ok", got)
	}
	if got := raw.Subprotocol(); got != "chosen-proto" {
		t.Fatalf("subprotocol=%q, want chosen-proto", got)
	}
	if got := resp.Header.Get("Sec-WebSocket-Extensions"); !strings.Contains(got, "permessage-deflate") {
		t.Fatalf("extensions=%q, want permessage-deflate", got)
	}

	select {
	case conn := <-accepted:
		if got := conn.Subprotocol(); got != "chosen-proto" {
			t.Fatalf("managed subprotocol=%q, want chosen-proto", got)
		}
		conn.Close(CloseInfo{Kind: CloseKindAbort})
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for accepted conn")
	}
}

func TestAcceptUpgraderCopiesBufferSizes(t *testing.T) {
	upgrader := acceptUpgrader(AcceptOptions{
		ReadBufferSize:  1234,
		WriteBufferSize: 5678,
	})
	if upgrader.ReadBufferSize != 1234 {
		t.Fatalf("ReadBufferSize=%d, want 1234", upgrader.ReadBufferSize)
	}
	if upgrader.WriteBufferSize != 5678 {
		t.Fatalf("WriteBufferSize=%d, want 5678", upgrader.WriteBufferSize)
	}
}

func TestAcceptManagedInvalidConfigFailsBeforeUpgrade(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := AcceptManaged(w, r, Config{ReadLimit: -1}, AcceptOptions{
			CheckOrigin: func(*http.Request) bool { return true },
		})
		if !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("AcceptManaged err=%v, want ErrInvalidConfig", err)
		}
		http.Error(w, "invalid config", http.StatusInternalServerError)
	}))
	defer server.Close()

	_, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err == nil {
		t.Fatalf("Dial err=nil, want invalid config failure")
	}
	if resp == nil || resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("response=%v, want 500", resp)
	}
}
