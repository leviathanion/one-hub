package requester

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"one-api/types"

	"github.com/spf13/viper"
)

func stubResponsesHTTPBridgeResolver(t *testing.T, addrs []netip.Addr, err error) {
	t.Helper()
	original := upstreamResponsesHTTPResolveHost
	upstreamResponsesHTTPResolveHost = func(context.Context, string) ([]netip.Addr, error) {
		return addrs, err
	}
	t.Cleanup(func() {
		upstreamResponsesHTTPResolveHost = original
	})
}

func TestValidateAndResolveUpstreamResponsesHTTPURLBlocksPrivateResolvedHostWithProxy(t *testing.T) {
	stubResponsesHTTPBridgeResolver(t, []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil)

	_, err := ValidateAndResolveUpstreamResponsesHTTPURL(context.Background(), "https://api.example.com/v1/responses", ResponsesHTTPBridgeSecurity{
		ProxyAddr: "http://proxy.example:8080",
	})
	if err == nil || !errors.Is(err, ErrUpstreamResponsesHTTPURLHostBlocked) {
		t.Fatalf("expected proxy bridge URL resolving to private IP to be blocked, got %v", err)
	}
}

func TestValidateAndResolveUpstreamResponsesHTTPURLSelfHostedBlocksMetadataResolvedHost(t *testing.T) {
	cases := []struct {
		name      string
		rawURL    string
		resolved  []netip.Addr
		security  ResponsesHTTPBridgeSecurity
		wantHost  string
		wantProxy bool
	}{
		{
			name:     "aws metadata resolved",
			rawURL:   "http://self-hosted.example/v1/responses",
			resolved: []netip.Addr{netip.MustParseAddr("169.254.169.254")},
			security: ResponsesHTTPBridgeSecurity{
				AllowSelfHosted: true,
			},
			wantHost: "self-hosted.example",
		},
		{
			name:     "aliyun metadata resolved",
			rawURL:   "http://self-hosted.example/v1/responses",
			resolved: []netip.Addr{netip.MustParseAddr("100.100.100.200")},
			security: ResponsesHTTPBridgeSecurity{
				AllowSelfHosted: true,
			},
			wantHost: "self-hosted.example",
		},
		{
			name:     "ec2 ipv6 metadata resolved",
			rawURL:   "http://self-hosted.example/v1/responses",
			resolved: []netip.Addr{netip.MustParseAddr("fd00:ec2::254")},
			security: ResponsesHTTPBridgeSecurity{
				AllowSelfHosted: true,
			},
			wantHost: "self-hosted.example",
		},
		{
			name:     "metadata hostname resolved with proxy still checked locally",
			rawURL:   "https://metadata-alias.example/v1/responses",
			resolved: []netip.Addr{netip.MustParseAddr("169.254.169.254")},
			security: ResponsesHTTPBridgeSecurity{
				AllowSelfHosted: true,
				ProxyAddr:       "http://proxy.example:8080",
			},
			wantHost:  "metadata-alias.example",
			wantProxy: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := upstreamResponsesHTTPResolveHost
			upstreamResponsesHTTPResolveHost = func(_ context.Context, host string) ([]netip.Addr, error) {
				if host != tc.wantHost {
					t.Fatalf("expected resolver host %q, got %q", tc.wantHost, host)
				}
				if tc.wantProxy && strings.TrimSpace(tc.security.ProxyAddr) == "" {
					t.Fatal("expected proxy case to keep proxy configured")
				}
				return tc.resolved, nil
			}
			t.Cleanup(func() {
				upstreamResponsesHTTPResolveHost = original
			})

			_, err := ValidateAndResolveUpstreamResponsesHTTPURL(context.Background(), tc.rawURL, tc.security)
			if err == nil || !errors.Is(err, ErrUpstreamResponsesHTTPURLHostBlocked) {
				t.Fatalf("expected self-hosted bridge URL resolving to metadata IP to be blocked, got %v", err)
			}
		})
	}
}

func TestPinnedResponsesHTTPBridgeDialUsesResolvedIP(t *testing.T) {
	var dialed string
	expectedErr := errors.New("stop after address capture")
	dial := pinnedResponsesHTTPBridgeDialContext(func(_ context.Context, network string, addr string) (net.Conn, error) {
		dialed = addr
		return nil, expectedErr
	}, ResolvedUpstreamResponsesHTTPURL{
		Host:         "api.example.com",
		OriginalHost: "api.example.com",
		IPs:          []netip.Addr{netip.MustParseAddr("203.0.113.10")},
	})

	_, err := dial(context.Background(), "tcp", "api.example.com:443")
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected base dial error, got %v", err)
	}
	if dialed != "203.0.113.10:443" {
		t.Fatalf("expected dial to pin resolved IP, got %q", dialed)
	}
}

func TestResponsesHTTPBridgeRejectsHTTPProxyBecauseTargetCannotBePinned(t *testing.T) {
	_, _, err := responsesHTTPBridgeClient(ResolvedUpstreamResponsesHTTPURL{
		Host:         "api.example.com",
		OriginalHost: "api.example.com",
		IPs:          []netip.Addr{netip.MustParseAddr("203.0.113.10")},
	}, "http://proxy.example:8080")
	if !errors.Is(err, errResponsesHTTPBridgeHTTPProxyUnsupported) {
		t.Fatalf("expected HTTP proxy to be rejected for bridge pinning, got %v", err)
	}
}

func TestResponsesHTTPBridgeClientTimeoutUsesRelayTimeoutWhenGlobalClientMissing(t *testing.T) {
	originalHTTPClient := HTTPClient
	originalRelayTimeout := viper.Get("relay_timeout")
	HTTPClient = nil
	t.Cleanup(func() {
		HTTPClient = originalHTTPClient
		viper.Set("relay_timeout", originalRelayTimeout)
	})
	viper.Set("relay_timeout", 7)

	client, transport, err := responsesHTTPBridgeClient(ResolvedUpstreamResponsesHTTPURL{
		Host:         "api.example.com",
		OriginalHost: "api.example.com",
		IPs:          []netip.Addr{netip.MustParseAddr("203.0.113.10")},
	}, "")
	if err != nil {
		t.Fatalf("build bridge client: %v", err)
	}
	if transport != nil {
		transport.CloseIdleConnections()
	}
	if client.Timeout != 7*time.Second {
		t.Fatalf("expected relay_timeout fallback, got %v", client.Timeout)
	}
}

func TestResponsesHTTPBridgeSocksProxyUsesResolvedIPTarget(t *testing.T) {
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake socks proxy: %v", err)
	}
	defer proxyListener.Close()

	targetCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, acceptErr := proxyListener.Accept()
		if acceptErr != nil {
			errCh <- acceptErr
			return
		}
		defer conn.Close()
		greeting := make([]byte, 3)
		if _, readErr := io.ReadFull(conn, greeting); readErr != nil {
			errCh <- readErr
			return
		}
		if _, writeErr := conn.Write([]byte{0x05, 0x00}); writeErr != nil {
			errCh <- writeErr
			return
		}
		header := make([]byte, 4)
		if _, readErr := io.ReadFull(conn, header); readErr != nil {
			errCh <- readErr
			return
		}
		if header[0] != 0x05 || header[1] != 0x01 || header[3] != 0x01 {
			errCh <- errors.New("expected SOCKS5 connect to IPv4 target")
			return
		}
		addr := make([]byte, 4)
		if _, readErr := io.ReadFull(conn, addr); readErr != nil {
			errCh <- readErr
			return
		}
		port := make([]byte, 2)
		if _, readErr := io.ReadFull(conn, port); readErr != nil {
			errCh <- readErr
			return
		}
		targetCh <- net.IP(addr).String()
		_, _ = conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	}()

	_, transport, err := responsesHTTPBridgeClient(ResolvedUpstreamResponsesHTTPURL{
		Host:         "api.example.com",
		OriginalHost: "api.example.com",
		IPs:          []netip.Addr{netip.MustParseAddr("203.0.113.10")},
	}, "socks5://"+proxyListener.Addr().String())
	if err != nil {
		t.Fatalf("build socks bridge transport: %v", err)
	}
	conn, err := transport.DialContext(context.Background(), "tcp", "api.example.com:443")
	if conn != nil {
		_ = conn.Close()
	}
	if err != nil {
		t.Fatalf("dial through fake socks proxy: %v", err)
	}
	select {
	case target := <-targetCh:
		if target != "203.0.113.10" {
			t.Fatalf("expected SOCKS proxy target to be pinned IP, got %q", target)
		}
	case err := <-errCh:
		t.Fatalf("fake socks proxy failed: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for fake socks proxy target")
	}
}

func TestValidateAndResolveUpstreamResponsesHTTPURLReportsResolveFailure(t *testing.T) {
	stubResponsesHTTPBridgeResolver(t, nil, errors.New("dns unavailable"))

	_, err := ValidateAndResolveUpstreamResponsesHTTPURL(context.Background(), "https://api.example.com/v1/responses", ResponsesHTTPBridgeSecurity{})
	if err == nil || !strings.Contains(err.Error(), ErrUpstreamResponsesHTTPURLResolveFailed.Error()) {
		t.Fatalf("expected resolve failure, got %v", err)
	}
}

func TestSendResponsesHTTPBridgeRawClosesPrivateTransportOnFailureStatus(t *testing.T) {
	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	server.Start()
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	ip, err := netip.ParseAddr(req.URL.Hostname())
	if err != nil {
		t.Fatalf("parse test server host %q: %v", req.URL.Hostname(), err)
	}
	stubResponsesHTTPBridgeResolver(t, []netip.Addr{ip}, nil)
	requester := NewHTTPRequester("", func(*http.Response) *types.OpenAIError {
		return &types.OpenAIError{Message: "rate limited"}
	})

	_, errWithCode := requester.SendResponsesHTTPBridgeRaw(req, ResponsesHTTPBridgeSecurity{AllowSelfHosted: true})
	if errWithCode == nil || errWithCode.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected upstream failure response, got %+v", errWithCode)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("expected failure response path to close the private bridge transport connection")
	}
}

func TestSendResponsesHTTPBridgeRawRejectsRedirectsWithoutFollowing(t *testing.T) {
	redirectHit := make(chan struct{}, 1)
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectHit <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer redirectTarget.Close()

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", redirectTarget.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	req, err := http.NewRequest(http.MethodPost, source.URL, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	ip, err := netip.ParseAddr(req.URL.Hostname())
	if err != nil {
		t.Fatalf("parse test server host %q: %v", req.URL.Hostname(), err)
	}
	stubResponsesHTTPBridgeResolver(t, []netip.Addr{ip}, nil)

	requester := NewHTTPRequester("", nil)
	_, errWithCode := requester.SendResponsesHTTPBridgeRaw(req, ResponsesHTTPBridgeSecurity{AllowSelfHosted: true})
	if errWithCode == nil || errWithCode.Code != "ws_request_failed" || !strings.Contains(errWithCode.Message, errResponsesHTTPBridgeRedirectUnsupported.Error()) {
		t.Fatalf("expected bridge redirect to fail closed with ws_request_failed, got %+v", errWithCode)
	}
	select {
	case <-redirectHit:
		t.Fatal("bridge followed redirect target")
	default:
	}
}

func TestSendResponsesHTTPBridgeRawUppercaseSocksProxyDoesNotBypassProxy(t *testing.T) {
	targetHit := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHit <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake socks proxy: %v", err)
	}
	defer proxyListener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := proxyListener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	ip, err := netip.ParseAddr(req.URL.Hostname())
	if err != nil {
		t.Fatalf("parse test server host %q: %v", req.URL.Hostname(), err)
	}
	stubResponsesHTTPBridgeResolver(t, []netip.Addr{ip}, nil)

	requester := NewHTTPRequester("", nil)
	_, errWithCode := requester.SendResponsesHTTPBridgeRaw(req, ResponsesHTTPBridgeSecurity{
		AllowSelfHosted: true,
		ProxyAddr:       " SOCKS5://" + proxyListener.Addr().String(),
	})
	if errWithCode == nil {
		t.Fatal("expected stalled socks proxy handshake to fail")
	}
	select {
	case conn := <-accepted:
		_ = conn.Close()
	case <-time.After(time.Second):
		t.Fatal("expected bridge to connect to configured socks proxy")
	}
	select {
	case <-targetHit:
		t.Fatal("bridge bypassed socks proxy and connected directly to target")
	default:
	}
}
