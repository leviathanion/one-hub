package utils

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSocks5ProxyFuncHonorsContextDuringHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ctx = context.WithValue(ctx, ProxySock5AddrKey, "socks5://"+listener.Addr().String())

	start := time.Now()
	conn, err := Socks5ProxyFunc(ctx, "tcp", "example.com:443")
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("expected context deadline to interrupt socks handshake")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expected socks handshake to stop near context deadline, took %s", elapsed)
	}

	select {
	case conn := <-accepted:
		_ = conn.Close()
	default:
	}
}

func TestSetProxyNormalizesSocksScheme(t *testing.T) {
	ctx := SetProxy(" SOCKS5://127.0.0.1:1080 ", context.Background())
	if _, ok := ctx.Value(ProxyHTTPAddrKey).(string); ok {
		t.Fatal("expected normalized socks proxy not to be stored as HTTP proxy")
	}
	proxyAddr, ok := ctx.Value(ProxySock5AddrKey).(string)
	if !ok {
		t.Fatal("expected normalized socks proxy context key")
	}
	if !strings.HasPrefix(proxyAddr, "socks5://") {
		t.Fatalf("expected normalized socks scheme, got %q", proxyAddr)
	}
}
