package requester

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestWSRequesterNewRequestReturnsHandshakeErrorWithBoundedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("rate limited"))
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	requester := &WSRequester{WSClient: websocket.DefaultDialer}
	conn, err := requester.NewRequest(wsURL, nil)
	if conn != nil {
		_ = conn.Close()
		t.Fatalf("expected websocket dial to fail")
	}

	var handshakeErr *WSDialHandshakeError
	if !errors.As(err, &handshakeErr) {
		t.Fatalf("expected WSDialHandshakeError, got %T %v", err, err)
	}
	if handshakeErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected status 429, got %d", handshakeErr.StatusCode)
	}
	if handshakeErr.Header.Get("Retry-After") != "3" {
		t.Fatalf("expected Retry-After header to be preserved, got %q", handshakeErr.Header.Get("Retry-After"))
	}
	if string(handshakeErr.BodySnippet) != "rate limited" {
		t.Fatalf("expected handshake body snippet, got %q", handshakeErr.BodySnippet)
	}
}

func TestNewWSDialHandshakeErrorBoundsLargeBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{"X-Test": []string{"present"}},
		Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", wsDialBodySnippetLimit+8))),
	}

	err := newWSDialHandshakeError("ws://internal.example.test/v1/responses?api_key=secret", resp, errors.New("bad gateway"))
	var handshakeErr *WSDialHandshakeError
	if !errors.As(err, &handshakeErr) {
		t.Fatalf("expected WSDialHandshakeError, got %T %v", err, err)
	}
	if strings.Contains(handshakeErr.Error(), "internal.example.test") || strings.Contains(handshakeErr.Error(), "secret") {
		t.Fatalf("expected client-safe handshake error string, got %q", handshakeErr.Error())
	}
	if handshakeErr.URL != "ws://internal.example.test/v1/responses?api_key=secret" {
		t.Fatalf("expected diagnostic URL field to be preserved, got %q", handshakeErr.URL)
	}
	if len(handshakeErr.BodySnippet) != wsDialBodySnippetLimit || !handshakeErr.BodyTruncated {
		t.Fatalf("expected bounded truncated body snippet, len=%d truncated=%v", len(handshakeErr.BodySnippet), handshakeErr.BodyTruncated)
	}
	if handshakeErr.Header.Get("X-Test") != "present" {
		t.Fatalf("expected header clone, got %q", handshakeErr.Header.Get("X-Test"))
	}
}

func TestNewWSDialHandshakeErrorPreservesBodyReadError(t *testing.T) {
	readErr := errors.New("body read failed")
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       errorReadCloser{err: readErr},
	}

	err := newWSDialHandshakeError("ws://upstream.example.test/realtime", resp, errors.New("bad gateway"))
	var handshakeErr *WSDialHandshakeError
	if !errors.As(err, &handshakeErr) {
		t.Fatalf("expected WSDialHandshakeError, got %T %v", err, err)
	}
	if !errors.Is(handshakeErr.BodyReadErr, readErr) {
		t.Fatalf("expected body read error to be preserved, got %v", handshakeErr.BodyReadErr)
	}
	if len(handshakeErr.BodySnippet) != 0 || handshakeErr.BodyTruncated {
		t.Fatalf("expected no body snippet on read error, snippet=%q truncated=%v", handshakeErr.BodySnippet, handshakeErr.BodyTruncated)
	}
}

func TestSendWSJsonRequestCreatesBufferedResultChannels(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial websocket test server: %v", err)
	}
	defer conn.Close()

	stream, apiErr := SendWSJsonRequest[string](conn, map[string]string{"type": "test"}, func(*[]byte, chan string, chan error) {})
	if apiErr != nil {
		t.Fatalf("expected SendWSJsonRequest to succeed, got %v", apiErr)
	}
	if cap(stream.DataChan) != 1 || cap(stream.ErrChan) != 1 {
		t.Fatalf("expected buffered websocket reader channels, data=%d err=%d", cap(stream.DataChan), cap(stream.ErrChan))
	}
}

type errorReadCloser struct {
	err error
}

func (r errorReadCloser) Read([]byte) (int, error) {
	return 0, r.err
}

func (r errorReadCloser) Close() error {
	return nil
}
