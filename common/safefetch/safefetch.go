// Package safefetch materializes untrusted remote images through a fixed,
// credential-free HTTP policy.
package safefetch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/idna"
)

const (
	maxURLBytes         = 8 << 10
	maxBodyBytes        = 10 << 20
	maxHeaderBytes      = 64 << 10
	connectTimeout      = 5 * time.Second
	tlsTimeout          = 5 * time.Second
	responseTimeout     = 10 * time.Second
	overallTimeout      = 30 * time.Second
	idleTimeout         = 30 * time.Second
	maxConnsPerHost     = 8
	maxIdleConns        = 32
	maxIdleConnsPerHost = 4
)

type lookupNetIPFunc func(context.Context, string, string) ([]netip.Addr, error)
type dialContextFunc func(context.Context, string, string) (net.Conn, error)

type requestTarget struct {
	host    string
	port    string
	allowed map[netip.Addr]struct{}
}

type requestTargetKey struct{}

type fetcher struct {
	client    *http.Client
	transport *http.Transport
	lookup    lookupNetIPFunc
}

var defaultFetcher = newFetcher(net.DefaultResolver.LookupNetIP, (&net.Dialer{
	Timeout:   connectTimeout,
	KeepAlive: idleTimeout,
}).DialContext)

// Fetch retrieves one untrusted remote image using a credential-free, read-only
// GET and the package's fixed safety policy. The standard transport may replay
// that idempotent GET only when a reused connection fails before a usable
// response is obtained; this package does not add retries. Materialization v1
// accepts only public IPv4 destinations on the scheme's default port. Fetch
// returns the normalized media type and the original image bytes.
func Fetch(ctx context.Context, rawURL string) (string, []byte, error) {
	return defaultFetcher.fetch(ctx, rawURL)
}

func newFetcher(lookup lookupNetIPFunc, dial dialContextFunc) *fetcher {
	if lookup == nil {
		lookup = net.DefaultResolver.LookupNetIP
	}
	if dial == nil {
		dial = (&net.Dialer{Timeout: connectTimeout, KeepAlive: idleTimeout}).DialContext
	}

	f := &fetcher{lookup: lookup}
	// This transport only receives credential-free GET requests. net/http may
	// internally replay such a GET when a previously idle connection is found
	// stale before a response; no broader retry contract is implemented here.
	transport := &http.Transport{
		Proxy:                  nil,
		DisableCompression:     true,
		ForceAttemptHTTP2:      false,
		TLSHandshakeTimeout:    tlsTimeout,
		ResponseHeaderTimeout:  responseTimeout,
		MaxResponseHeaderBytes: maxHeaderBytes,
		IdleConnTimeout:        idleTimeout,
		MaxConnsPerHost:        maxConnsPerHost,
		MaxIdleConns:           maxIdleConns,
		MaxIdleConnsPerHost:    maxIdleConnsPerHost,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSNextProto:           make(map[string]func(string, *tls.Conn) http.RoundTripper),
	}
	transport.DialContext = f.validatedDialer(dial)
	f.transport = transport
	f.client = &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return f
}

func (f *fetcher) fetch(ctx context.Context, rawURL string) (string, []byte, error) {
	if ctx == nil {
		return "", nil, errors.New("安全抓取 context 不能为空")
	}
	ctx, cancel := context.WithTimeout(ctx, overallTimeout)
	defer cancel()

	u, target, err := f.validateTarget(ctx, rawURL)
	if err != nil {
		return "", nil, err
	}

	ctx = context.WithValue(ctx, requestTargetKey{}, target)
	var connectionViolation atomic.Bool
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if !remoteAddrAllowed(info.Conn.RemoteAddr(), target) {
			connectionViolation.Store(true)
			_ = info.Conn.Close()
		}
	}}
	ctx = httptrace.WithClientTrace(ctx, trace)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", nil, fmt.Errorf("构造安全抓取请求失败: %w", err)
	}
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := f.client.Do(req)
	if connectionViolation.Load() {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return "", nil, errors.New("安全抓取连接的远端地址不在已验证地址集合中")
	}
	if err != nil {
		return "", nil, fmt.Errorf("安全抓取请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", nil, fmt.Errorf("安全抓取拒绝上游状态码 %d", resp.StatusCode)
	}
	if encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return "", nil, fmt.Errorf("安全抓取拒绝 Content-Encoding %q", encoding)
	}
	if resp.ContentLength > maxBodyBytes {
		return "", nil, fmt.Errorf("安全抓取响应超过 %d 字节上限", maxBodyBytes)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return "", nil, fmt.Errorf("读取安全抓取响应失败: %w", err)
	}
	if len(body) > maxBodyBytes {
		return "", nil, fmt.Errorf("安全抓取响应超过 %d 字节上限", maxBodyBytes)
	}
	mimeType, err := validatedImageMIME(resp.Header.Get("Content-Type"), body)
	if err != nil {
		return "", nil, err
	}
	return mimeType, body, nil
}

func (f *fetcher) validateTarget(ctx context.Context, rawURL string) (*url.URL, *requestTarget, error) {
	rawURL = strings.TrimSpace(rawURL)
	if len(rawURL) > maxURLBytes {
		return nil, nil, fmt.Errorf("安全抓取 URL 超过 %d 字节上限", maxURLBytes)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("安全抓取 URL 无效: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return nil, nil, errors.New("安全抓取只支持 http 和 https URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.User != nil {
		return nil, nil, errors.New("安全抓取 URL 不允许 userinfo")
	}
	originalHost := strings.TrimSuffix(u.Hostname(), ".")
	if originalHost == "" {
		return nil, nil, errors.New("安全抓取 URL 缺少 host")
	}

	if literal, parseErr := netip.ParseAddr(originalHost); parseErr == nil && literal.Is6() {
		return nil, nil, errors.New("安全抓取 v1 只支持公网 IPv4")
	}
	host, err := normalizeHost(originalHost)
	if err != nil {
		return nil, nil, err
	}
	defaultPort := "80"
	if u.Scheme == "https" {
		defaultPort = "443"
	}
	port := u.Port()
	explicitPort := port != ""
	if strings.HasSuffix(u.Host, ":") {
		return nil, nil, errors.New("安全抓取 URL 端口无效")
	}
	if port == "" {
		port = defaultPort
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 || port != defaultPort {
		return nil, nil, errors.New("安全抓取只允许协议默认端口")
	}

	ips, err := f.resolveTarget(ctx, host)
	if err != nil {
		return nil, nil, err
	}
	allowed := make(map[netip.Addr]struct{}, len(ips))
	for _, ip := range ips {
		if !ip.Is4() {
			return nil, nil, fmt.Errorf("安全抓取拒绝 IPv6 地址 %s", ip)
		}
		ip = ip.Unmap()
		if !safePublicIPv4(ip) {
			return nil, nil, fmt.Errorf("安全抓取拒绝非公网 IPv4 地址 %s", ip)
		}
		allowed[ip] = struct{}{}
	}
	if len(allowed) == 0 {
		return nil, nil, errors.New("安全抓取 host 未解析到地址")
	}

	u.Host = host
	if explicitPort {
		u.Host = net.JoinHostPort(host, port)
	}
	return u, &requestTarget{host: host, port: port, allowed: allowed}, nil
}

func normalizeHost(host string) (string, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().String(), nil
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil || ascii == "" {
		return "", errors.New("安全抓取 URL host 无效")
	}
	return strings.ToLower(ascii), nil
}

func (f *fetcher) resolveTarget(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		if !ip.Is4() {
			return nil, errors.New("安全抓取 v1 只支持公网 IPv4")
		}
		return []netip.Addr{ip}, nil
	}
	// IPv6 support requires a trusted Pref64 and network-routing contract. Until
	// that contract exists, v1 resolves only A records and rejects every IPv6 form.
	ips, err := f.lookup(ctx, "ip4", host)
	if err != nil {
		return nil, fmt.Errorf("解析安全抓取 host 失败: %w", err)
	}
	return ips, nil
}

func (f *fetcher) validatedDialer(dial dialContextFunc) dialContextFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, errors.New("安全抓取只允许 TCP 连接")
		}
		target, ok := ctx.Value(requestTargetKey{}).(*requestTarget)
		if !ok || target == nil {
			return nil, errors.New("安全抓取连接缺少已验证目标")
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil || port != target.port {
			return nil, errors.New("安全抓取连接目标与已验证目标不一致")
		}
		normalized, err := normalizeHost(strings.TrimSuffix(host, "."))
		if err != nil {
			return nil, err
		}
		if ip, parseErr := netip.ParseAddr(normalized); parseErr == nil {
			if _, ok := target.allowed[ip.Unmap()]; !ok {
				return nil, errors.New("安全抓取连接 IP 不在已验证地址集合中")
			}
		} else if normalized != target.host {
			return nil, errors.New("安全抓取连接 host 与已验证 host 不一致")
		}

		var lastErr error
		for ip := range target.allowed {
			conn, dialErr := dial(ctx, network, net.JoinHostPort(ip.String(), target.port))
			if dialErr != nil {
				lastErr = dialErr
				continue
			}
			if !remoteAddrAllowed(conn.RemoteAddr(), target) {
				_ = conn.Close()
				return nil, errors.New("安全抓取连接的远端地址不在已验证地址集合中")
			}
			return conn, nil
		}
		if lastErr == nil {
			lastErr = errors.New("没有可拨号的安全抓取地址")
		}
		return nil, lastErr
	}
}

func remoteAddrAllowed(addr net.Addr, target *requestTarget) bool {
	if addr == nil {
		return false
	}
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil || port != target.port {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	_, ok := target.allowed[ip.Unmap()]
	return ok
}

func validatedImageMIME(contentType string, body []byte) (string, error) {
	actualMIME := http.DetectContentType(body)
	if !supportedImageMIME(actualMIME) {
		return "", fmt.Errorf("安全抓取拒绝实际 MIME %q", actualMIME)
	}

	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		return actualMIME, nil
	}
	declaredMIME, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", errors.New("安全抓取响应的 Content-Type 无效")
	}
	declaredMIME = strings.ToLower(declaredMIME)
	if declaredMIME == "application/octet-stream" {
		return actualMIME, nil
	}
	if declaredMIME == "image/jpg" {
		declaredMIME = "image/jpeg"
	}
	if !supportedImageMIME(declaredMIME) {
		return "", fmt.Errorf("安全抓取拒绝声明 MIME %q", declaredMIME)
	}
	if actualMIME != declaredMIME {
		return "", fmt.Errorf("安全抓取 MIME 不匹配: 声明为 %s，实际为 %s", declaredMIME, actualMIME)
	}
	return actualMIME, nil
}

func supportedImageMIME(value string) bool {
	switch value {
	case "image/gif", "image/jpeg", "image/png", "image/webp":
		return true
	default:
		return false
	}
}

var deniedIPv4Prefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

func safePublicIPv4(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	for _, prefix := range deniedIPv4Prefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
