package requester

import (
	"crypto/tls"
	"errors"
	"net/http"
	"one-api/common/utils"
	"time"
)

var HTTPClient *http.Client

var errNoImplicitReplayTransport = errors.New("request transport cannot disable implicit replay safely")

// providerHTTPTransportSet owns the two transports used by one provider
// client configuration. The noKeepAlive sibling is created with the same
// proxy, TLS, and timeout settings as normal, but never reuses a connection.
// It is configuration owned by the requester; it is not a request or source
// replacement cache.
type providerHTTPTransportSet struct {
	normal      *http.Transport
	noKeepAlive *http.Transport
}

var defaultProviderHTTPTransports *providerHTTPTransportSet

func InitHttpClient() {
	transports := newProviderHTTPTransportSet(newProviderHTTPTransport())
	defaultProviderHTTPTransports = transports
	HTTPClient = &http.Client{
		Transport:     transports.normal,
		CheckRedirect: checkRedirectForPolicy(policyForHTTPProfile(HTTPProfileWorkAction)),
	}

	relayTimeout := utils.GetOrDefault("relay_timeout", 0)
	if relayTimeout > 0 {
		HTTPClient.Timeout = time.Duration(relayTimeout) * time.Second
	}
}

func newProviderHTTPTransportSet(normal *http.Transport) *providerHTTPTransportSet {
	if normal == nil {
		return nil
	}
	return &providerHTTPTransportSet{
		normal:      normal,
		noKeepAlive: cloneWithoutKeepAlives(normal),
	}
}

func newProviderHTTPTransport() *http.Transport {
	return &http.Transport{
		DialContext:           utils.Socks5ProxyFunc,
		Proxy:                 utils.ProxyFunc,
		DisableCompression:    true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}

func cloneWithoutKeepAlives(source *http.Transport) *http.Transport {
	if source == nil {
		return nil
	}
	clone := source.Clone()
	clone.DisableKeepAlives = true
	// Go 1.25's bundled HTTP/2 transport may retry a peer PROTOCOL_ERROR
	// after a complete NoBody request even when DisableKeepAlives is true.
	// Keep this at-most-once sibling HTTP/1-only; the normal pool remains
	// unchanged and retains its configured HTTP/2 capability.
	clone.ForceAttemptHTTP2 = false
	clone.Protocols = &http.Protocols{}
	clone.Protocols.SetHTTP1(true)
	clone.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	if clone.TLSClientConfig != nil {
		protocols := clone.TLSClientConfig.NextProtos
		filtered := make([]string, 0, len(protocols))
		hasHTTP11 := false
		for _, protocol := range protocols {
			if protocol != "h2" {
				filtered = append(filtered, protocol)
				if protocol == "http/1.1" {
					hasHTTP11 = true
				}
			}
		}
		if !hasHTTP11 {
			filtered = append(filtered, "http/1.1")
		}
		clone.TLSClientConfig.NextProtos = filtered
	}
	return clone
}

func (s *providerHTTPTransportSet) noKeepAliveFor(source http.RoundTripper) (*http.Transport, error) {
	if s == nil || s.normal == nil || s.noKeepAlive == nil {
		return nil, errNoImplicitReplayTransport
	}
	base, ok := source.(*http.Transport)
	if !ok || base != s.normal {
		return nil, errNoImplicitReplayTransport
	}
	return s.noKeepAlive, nil
}

// providerRedirectPolicy never turns one application submission into a second
// provider request. Callers surface the original 3xx response according to
// their protocol policy.
func providerRedirectPolicy(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}
