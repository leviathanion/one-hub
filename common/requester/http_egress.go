package requester

import (
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"one-api/metrics"
)

// This bounds transport configurations, not active connections or account work.
// Active response bodies pin their entry; only zero-reference entries may leave.
const maxProviderHTTPEgresses = 32

var (
	errProviderTransportCapacity = errors.New("provider HTTP egress transport capacity exhausted")
	errInvalidProviderProxy      = errors.New("invalid provider HTTP proxy configuration")
	directProviderHTTPEgressKey  = sha256.Sum256(nil)
)

type providerHTTPEgress struct {
	normal, noKeepAlive *http.Transport
	inFlight            int
	lastUsed            uint64
}

type providerHTTPEgressPool struct {
	mu       sync.Mutex
	base     *http.Transport
	capacity int
	clock    uint64
	entries  map[[sha256.Size]byte]*providerHTTPEgress
}

func newProviderHTTPEgressPool(base *http.Transport, capacity int) *providerHTTPEgressPool {
	return &providerHTTPEgressPool{base: base, capacity: capacity, entries: make(map[[sha256.Size]byte]*providerHTTPEgress)}
}

func parseProviderProxy(address string) (*url.URL, [sha256.Size]byte, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, directProviderHTTPEgressKey, nil
	}
	proxy, err := url.Parse(address)
	if err != nil || proxy.Hostname() == "" {
		return nil, [sha256.Size]byte{}, errInvalidProviderProxy
	}
	proxy.Scheme = strings.ToLower(proxy.Scheme)
	switch proxy.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, [sha256.Size]byte{}, errInvalidProviderProxy
	}
	// Include authentication and retain conservative URL distinctions. Never
	// put the URL (which can contain a password) in errors or metric labels.
	return proxy, sha256.Sum256([]byte(proxy.String())), nil
}

func (p *providerHTTPEgressPool) acquire(proxy *url.URL, key [sha256.Size]byte) (*providerHTTPEgress, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.entries[key]
	if entry == nil {
		if len(p.entries) >= p.capacity {
			var oldest *providerHTTPEgress
			var oldestKey [sha256.Size]byte
			for candidateKey, candidate := range p.entries {
				if candidate.inFlight == 0 && (oldest == nil || candidate.lastUsed < oldest.lastUsed) {
					oldest, oldestKey = candidate, candidateKey
				}
			}
			if oldest == nil {
				metrics.RecordProviderHTTPEgressCapacityExhausted()
				return nil, errProviderTransportCapacity
			}
			oldest.normal.CloseIdleConnections()
			oldest.noKeepAlive.CloseIdleConnections()
			delete(p.entries, oldestKey)
		}
		normal := p.base.Clone()
		// A transport, including its HTTP/2 pool, has exactly one fixed exit.
		// Explicit direct mode ignores proxy values inherited in request context.
		normal.Proxy = http.ProxyURL(proxy)
		entry = &providerHTTPEgress{normal: normal, noKeepAlive: cloneWithoutKeepAlives(normal)}
		p.entries[key] = entry
	}
	p.clock++
	entry.lastUsed = p.clock
	entry.inFlight++
	return entry, nil
}

func (p *providerHTTPEgressPool) release(entry *providerHTTPEgress) {
	p.mu.Lock()
	entry.inFlight--
	p.mu.Unlock()
}

func (p *providerHTTPEgressPool) closeIdleConnections() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, entry := range p.entries {
		entry.normal.CloseIdleConnections()
		entry.noKeepAlive.CloseIdleConnections()
	}
}

type egressResponseBody struct {
	io.ReadCloser
	release    func()
	stopCancel func() bool
	once       sync.Once
	err        error
}

func (b *egressResponseBody) Close() error { return b.close(true) }

func (b *egressResponseBody) close(stopObserver bool) error {
	b.once.Do(func() {
		if stopObserver && b.stopCancel != nil {
			b.stopCancel()
		}
		// Keep the reference until transport I/O has actually been closed.
		b.err = b.ReadCloser.Close()
		b.release()
	})
	return b.err
}

// CloseIdleConnections runs after the existing business shutdown drain. It
// does not create another request admission gate or cancel active business.
func CloseIdleConnections() {
	if defaultProviderHTTPTransports != nil {
		defaultProviderHTTPTransports.egresses.closeIdleConnections()
		defaultProviderHTTPTransports.normal.CloseIdleConnections()
	}
}
