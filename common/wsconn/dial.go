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
	ErrInvalidDialURL     = errors.New("wsconn: invalid dial url")
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
	// URL is safe for diagnostics: userinfo, query, and fragment are removed.
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
	safeURL := e.SafeURL()
	if e.StatusCode > 0 {
		return fmt.Sprintf("wsconn: dial %s failed with status %d: %v", safeURL, e.StatusCode, e.Err)
	}
	return fmt.Sprintf("wsconn: dial %s failed: %v", safeURL, e.Err)
}

func (e *DialError) SafeURL() string {
	if e == nil {
		return ""
	}
	return redactedURLForError(e.URL)
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
	handshakeTimeout := normalizeDialHandshakeTimeout(dc.handshakeTimeout)
	netDial := dc.netDialContext
	if netDial == nil {
		d := &net.Dialer{Timeout: handshakeTimeout, KeepAlive: 30 * time.Second}
		netDial = d.DialContext
	}
	var resolved resolvedDialTarget
	if dc.proxyURL == "" {
		validationCtx, cancelValidation := dialValidationContext(ctx, handshakeTimeout)
		defer cancelValidation()
		var err error
		resolved, err = validateDialTarget(validationCtx, rawURL, dc.security)
		if err != nil {
			return nil, err
		}
		netDial = pinnedNetDialContext(netDial, resolved)
	} else if err := validateProxiedDialTarget(rawURL, dc.security); err != nil {
		return nil, err
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: handshakeTimeout,
		Subprotocols:     append([]string(nil), dc.subprotocols...),
		TLSClientConfig:  dc.tlsConfig,
		NetDialContext:   netDial,
	}
	if dc.proxyURL != "" {
		if err := applyProxy(&dialer, dc.proxyURL); err != nil {
			return nil, err
		}
	}
	conn, resp, err := dialer.DialContext(ctx, rawURL, headerClone(header))
	if err != nil {
		return nil, newDialError(rawURL, resp, err, dc.security)
	}
	if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, newDialError(rawURL, resp, errors.New("unexpected websocket status"), dc.security)
	}
	return newManagedConn(conn, cfg), nil
}

type resolvedDialTarget struct {
	host string
	ips  []net.IP
}

func validateDialTarget(ctx context.Context, rawURL string, policy DialSecurityPolicy) (resolvedDialTarget, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return resolvedDialTarget{}, fmt.Errorf("%w: %s", ErrInvalidDialURL, redactedURLForError(rawURL))
	}
	switch strings.ToLower(u.Scheme) {
	case "wss":
	case "ws":
		if !policy.AllowInsecureWS {
			return resolvedDialTarget{}, fmt.Errorf("%w: %s", ErrInsecureScheme, redactedURLForError(rawURL))
		}
	default:
		return resolvedDialTarget{}, fmt.Errorf("%w: %s", ErrInsecureScheme, redactedURLForError(rawURL))
	}
	host := u.Hostname()
	if host == "" {
		return resolvedDialTarget{}, fmt.Errorf("%w: missing host", ErrInvalidDialURL)
	}
	ips, err := resolveDialHost(ctx, host)
	if err != nil {
		return resolvedDialTarget{}, fmt.Errorf("%w: %v", ErrPrivateAddrBlocked, err)
	}
	if containsMetadataIP(host, ips) {
		return resolvedDialTarget{}, fmt.Errorf("%w: %s", ErrPrivateAddrBlocked, host)
	}
	if policy.HostFilter != nil {
		if !policy.HostFilter(host, ips) {
			return resolvedDialTarget{}, fmt.Errorf("%w: %s", ErrPrivateAddrBlocked, host)
		}
		return resolvedDialTarget{host: host, ips: ips}, nil
	}
	for _, ip := range ips {
		if isBlockedIP(ip, policy.AllowPrivateIP) {
			return resolvedDialTarget{}, fmt.Errorf("%w: %s", ErrPrivateAddrBlocked, ip.String())
		}
	}
	return resolvedDialTarget{host: host, ips: ips}, nil
}

func validateProxiedDialTarget(rawURL string, policy DialSecurityPolicy) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidDialURL, redactedURLForError(rawURL))
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
		return fmt.Errorf("%w: missing host", ErrInvalidDialURL)
	}
	if isMetadataIP(net.ParseIP(host)) {
		return fmt.Errorf("%w: %s", ErrPrivateAddrBlocked, host)
	}
	return nil
}

func dialValidationContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		timeout = defaultDialHandshakeTimeout
	}
	deadline := time.Now().Add(timeout)
	if parentDeadline, ok := parent.Deadline(); ok && parentDeadline.Before(deadline) {
		return parent, func() {}
	}
	return context.WithDeadline(parent, deadline)
}

func resolveDialHost(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{normalizeIP(ip)}, nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, net.ErrClosed
	}
	for i := range ips {
		ips[i] = normalizeIP(ips[i])
	}
	return ips, nil
}

func pinnedNetDialContext(base func(context.Context, string, string) (net.Conn, error), resolved resolvedDialTarget) func(context.Context, string, string) (net.Conn, error) {
	if base == nil || resolved.host == "" || len(resolved.ips) == 0 {
		return base
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil || !sameDialHost(host, resolved.host) {
			return base(ctx, network, addr)
		}
		var lastErr error
		for _, ip := range resolved.ips {
			conn, err := base(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if ctx != nil && ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, lastErr
	}
}

func sameDialHost(a, b string) bool {
	return strings.TrimSuffix(strings.ToLower(a), ".") == strings.TrimSuffix(strings.ToLower(b), ".")
}

func isBlockedIP(ip net.IP, allowPrivate bool) bool {
	if ip == nil {
		return false
	}
	ip = normalizeIP(ip)
	if isMetadataIP(ip) {
		return true
	}
	if allowPrivate {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.IsPrivate() {
		return true
	}
	return false
}

func containsMetadataIP(host string, ips []net.IP) bool {
	if isMetadataIP(net.ParseIP(host)) {
		return true
	}
	for _, ip := range ips {
		if isMetadataIP(ip) {
			return true
		}
	}
	return false
}

func isMetadataIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	ip = normalizeIP(ip)
	return ip.Equal(net.ParseIP("169.254.169.254")) ||
		ip.Equal(net.ParseIP("100.100.100.200")) ||
		ip.Equal(net.ParseIP("fd00:ec2::254"))
}

func normalizeIP(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
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

func applyProxy(dialer *websocket.Dialer, rawURL string) error {
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
	return nil
}

func newDialError(rawURL string, resp *http.Response, err error, policy DialSecurityPolicy) error {
	e := &DialError{
		URL:       redactedURLForError(rawURL),
		Err:       err,
		CloseInfo: CloseInfo{Kind: CloseKindDialFailed, Reason: "dial_failed", Err: err, At: time.Now()},
	}
	if resp == nil {
		return e
	}
	limit := policy.MaxBodySnippet
	if limit <= 0 {
		limit = 4 << 10
	}
	e.StatusCode = resp.StatusCode
	e.Header = redactHeaders(resp.Header.Clone(), policy)
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

func headerClone(header http.Header) http.Header {
	if header == nil {
		return nil
	}
	return header.Clone()
}

func redactHeaders(header http.Header, policy DialSecurityPolicy) http.Header {
	names := policy.RedactHeaders
	if len(names) == 0 {
		names = []string{"Authorization", "Cookie", "Set-Cookie", "Sec-WebSocket-Protocol"}
	}
	for _, name := range names {
		if _, ok := header[http.CanonicalHeaderKey(name)]; ok {
			header[http.CanonicalHeaderKey(name)] = []string{"[REDACTED]"}
		}
	}
	return header
}
