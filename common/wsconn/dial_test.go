package wsconn

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "negative ping", cfg: Config{PingInterval: -time.Millisecond}},
		{name: "negative pong", cfg: Config{PingInterval: time.Second, PongMissTimeout: -time.Millisecond}},
		{name: "pong without ping", cfg: Config{PongMissTimeout: time.Second}},
		{name: "negative read limit", cfg: Config{ReadLimit: -1}},
		{name: "negative write timeout", cfg: Config{WriteTimeout: func() time.Duration { return -time.Millisecond }}},
		{name: "negative inbound timeout", cfg: Config{InboundActivityTimeout: func() time.Duration { return -time.Millisecond }}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DialManaged(context.Background(), "wss://example.com/ws", nil, tt.cfg)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("err=%v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestConfigAllowsZeroAndPingIntervalGreaterThanPongMiss(t *testing.T) {
	if err := validateConfig(Config{}); err != nil {
		t.Fatalf("zero Config validate err=%v", err)
	}
	if err := validateConfig(Config{PingInterval: time.Second, PongMissTimeout: time.Millisecond}); err != nil {
		t.Fatalf("PingInterval>=PongMissTimeout should be valid: %v", err)
	}
}

func TestDialSecurityPolicy(t *testing.T) {
	if _, err := DialManaged(context.Background(), "ws://example.com/ws", nil, Config{}); !errors.Is(err, ErrInsecureScheme) {
		t.Fatalf("default ws err=%v, want ErrInsecureScheme", err)
	}
	if _, err := DialManaged(context.Background(), "wss://127.0.0.1/ws", nil, Config{}); !errors.Is(err, ErrPrivateAddrBlocked) {
		t.Fatalf("default private err=%v, want ErrPrivateAddrBlocked", err)
	}
	if _, err := DialManaged(context.Background(), "wss://10.0.0.1/ws", nil, Config{}); !errors.Is(err, ErrPrivateAddrBlocked) {
		t.Fatalf("default 10.x err=%v, want ErrPrivateAddrBlocked", err)
	}
	if _, err := DialManaged(context.Background(), "wss://192.168.0.1/ws", nil, Config{}); !errors.Is(err, ErrPrivateAddrBlocked) {
		t.Fatalf("default 192.168.x err=%v, want ErrPrivateAddrBlocked", err)
	}
	if _, err := DialManaged(context.Background(), "wss://127.0.0.1/ws", nil, Config{},
		WithDialSecurityPolicy(DialSecurityPolicy{AllowPrivateIP: true}),
		WithNetDialContext(func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("dial attempted")
		}),
	); errors.Is(err, ErrPrivateAddrBlocked) {
		t.Fatalf("AllowPrivateIP err=%v, want loopback allowed before dial", err)
	}
	if _, err := DialManaged(context.Background(), "wss://10.0.0.1/ws", nil, Config{},
		WithDialSecurityPolicy(DialSecurityPolicy{AllowPrivateIP: true}),
		WithNetDialContext(func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("dial attempted")
		}),
	); errors.Is(err, ErrPrivateAddrBlocked) {
		t.Fatalf("AllowPrivateIP err=%v, want private IP allowed before dial", err)
	}
	if _, err := DialManaged(context.Background(), "wss://169.254.169.254/ws", nil, Config{},
		WithDialSecurityPolicy(DialSecurityPolicy{AllowPrivateIP: true}),
	); !errors.Is(err, ErrPrivateAddrBlocked) {
		t.Fatalf("metadata err=%v, want ErrPrivateAddrBlocked", err)
	}
	if _, err := DialManaged(context.Background(), "wss://[fd00:ec2::254]/ws", nil, Config{},
		WithDialSecurityPolicy(DialSecurityPolicy{AllowPrivateIP: true}),
	); !errors.Is(err, ErrPrivateAddrBlocked) {
		t.Fatalf("IPv6 metadata err=%v, want ErrPrivateAddrBlocked", err)
	}
	if _, err := DialManaged(context.Background(), "wss://169.254.169.254/ws", nil, Config{},
		WithDialSecurityPolicy(DialSecurityPolicy{
			AllowPrivateIP: true,
			HostFilter:     func(string, []net.IP) bool { return true },
		}),
	); !errors.Is(err, ErrPrivateAddrBlocked) {
		t.Fatalf("metadata with HostFilter err=%v, want ErrPrivateAddrBlocked", err)
	}

	var dialed bool
	_, err := DialManaged(context.Background(), "ws://127.0.0.1/ws", nil, Config{},
		WithDialSecurityPolicy(DialSecurityPolicy{
			AllowInsecureWS: true,
			HostFilter:      func(string, []net.IP) bool { return true },
		}),
		WithNetDialContext(func(context.Context, string, string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("dial attempted")
		}),
	)
	if err == nil || !dialed {
		t.Fatalf("expected dial attempt after HostFilter allow, err=%v dialed=%v", err, dialed)
	}
}

func TestDialErrorHasDiagnosticsAndRedactsHeaders(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Authorization", "secret-token")
		w.Header().Set("Cookie", "sid=secret")
		w.Header().Set("Sec-WebSocket-Protocol", "secret-proto")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(strings.Repeat("x", 16)))
	}))
	defer ts.Close()

	rawURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/realtime?authorization=secret-token&signature=secret-sig"
	_, err := DialManaged(context.Background(), rawURL, nil, Config{},
		WithDialSecurityPolicy(DialSecurityPolicy{
			AllowInsecureWS: true,
			MaxBodySnippet:  4,
			HostFilter:      func(string, []net.IP) bool { return true },
		}),
	)
	var dialErr *DialError
	if !errors.As(err, &dialErr) {
		t.Fatalf("err=%T %v, want *DialError", err, err)
	}
	if strings.Contains(dialErr.URL, "?") || strings.Contains(dialErr.URL, "secret") {
		t.Fatalf("DialError.URL leaked sensitive URL data: %q", dialErr.URL)
	}
	if dialErr.StatusCode != http.StatusUnauthorized || dialErr.CloseInfo.Kind != CloseKindDialFailed {
		t.Fatalf("unexpected DialError diagnostics: %+v", dialErr)
	}
	if got := dialErr.Header.Get("Authorization"); got != "[REDACTED]" {
		t.Fatalf("Authorization header=%q, want [REDACTED]", got)
	}
	if got := dialErr.Header.Get("Cookie"); got != "[REDACTED]" {
		t.Fatalf("Cookie header=%q, want [REDACTED]", got)
	}
	if got := dialErr.Header.Get("Sec-WebSocket-Protocol"); got != "[REDACTED]" {
		t.Fatalf("Sec-WebSocket-Protocol header=%q, want [REDACTED]", got)
	}
	if string(dialErr.BodySnippet) != "xxxx" || !dialErr.BodyTruncated {
		t.Fatalf("snippet=%q truncated=%v, want limited body", dialErr.BodySnippet, dialErr.BodyTruncated)
	}
	if strings.Contains(dialErr.Error(), "secret") {
		t.Fatalf("DialError.Error leaked sensitive value: %q", dialErr.Error())
	}
	if strings.Contains(dialErr.Error(), "?") {
		t.Fatalf("DialError.Error leaked query string: %q", dialErr.Error())
	}
}

func TestDialErrorRedactsURLUserInfoAndQuery(t *testing.T) {
	dialErr := &DialError{
		URL: "wss://user:secret-pass@upstream.example.test/realtime?api_key=secret-key&signature=secret-sig",
		Err: errors.New("bad handshake"),
	}

	msg := dialErr.Error()
	for _, leaked := range []string{"user", "secret", "api_key", "signature", "?"} {
		if strings.Contains(msg, leaked) {
			t.Fatalf("DialError.Error leaked %q in %q", leaked, msg)
		}
	}
	if safeURL := dialErr.SafeURL(); safeURL == "" || strings.Contains(safeURL, "secret-key") {
		t.Fatalf("DialError.SafeURL() = %q, want redacted diagnostic URL", safeURL)
	}
}

func TestDialManagedAppliesDefaultAndOverrideHandshakeTimeout(t *testing.T) {
	tests := []struct {
		name string
		opt  DialOption
		want time.Duration
	}{
		{name: "default", want: defaultDialHandshakeTimeout},
		{name: "override", opt: WithHandshakeTimeout(1200 * time.Millisecond), want: 1200 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got time.Duration
			start := time.Now()
			opts := []DialOption{
				WithDialSecurityPolicy(DialSecurityPolicy{HostFilter: func(string, []net.IP) bool { return true }}),
				WithNetDialContext(func(ctx context.Context, _, _ string) (net.Conn, error) {
					deadline, ok := ctx.Deadline()
					if !ok {
						t.Fatalf("net dial context has no handshake deadline")
					}
					got = deadline.Sub(start)
					return nil, errors.New("dial attempted")
				}),
			}
			if tt.opt != nil {
				opts = append(opts, tt.opt)
			}

			_, err := DialManaged(context.Background(), "wss://127.0.0.1/realtime", nil, Config{}, opts...)
			if err == nil {
				t.Fatalf("DialManaged err=nil, want dial failure")
			}
			if got < tt.want-250*time.Millisecond || got > tt.want+250*time.Millisecond {
				t.Fatalf("handshake timeout deadline=%s, want about %s", got, tt.want)
			}
		})
	}
}

func TestDialErrorPreservesBodyReadError(t *testing.T) {
	readErr := errors.New("body read failed")
	err := newDialError("wss://upstream.example.test/realtime", &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{"X-Test": []string{"present"}},
		Body:       failingReadCloser{err: readErr},
	}, errors.New("bad gateway"), DialSecurityPolicy{})

	var dialErr *DialError
	if !errors.As(err, &dialErr) {
		t.Fatalf("err=%T %v, want *DialError", err, err)
	}
	if !errors.Is(dialErr.BodyReadErr, readErr) {
		t.Fatalf("BodyReadErr=%v, want %v", dialErr.BodyReadErr, readErr)
	}
	if dialErr.StatusCode != http.StatusBadGateway || dialErr.Header.Get("X-Test") != "present" || dialErr.CloseInfo.Kind != CloseKindDialFailed {
		t.Fatalf("diagnostics lost on body read error: %+v", dialErr)
	}
	if len(dialErr.BodySnippet) != 0 || dialErr.BodyTruncated {
		t.Fatalf("snippet=%q truncated=%v, want no body snippet on read error", dialErr.BodySnippet, dialErr.BodyTruncated)
	}
}

func TestDialManagedTLSVerificationDefaultsOn(t *testing.T) {
	accepted := make(chan *ManagedConn, 1)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := AcceptManaged(w, r, Config{}, AcceptOptions{
			CheckOrigin: func(*http.Request) bool { return true },
		})
		if err != nil {
			return
		}
		accepted <- conn
	}))
	defer ts.Close()

	rawURL := "wss" + strings.TrimPrefix(ts.URL, "https")
	_, err := DialManaged(context.Background(), rawURL, nil, Config{},
		WithDialSecurityPolicy(DialSecurityPolicy{HostFilter: func(string, []net.IP) bool { return true }}),
	)
	if err == nil {
		t.Fatalf("DialManaged default TLS err=nil, want self-signed certificate failure")
	}

	conn, err := DialManaged(context.Background(), rawURL, nil, Config{},
		WithTLSConfig(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // test-only explicit insecure TLS opt-in
		WithDialSecurityPolicy(DialSecurityPolicy{HostFilter: func(string, []net.IP) bool { return true }}),
	)
	if err != nil {
		t.Fatalf("DialManaged with explicit InsecureSkipVerify: %v", err)
	}
	defer conn.Close(CloseInfo{Kind: CloseKindAbort})
	select {
	case serverConn := <-accepted:
		serverConn.Close(CloseInfo{Kind: CloseKindAbort})
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for accepted TLS websocket")
	}
}

func TestProxyURLFailClosed(t *testing.T) {
	var dialed bool
	_, err := DialManaged(context.Background(), "wss://example.com/ws", nil, Config{},
		WithProxyURL(""),
		WithNetDialContext(func(context.Context, string, string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("direct dial attempted")
		}),
		WithDialSecurityPolicy(DialSecurityPolicy{HostFilter: func(string, []net.IP) bool { return true }}),
	)
	if err == nil || !dialed || errors.Is(err, ErrInvalidProxyURL) {
		t.Fatalf("empty proxy err=%v dialed=%v, want direct dial without ErrInvalidProxyURL", err, dialed)
	}

	dialed = false
	_, err = DialManaged(context.Background(), "wss://example.com/ws", nil, Config{},
		WithProxyURL("not a url"),
		WithNetDialContext(func(context.Context, string, string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("unexpected direct dial")
		}),
		WithDialSecurityPolicy(DialSecurityPolicy{HostFilter: func(string, []net.IP) bool { return true }}),
	)
	if !errors.Is(err, ErrInvalidProxyURL) {
		t.Fatalf("err=%v, want ErrInvalidProxyURL", err)
	}
	if dialed {
		t.Fatalf("invalid proxy attempted direct dial")
	}

	dialed = false
	_, err = DialManaged(context.Background(), "wss://example.com/ws", nil, Config{},
		WithProxyURL("ftp://proxy.invalid"),
		WithNetDialContext(func(context.Context, string, string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("unexpected direct dial")
		}),
		WithDialSecurityPolicy(DialSecurityPolicy{HostFilter: func(string, []net.IP) bool { return true }}),
	)
	if !errors.Is(err, ErrInvalidProxyURL) {
		t.Fatalf("err=%v, want ErrInvalidProxyURL", err)
	}
	if dialed {
		t.Fatalf("proxy parse failure attempted direct dial")
	}
}

func TestProxyURLAcceptsSocks5H(t *testing.T) {
	var dialer websocket.Dialer
	if err := applyProxy(&dialer, "socks5h://127.0.0.1:1080", nil); err != nil {
		t.Fatalf("applyProxy socks5h err=%v, want nil", err)
	}
	if dialer.NetDialContext == nil {
		t.Fatal("expected socks5h proxy to install NetDialContext")
	}
}

type failingReadCloser struct {
	err error
}

func (r failingReadCloser) Read([]byte) (int, error) {
	return 0, r.err
}

func (r failingReadCloser) Close() error {
	return nil
}

var _ io.ReadCloser = failingReadCloser{}
