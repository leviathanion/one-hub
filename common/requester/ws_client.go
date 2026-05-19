package requester

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"one-api/common/logger"
	"one-api/common/utils"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

func GetWSClient(proxyAddr string) *websocket.Dialer {
	timeout := time.Duration(utils.GetOrDefault("connect_timeout", 5)) * time.Second
	netDialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
	}
	dialer := &websocket.Dialer{
		HandshakeTimeout:  timeout,
		EnableCompression: false,
		NetDialContext:    netDialer.DialContext,
	}

	if proxyAddr != "" {
		err := setWSProxy(dialer, proxyAddr)
		if err != nil {
			logger.SysError(err.Error())
			return dialer
		}
	}

	return dialer
}

func setWSProxy(dialer *websocket.Dialer, proxyAddr string) error {
	proxyURL, err := url.Parse(proxyAddr)
	if err != nil {
		return fmt.Errorf("error parsing proxy address: %w", err)
	}

	switch proxyURL.Scheme {
	case "http", "https":
		dialer.Proxy = func(*http.Request) (*url.URL, error) {
			return proxyURL, nil
		}
	case "socks5", "socks5h":
		proxyDialer, err := proxy.FromURL(proxyURL, proxy.Direct)
		if err != nil {
			return fmt.Errorf("error creating proxy dialer: %w", err)
		}
		originalNetDialContext := dialer.NetDialContext
		dialer.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if contextDialer, ok := proxyDialer.(proxy.ContextDialer); ok {
				return contextDialer.DialContext(ctx, network, addr)
			}
			if originalNetDialContext != nil {
				return originalNetDialContext(ctx, network, addr)
			}
			return proxyDialer.Dial(network, addr)
		}
	default:
		return fmt.Errorf("unsupported proxy scheme: %s", proxyURL.Scheme)
	}

	return nil
}
