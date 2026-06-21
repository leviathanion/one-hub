package requester

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"one-api/common"
	"one-api/common/utils"
	"one-api/types"

	"golang.org/x/net/proxy"
)

type ResponsesHTTPBridgeSecurity struct {
	AllowSelfHosted bool
	ProxyAddr       string
}

type ResolvedUpstreamResponsesHTTPURL struct {
	URL          string
	Host         string
	OriginalHost string
	IPs          []netip.Addr
}

var upstreamResponsesHTTPResolveHost = defaultUpstreamResponsesHTTPResolveHost

var errResponsesHTTPBridgeRedirectUnsupported = errors.New("responses_ws_http_bridge_redirect_unsupported")
var errResponsesHTTPBridgeHTTPProxyUnsupported = errors.New("responses_ws_http_bridge_http_proxy_unsupported")

func ValidateAndResolveUpstreamResponsesHTTPURL(ctx context.Context, rawURL string, security ResponsesHTTPBridgeSecurity) (ResolvedUpstreamResponsesHTTPURL, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed == nil || parsed.Hostname() == "" {
		return ResolvedUpstreamResponsesHTTPURL{}, upstreamResponsesHTTPURLValidationError(ErrUpstreamResponsesHTTPURLInvalid, http.StatusInternalServerError)
	}

	switch strings.ToLower(parsed.Scheme) {
	case "https":
		parsed.Scheme = "https"
	case "http":
		if !security.AllowSelfHosted {
			return ResolvedUpstreamResponsesHTTPURL{}, upstreamResponsesHTTPURLValidationError(ErrUpstreamResponsesHTTPURLRequiresHTTPS, http.StatusBadRequest)
		}
		parsed.Scheme = "http"
	default:
		return ResolvedUpstreamResponsesHTTPURL{}, upstreamResponsesHTTPURLValidationError(ErrUpstreamResponsesHTTPURLUnsupported, http.StatusInternalServerError)
	}

	originalHost := parsed.Hostname()
	policyHost, err := normalizeUpstreamRealtimePolicyHost(originalHost)
	if err != nil {
		return ResolvedUpstreamResponsesHTTPURL{}, upstreamResponsesHTTPURLValidationError(ErrUpstreamResponsesHTTPURLInvalid, http.StatusBadRequest)
	}
	if UpstreamRealtimeMetadataHostBlocked(policyHost) {
		return ResolvedUpstreamResponsesHTTPURL{}, upstreamResponsesHTTPURLValidationError(ErrUpstreamResponsesHTTPURLHostBlocked, http.StatusBadRequest)
	}
	if !security.AllowSelfHosted && UpstreamRealtimeHostBlocked(policyHost) {
		return ResolvedUpstreamResponsesHTTPURL{}, upstreamResponsesHTTPURLValidationError(ErrUpstreamResponsesHTTPURLHostBlocked, http.StatusBadRequest)
	}

	resolveCtx := ctx
	if resolveCtx == nil {
		resolveCtx = context.Background()
	}
	resolveCtx, cancel := context.WithTimeout(resolveCtx, upstreamRealtimeURLLookupTimeout)
	defer cancel()
	ips, err := upstreamResponsesHTTPResolveHost(resolveCtx, policyHost)
	if err != nil {
		return ResolvedUpstreamResponsesHTTPURL{}, upstreamResponsesHTTPURLValidationError(ErrUpstreamResponsesHTTPURLResolveFailed, http.StatusBadRequest)
	}
	if len(ips) == 0 {
		return ResolvedUpstreamResponsesHTTPURL{}, upstreamResponsesHTTPURLValidationError(ErrUpstreamResponsesHTTPURLResolveFailed, http.StatusBadRequest)
	}
	for _, ip := range ips {
		if security.AllowSelfHosted {
			if UpstreamRealtimeMetadataIPBlocked(ip) {
				return ResolvedUpstreamResponsesHTTPURL{}, upstreamResponsesHTTPURLValidationError(ErrUpstreamResponsesHTTPURLHostBlocked, http.StatusBadRequest)
			}
			continue
		}
		if UpstreamRealtimeIPBlocked(ip) {
			return ResolvedUpstreamResponsesHTTPURL{}, upstreamResponsesHTTPURLValidationError(ErrUpstreamResponsesHTTPURLHostBlocked, http.StatusBadRequest)
		}
	}

	return ResolvedUpstreamResponsesHTTPURL{
		URL:          parsed.String(),
		Host:         policyHost,
		OriginalHost: originalHost,
		IPs:          append([]netip.Addr(nil), ips...),
	}, nil
}

func (r *HTTPRequester) SendResponsesHTTPBridgeRaw(req *http.Request, security ResponsesHTTPBridgeSecurity) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	if req == nil || req.URL == nil {
		return nil, common.StringErrorWrapperLocal(ErrUpstreamResponsesHTTPURLInvalid.Error(), "ws_request_failed", http.StatusInternalServerError)
	}
	if r != nil && strings.TrimSpace(security.ProxyAddr) == "" {
		security.ProxyAddr = r.proxyAddr
	}
	proxyAddr := strings.TrimSpace(security.ProxyAddr)
	resolved, err := ValidateAndResolveUpstreamResponsesHTTPURL(req.Context(), req.URL.String(), security)
	if err != nil {
		return nil, common.StringErrorWrapperLocal(err.Error(), "ws_request_failed", UpstreamResponsesHTTPURLStatusCode(err))
	}

	client, transport, err := responsesHTTPBridgeClient(resolved, proxyAddr)
	if err != nil {
		return nil, common.StringErrorWrapperLocal(err.Error(), "ws_request_failed", http.StatusBadRequest)
	}
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if transport != nil {
			transport.CloseIdleConnections()
		}
		if errors.Is(err, errResponsesHTTPBridgeRedirectUnsupported) {
			return nil, common.StringErrorWrapperLocal(errResponsesHTTPBridgeRedirectUnsupported.Error(), "ws_request_failed", http.StatusBadGateway)
		}
		return nil, common.StringErrorWrapperLocal(err.Error(), "http_request_failed", http.StatusInternalServerError)
	}
	if resp.Body != nil {
		// ResponsesWS bridge uses a private, pinned transport per upstream HTTP
		// stream for SSRF resistance. Binding idle cleanup to Body.Close keeps
		// that isolation without leaking the transport on success or error paths.
		resp.Body = closeIdleTransportOnBodyClose{ReadCloser: resp.Body, transport: transport}
	} else if transport != nil {
		transport.CloseIdleConnections()
	}
	if r != nil && r.IsFailureStatusCode(resp) {
		return nil, HandleErrorResp(resp, r.ErrorHandler, r.IsOpenAI)
	}
	return resp, nil
}

func responsesHTTPBridgeClient(resolved ResolvedUpstreamResponsesHTTPURL, proxyAddr string) (*http.Client, *http.Transport, error) {
	transport, err := responsesHTTPBridgeTransport(resolved, proxyAddr)
	if err != nil {
		return nil, nil, err
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// Bridge URL policy and IP pinning are scoped to the validated
			// upstream. Following redirects would need per-hop validation and
			// credential header stripping; fail closed until that complexity pays.
			return errResponsesHTTPBridgeRedirectUnsupported
		},
	}
	if HTTPClient != nil {
		client.Timeout = HTTPClient.Timeout
	}
	return client, transport, nil
}

type closeIdleTransportOnBodyClose struct {
	io.ReadCloser
	transport *http.Transport
}

func (b closeIdleTransportOnBodyClose) Close() error {
	err := b.ReadCloser.Close()
	if b.transport != nil {
		b.transport.CloseIdleConnections()
	}
	return err
}

func responsesHTTPBridgeTransport(resolved ResolvedUpstreamResponsesHTTPURL, proxyAddr string) (*http.Transport, error) {
	proxyAddr = strings.TrimSpace(proxyAddr)
	dialer := &net.Dialer{
		Timeout:   time.Duration(utils.GetOrDefault("connect_timeout", 30)) * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
	if proxyAddr == "" {
		transport.DialContext = pinnedResponsesHTTPBridgeDialContext(dialer.DialContext, resolved)
		return transport, nil
	}

	proxyURL, err := url.Parse(proxyAddr)
	if err != nil {
		return nil, err
	}
	proxyURL.Scheme = strings.ToLower(strings.TrimSpace(proxyURL.Scheme))
	switch proxyURL.Scheme {
	case "http", "https":
		// HTTP proxies resolve CONNECT/absolute-URL targets on the proxy side.
		// ResponsesWS HTTP bridge carries provider credentials, so v1 fails closed
		// instead of trusting proxy-side DNS that cannot be pinned to resolved.IPs.
		return nil, errResponsesHTTPBridgeHTTPProxyUnsupported
	case "socks5", "socks5h":
		proxyDialer, err := proxy.FromURL(proxyURL, dialer)
		if err != nil {
			return nil, err
		}
		transport.DialContext = pinnedResponsesHTTPBridgeDialContext(responsesHTTPBridgeProxyDialContext(proxyDialer), resolved)
	default:
		return nil, urlInvalidProxySchemeError(proxyURL.Scheme)
	}
	return transport, nil
}

func responsesHTTPBridgeProxyDialContext(proxyDialer proxy.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		if contextDialer, ok := proxyDialer.(proxy.ContextDialer); ok {
			return contextDialer.DialContext(ctx, network, addr)
		}
		return responsesHTTPBridgeDialProxyWithContext(ctx, proxyDialer, network, addr)
	}
}

func responsesHTTPBridgeDialProxyWithContext(ctx context.Context, dialer proxy.Dialer, network, addr string) (net.Conn, error) {
	type dialResult struct {
		conn net.Conn
		err  error
	}
	resultCh := make(chan dialResult, 1)
	go func() {
		conn, err := dialer.Dial(network, addr)
		if conn != nil && ctx.Err() != nil {
			_ = conn.Close()
		}
		resultCh <- dialResult{conn: conn, err: err}
	}()
	select {
	case result := <-resultCh:
		return result.conn, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func pinnedResponsesHTTPBridgeDialContext(base func(context.Context, string, string) (net.Conn, error), resolved ResolvedUpstreamResponsesHTTPURL) func(context.Context, string, string) (net.Conn, error) {
	if base == nil || len(resolved.IPs) == 0 {
		return base
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil || !responsesHTTPBridgeHostMatches(host, resolved) {
			return base(ctx, network, addr)
		}
		var lastErr error
		for _, ip := range resolved.IPs {
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

func responsesHTTPBridgeHostMatches(host string, resolved ResolvedUpstreamResponsesHTTPURL) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return host != "" && (host == strings.TrimSuffix(strings.ToLower(resolved.Host), ".") ||
		host == strings.TrimSuffix(strings.ToLower(resolved.OriginalHost), "."))
}

func defaultUpstreamResponsesHTTPResolveHost(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	return addrs, nil
}

func urlInvalidProxySchemeError(scheme string) error {
	return fmt.Errorf("unsupported proxy scheme: %s", scheme)
}
