package wsconn

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

var (
	ErrInsecureScheme     = errors.New("wsconn: insecure ws scheme blocked")
	ErrPrivateAddrBlocked = errors.New("wsconn: private address blocked")
	ErrInvalidProxyURL    = errors.New("wsconn: invalid proxy url")
)

const defaultDialHandshakeTimeout = 5 * time.Second

type DialSecurityPolicy struct {
	AllowInsecureWS bool
	AllowPrivateIP  bool
	MaxBodySnippet  int64
	RedactHeaders   []string
	HostFilter      func(host string, ips []net.IP) bool
}

type DialOption func(*dialConfig)

type dialConfig struct {
	proxyURL         string
	subprotocols     []string
	handshakeTimeout time.Duration
	tlsConfig        *tls.Config
	netDialContext   func(ctx context.Context, network, addr string) (net.Conn, error)
	security         DialSecurityPolicy
}

func WithProxyURL(rawURL string) DialOption {
	return func(c *dialConfig) { c.proxyURL = rawURL }
}

func WithSubprotocols(protos ...string) DialOption {
	return func(c *dialConfig) { c.subprotocols = append([]string(nil), protos...) }
}

func WithHandshakeTimeout(d time.Duration) DialOption {
	return func(c *dialConfig) { c.handshakeTimeout = d }
}

func WithTLSConfig(cfg *tls.Config) DialOption {
	return func(c *dialConfig) { c.tlsConfig = cfg }
}

func WithNetDialContext(f func(ctx context.Context, network, addr string) (net.Conn, error)) DialOption {
	return func(c *dialConfig) { c.netDialContext = f }
}

func WithDialSecurityPolicy(p DialSecurityPolicy) DialOption {
	return func(c *dialConfig) { c.security = p }
}

type DialError struct {
	URL           string
	StatusCode    int
	Header        http.Header
	BodySnippet   []byte
	BodyTruncated bool
	BodyReadErr   error
	Err           error
	CloseInfo     CloseInfo
}

func (e *DialError) Error() string {
	if e == nil {
		return ""
	}
	safeURL := redactedURLForError(e.URL)
	if e.StatusCode > 0 {
		return fmt.Sprintf("wsconn: dial %s failed with status %d: %v", safeURL, e.StatusCode, e.Err)
	}
	return fmt.Sprintf("wsconn: dial %s failed: %v", safeURL, e.Err)
}

func (e *DialError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func DialManaged(ctx context.Context, rawURL string, header http.Header, cfg Config, opts ...DialOption) (*ManagedConn, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	dc := dialConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(&dc)
		}
	}
	if err := validateDialTarget(ctx, rawURL, dc.security); err != nil {
		return nil, err
	}
	handshakeTimeout := normalizeDialHandshakeTimeout(dc.handshakeTimeout)
	netDial := dc.netDialContext
	if netDial == nil {
		d := &net.Dialer{Timeout: handshakeTimeout, KeepAlive: 30 * time.Second}
		netDial = d.DialContext
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: handshakeTimeout,
		Subprotocols:     append([]string(nil), dc.subprotocols...),
		TLSClientConfig:  dc.tlsConfig,
		NetDialContext:   netDial,
	}
	if dc.proxyURL != "" {
		if err := applyProxy(&dialer, dc.proxyURL, netDial); err != nil {
			return nil, err
		}
	}
	conn, resp, err := dialer.DialContext(ctx, rawURL, redactedHeaderClone(header, dc.security))
	if err != nil {
		return nil, newDialError(rawURL, resp, err, dc.security)
	}
	if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, newDialError(rawURL, resp, errors.New("unexpected websocket status"), dc.security)
	}
	return newManagedConn(conn, cfg), nil
}

func validateDialTarget(ctx context.Context, rawURL string, policy DialSecurityPolicy) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	switch strings.ToLower(u.Scheme) {
	case "wss":
	case "ws":
		if !policy.AllowInsecureWS {
			return fmt.Errorf("%w: %s", ErrInsecureScheme, redactedURLForError(rawURL))
		}
	default:
		return fmt.Errorf("%w: %s", ErrInsecureScheme, redactedURLForError(rawURL))
	}
	host := u.Hostname()
	if host == "" {
		return nil
	}
	ips, _ := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if policy.HostFilter != nil {
		if !policy.HostFilter(host, ips) {
			return fmt.Errorf("%w: %s", ErrPrivateAddrBlocked, host)
		}
		return nil
	}
	for _, ip := range ips {
		if isBlockedIP(ip, policy.AllowPrivateIP) {
			return fmt.Errorf("%w: %s", ErrPrivateAddrBlocked, ip.String())
		}
	}
	return nil
}

func isBlockedIP(ip net.IP, allowPrivate bool) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.Equal(net.ParseIP("169.254.169.254")) {
		return true
	}
	if ip.IsPrivate() && !allowPrivate {
		return true
	}
	return false
}

func normalizeDialHandshakeTimeout(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultDialHandshakeTimeout
	}
	return d
}

func redactedURLForError(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<redacted>"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	if u.Scheme == "" && u.Host == "" && u.Path == "" {
		return "<redacted>"
	}
	return u.String()
}

func applyProxy(dialer *websocket.Dialer, rawURL string, base func(context.Context, string, string) (net.Conn, error)) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" {
		return fmt.Errorf("%w: %v", ErrInvalidProxyURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		dialer.Proxy = func(*http.Request) (*url.URL, error) { return u, nil }
	case "socks5", "socks5h":
		proxyDialer, err := proxy.FromURL(u, proxy.Direct)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidProxyURL, err)
		}
		dialer.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if contextDialer, ok := proxyDialer.(proxy.ContextDialer); ok {
				return contextDialer.DialContext(ctx, network, addr)
			}
			return proxyDialer.Dial(network, addr)
		}
	default:
		return fmt.Errorf("%w: unsupported scheme %s", ErrInvalidProxyURL, u.Scheme)
	}
	_ = base
	return nil
}

func newDialError(rawURL string, resp *http.Response, err error, policy DialSecurityPolicy) error {
	if resp == nil {
		return err
	}
	limit := policy.MaxBodySnippet
	if limit <= 0 {
		limit = 4 << 10
	}
	e := &DialError{
		URL:        rawURL,
		StatusCode: resp.StatusCode,
		Header:     redactHeaders(resp.Header.Clone(), policy),
		Err:        err,
		CloseInfo:  CloseInfo{Kind: CloseKindDialFailed, Reason: "dial_failed", Err: err, At: time.Now()},
	}
	if resp.Body != nil {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		_ = resp.Body.Close()
		if readErr != nil {
			e.BodyReadErr = readErr
		} else if int64(len(body)) > limit {
			e.BodySnippet = append([]byte(nil), body[:int(limit)]...)
			e.BodyTruncated = true
		} else {
			e.BodySnippet = append([]byte(nil), body...)
		}
	}
	return e
}

func redactedHeaderClone(header http.Header, policy DialSecurityPolicy) http.Header {
	if header == nil {
		return nil
	}
	return header.Clone()
}

func redactHeaders(header http.Header, policy DialSecurityPolicy) http.Header {
	names := policy.RedactHeaders
	if len(names) == 0 {
		names = []string{"Authorization", "Cookie", "Sec-WebSocket-Protocol"}
	}
	for _, name := range names {
		if _, ok := header[http.CanonicalHeaderKey(name)]; ok {
			header[http.CanonicalHeaderKey(name)] = []string{"[REDACTED]"}
		}
	}
	return header
}
