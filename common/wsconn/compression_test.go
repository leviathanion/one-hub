package wsconn

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func compressedManagedPair(t *testing.T, clientConfig Config, peerCompression bool, opts ...DialOption) (*ManagedConn, *ManagedConn, string) {
	t.Helper()
	type acceptedConnection struct {
		conn      *ManagedConn
		extension string
	}
	accepted := make(chan acceptedConnection, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := AcceptManaged(w, r, Config{}, AcceptOptions{EnableCompression: peerCompression})
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		accepted <- acceptedConnection{conn, r.Header.Get("Sec-WebSocket-Extensions")}
	}))
	t.Cleanup(server.Close)
	opts = append(opts, WithDialSecurityPolicy(DialSecurityPolicy{AllowInsecureWS: true, AllowPrivateIP: true}))
	client, err := DialManaged(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil, clientConfig, opts...)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { client.Close(CloseInfo{Kind: CloseKindAbort}) })
	select {
	case result := <-accepted:
		t.Cleanup(func() { result.conn.Close(CloseInfo{Kind: CloseKindAbort}) })
		return client, result.conn, result.extension
	case <-time.After(time.Second):
		t.Fatal("timed out accepting websocket")
		return nil, nil, ""
	}
}

func TestDialCompressionNegotiationPreservesMessages(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
		peer    bool
	}{
		{name: "negotiated", enabled: true, peer: true},
		{name: "peer declines", enabled: true},
		{name: "default does not offer", peer: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var opts []DialOption
			if test.enabled {
				opts = append(opts, WithCompression())
			}
			client, server, extension := compressedManagedPair(t, Config{}, test.peer, opts...)
			if offered := strings.Contains(extension, "permessage-deflate"); offered != test.enabled {
				t.Fatalf("compression offer=%q, enabled=%v", extension, test.enabled)
			}
			payload := []byte(`{"future_field":{"number":9007199254740993,"value":null},"text":"` + strings.Repeat("压缩消息", 1024) + `"}`)
			for _, pair := range [][2]*ManagedConn{{client, server}, {server, client}} {
				if err := pair[0].WriteMessage(TextMessage, payload); err != nil {
					t.Fatal(err)
				}
				mt, got, err := pair[1].ReadInitial(t.Context())
				if err != nil || mt != TextMessage || !bytes.Equal(got, payload) {
					t.Fatalf("message changed across compression: type=%d bytes=%d err=%v", mt, len(got), err)
				}
			}
		})
	}
}

func TestCompressedInitialMessageUsesDecodedLimitAndReservation(t *testing.T) {
	const limit = 1024
	for _, size := range []int{limit, limit + 1} {
		client, server, _ := compressedManagedPair(t, Config{ReadLimit: limit}, true, WithCompression())
		if err := server.WriteMessage(TextMessage, bytes.Repeat([]byte("x"), size)); err != nil {
			t.Fatal(err)
		}
		reserved := 0
		_, payload, err := client.ReadInitialWithReservation(t.Context(), func(n int) bool { reserved += n; return true })
		if size > limit {
			if !errors.Is(err, ErrFirstFrameTooLarge) || payload != nil || reserved > limit {
				t.Fatalf("oversized decoded first message: bytes=%d reserved=%d err=%v", len(payload), reserved, err)
			}
		} else if err != nil || len(payload) != size || reserved != size {
			t.Fatalf("decoded first message at limit: bytes=%d reserved=%d err=%v", len(payload), reserved, err)
		}
	}
}

func TestCompressedPumpMessageEnforcesDecodedLimit(t *testing.T) {
	const limit = 1024
	client, server, _ := compressedManagedPair(t, Config{ReadLimit: limit}, true, WithCompression())
	delivered := make(chan []byte, 2)
	closed := make(chan CloseInfo, 1)
	go Pump{
		Conn: client,
		Handle: func(_ context.Context, _ MessageType, payload []byte) {
			delivered <- payload
		},
		OnClose: func(info CloseInfo) { closed <- info },
	}.Run(t.Context())
	if err := server.WriteMessage(TextMessage, bytes.Repeat([]byte("x"), limit)); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-delivered:
		if len(payload) != limit {
			t.Fatalf("message length=%d, want %d", len(payload), limit)
		}
	case <-time.After(time.Second):
		t.Fatal("message at decoded limit was not delivered")
	}
	if err := server.WriteMessage(TextMessage, bytes.Repeat([]byte("x"), limit+1)); err != nil {
		t.Fatal(err)
	}
	select {
	case info := <-closed:
		if info.Code != CloseMessageTooBig || !errors.Is(info.Err, websocket.ErrReadLimit) {
			t.Fatalf("oversized decoded message close=%+v", info)
		}
	case <-time.After(time.Second):
		t.Fatal("oversized decoded message did not close the connection")
	}
	select {
	case payload := <-delivered:
		t.Fatalf("oversized message reached handler: %d bytes", len(payload))
	default:
	}
	_, _, err := server.ReadInitial(t.Context())
	var closeErr *CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != CloseMessageTooBig {
		t.Fatalf("peer close=%v, want 1009", err)
	}
}
