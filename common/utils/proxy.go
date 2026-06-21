package utils

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

type ContextKey string

const ProxyHTTPAddrKey ContextKey = "proxyHttpAddr"
const ProxySock5AddrKey ContextKey = "proxySock5Addr"
const ProxyAddrKey ContextKey = "proxyAddr"

func ProxyFunc(req *http.Request) (*url.URL, error) {
	proxyAddr := req.Context().Value(ProxyHTTPAddrKey)
	if proxyAddr == nil {
		return nil, nil
	}

	proxyURL, err := url.Parse(proxyAddr.(string))
	if err != nil {
		return nil, fmt.Errorf("error parsing proxy address: %w", err)
	}

	switch proxyURL.Scheme {
	case "http", "https":
		return proxyURL, nil
	}

	return nil, fmt.Errorf("unsupported proxy scheme: %s", proxyURL.Scheme)
}

func Socks5ProxyFunc(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   time.Duration(GetOrDefault("connect_timeout", 30)) * time.Second,
		KeepAlive: 30 * time.Second,
	}

	proxyAddr, ok := ctx.Value(ProxySock5AddrKey).(string)
	proxyAddr = strings.TrimSpace(proxyAddr)
	if !ok || proxyAddr == "" {
		return dialer.DialContext(ctx, network, addr)
	}

	proxyURL, err := url.Parse(proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("error parsing proxy address: %w", err)
	}

	proxyDialer, err := proxy.FromURL(proxyURL, dialer)
	if err != nil {
		return nil, fmt.Errorf("error creating proxy dialer: %w", err)
	}

	if contextDialer, ok := proxyDialer.(proxy.ContextDialer); ok {
		return contextDialer.DialContext(ctx, network, addr)
	}
	return dialProxyWithContext(ctx, proxyDialer, network, addr)
}

func dialProxyWithContext(ctx context.Context, dialer proxy.Dialer, network, addr string) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
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

func SetProxy(proxyAddr string, ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	proxyAddr = strings.TrimSpace(proxyAddr)
	if proxyAddr == "" {
		return ctx
	}
	proxyURL, err := url.Parse(proxyAddr)
	if err == nil && proxyURL != nil {
		proxyURL.Scheme = strings.ToLower(strings.TrimSpace(proxyURL.Scheme))
		if proxyURL.Scheme != "" {
			proxyAddr = proxyURL.String()
		}
	}

	key := ProxyHTTPAddrKey

	// 如果是以 socks5:// 开头的地址，那么使用 socks5 代理
	if proxyURL != nil && strings.HasPrefix(proxyURL.Scheme, "socks5") {
		key = ProxySock5AddrKey
	}

	// 否则使用 http 代理
	return context.WithValue(ctx, key, proxyAddr)
}
