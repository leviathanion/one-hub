package requester

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func setupEgressPoolClient(t *testing.T, base *http.Transport, capacity int) *providerHTTPEgressPool {
	t.Helper()
	originalClient, originalTransports := HTTPClient, defaultProviderHTTPTransports
	transports := newProviderHTTPTransportSet(base)
	transports.egresses.capacity = capacity
	defaultProviderHTTPTransports = transports
	HTTPClient = &http.Client{Transport: base}
	t.Cleanup(func() {
		transports.egresses.closeIdleConnections()
		base.CloseIdleConnections()
		HTTPClient, defaultProviderHTTPTransports = originalClient, originalTransports
	})
	return transports.egresses
}

func sendEgressTestRequest(t *testing.T, proxy, target string) (*http.Response, error) {
	t.Helper()
	r := NewHTTPRequester(proxy, nil)
	req, err := r.NewRequest(http.MethodGet, target)
	if err != nil {
		return nil, err
	}
	return r.Do(req)
}

func egressTestReferences(p *providerHTTPEgressPool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	var n int
	for _, entry := range p.entries {
		n += entry.inFlight
	}
	return n
}

func waitEgressReleased(t *testing.T, p *providerHTTPEgressPool) {
	t.Helper()
	deadline := time.After(time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for egressTestReferences(p) != 0 {
		select {
		case <-deadline:
			t.Fatal("transport reference did not release")
		case <-tick.C:
		}
	}
}

func TestEgressPoolPinsStreamingBodyDuringEviction(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("h2=%t", h2), func(t *testing.T) {
			tail := make(chan struct{})
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/probe" {
					_, _ = io.WriteString(w, "probe complete")
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: first\n")
				w.(http.Flusher).Flush()
				select {
				case <-tail:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, "data: response.completed\n\ndata: retained tail\n\n")
			}))
			server.EnableHTTP2 = h2
			server.StartTLS()
			defer server.Close()
			pool := setupEgressPoolClient(t, server.Client().Transport.(*http.Transport).Clone(), 2)
			resp, err := sendEgressTestRequest(t, "", server.URL+"/stream")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if (resp.ProtoMajor == 2) != h2 {
				t.Fatalf("protocol=%s", resp.Proto)
			}
			reader := bufio.NewReader(resp.Body)
			if line, err := reader.ReadString('\n'); err != nil || line != "data: first\n" {
				t.Fatalf("first event=%q, %v", line, err)
			}
			var exits sync.Map
			for i := 0; i < 8; i++ {
				proxy := newEgressProxy(t, "http", fmt.Sprintf("proxy-%d", i), &exits, false)
				probe, err := sendEgressTestRequest(t, proxy.URL(), server.URL+"/probe")
				if err != nil {
					t.Fatal(err)
				}
				_, readErr := io.Copy(io.Discard, probe.Body)
				closeErr := probe.Body.Close()
				if readErr != nil || closeErr != nil {
					t.Fatalf("probe body: read=%v close=%v", readErr, closeErr)
				}
			}
			// Idle cleanup, including shutdown cleanup, cannot truncate the stream.
			pool.closeIdleConnections()
			if len(pool.entries) != 2 || egressTestReferences(pool) != 1 {
				t.Fatal("active stream was evicted or capacity exceeded")
			}
			close(tail)
			body, err := io.ReadAll(reader)
			if err != nil || !strings.HasSuffix(string(body), "data: retained tail\n\n") {
				t.Fatalf("tail lost: %q %v", body, err)
			}
			if egressTestReferences(pool) != 1 {
				t.Fatal("reading to EOF released before Body.Close")
			}
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); _ = resp.Body.Close() }()
			}
			wg.Wait()
			if egressTestReferences(pool) != 0 {
				t.Fatal("duplicate Close leaked or double-released")
			}
		})
	}
}

type egressCloseProbe struct {
	io.Reader
	closed atomic.Bool
}

func (b *egressCloseProbe) Close() error { b.closed.Store(true); return nil }

func TestEgressPoolCapacityFailsLocallyBeforeDial(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	base := newProviderHTTPTransport()
	var dials atomic.Int32
	baseDial := base.DialContext
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		return baseDial(ctx, network, address)
	}
	pool := setupEgressPoolClient(t, base, 1)
	held, err := sendEgressTestRequest(t, "", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Body.Close()
	before := dials.Load()
	r := NewHTTPRequester("http://proxy.test:80", nil)
	body := &egressCloseProbe{Reader: strings.NewReader("work")}
	req, _ := http.NewRequest(http.MethodPost, server.URL, body)
	resp, apiErr := r.SendRequestRaw(req)
	if resp != nil || apiErr == nil || !apiErr.LocalError || !apiErr.UpstreamNotAttempted || apiErr.UpstreamAmbiguous || apiErr.StatusCode != 503 || apiErr.Code != "provider_transport_capacity" {
		t.Fatalf("capacity rejection=%v %+v", resp, apiErr)
	}
	if dials.Load() != before || !body.closed.Load() {
		t.Fatal("capacity failure dialed or leaked request body")
	}
	if len(pool.entries) != 1 || egressTestReferences(pool) != 1 {
		t.Fatal("capacity failure changed live pool budget")
	}
}

func TestEgressCancellationAndDialFailureReleaseReferences(t *testing.T) {
	for _, mode := range []string{"cancel", "client-timeout", "client-timeout-no-read", "stream-idle"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			pool := setupEgressPoolClient(t, newProviderHTTPTransport(), 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "client-timeout" || mode == "client-timeout-no-read" {
				HTTPClient.Timeout = 50 * time.Millisecond
			}
			r := NewHTTPRequester("", nil)
			if mode == "stream-idle" {
				r = r.ForHTTPProfile(HTTPProfileLongStream)
			}
			req, _ := r.NewRequest(http.MethodGet, server.URL, r.WithContext(ctx))
			resp, err := r.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if mode == "stream-idle" {
				// Keep the production profile and body stack; shorten only this
				// returned body's idle timer before its first read.
				leased, ok := resp.Body.(*egressResponseBody)
				if !ok {
					t.Fatalf("leased body=%T", resp.Body)
				}
				stream, ok := leased.ReadCloser.(*policyResponseBody)
				if !ok {
					t.Fatalf("stream body=%T", leased.ReadCloser)
				}
				stream.idle = 20 * time.Millisecond
			}
			if egressTestReferences(pool) != 1 {
				t.Fatal("headers released active reference")
			}
			if mode == "cancel" {
				cancel()
			} else if mode != "client-timeout-no-read" {
				if _, err := resp.Body.Read(make([]byte, 1)); err == nil {
					t.Fatal("body read did not time out")
				}
			}
			waitEgressReleased(t, pool)
		})
	}
	pool := setupEgressPoolClient(t, &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("dial failed") }}, 1)
	if _, err := sendEgressTestRequest(t, "", "http://upstream.test"); err == nil {
		t.Fatal("expected dial error")
	}
	if egressTestReferences(pool) != 0 {
		t.Fatal("dial error leaked reference")
	}
	if _, err := sendEgressTestRequest(t, "http://other.test:80", "http://upstream.test"); err == nil || errors.Is(err, errProviderTransportCapacity) {
		t.Fatalf("failed request prevented next exit from dialing: %v", err)
	}
	_, otherKey, _ := parseProviderProxy("http://other.test:80")
	if len(pool.entries) != 1 || pool.entries[otherKey] == nil || egressTestReferences(pool) != 0 {
		t.Fatal("failed first exit was not evictable after release")
	}
}

func TestEgressInvalidProxyFailsWithoutFallback(t *testing.T) {
	original, originalTransports := HTTPClient, defaultProviderHTTPTransports
	InitHttpClient()
	t.Cleanup(func() {
		CloseIdleConnections()
		HTTPClient, defaultProviderHTTPTransports = original, originalTransports
	})
	for _, address := range []string{"invalid", "ftp://user:secret@proxy.test", "http://user:secret@%zz"} {
		r := NewHTTPRequester(address, nil)
		body := &egressCloseProbe{Reader: strings.NewReader("work")}
		req, _ := http.NewRequest(http.MethodPost, "http://upstream.test", body)
		resp, apiErr := r.SendRequestRaw(req)
		if resp != nil || apiErr == nil || !apiErr.LocalError || !apiErr.UpstreamNotAttempted || strings.Contains(apiErr.Error(), "secret") {
			t.Fatalf("invalid proxy error=%+v", apiErr)
		}
		if !body.closed.Load() {
			t.Fatal("invalid proxy leaked request body")
		}
	}
	if len(defaultProviderHTTPTransports.egresses.entries) != 0 {
		t.Fatal("invalid proxy acquired an exit")
	}
}

func TestEgressConcurrentAdmissionKeepsProductionCapacity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	base := newProviderHTTPTransport()
	base.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		// Distinct configured proxy identities use a local HTTP proxy fixture.
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	pool := setupEgressPoolClient(t, base, maxProviderHTTPEgresses)
	responses := make(chan *http.Response, 2*maxProviderHTTPEgresses)
	var rejected atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 2*maxProviderHTTPEgresses; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := sendEgressTestRequest(t, fmt.Sprintf("http://proxy-%d.test", i), server.URL)
			if err != nil {
				if errors.Is(err, errProviderTransportCapacity) {
					rejected.Add(1)
				} else {
					t.Error(err)
				}
				return
			}
			responses <- resp
		}()
	}
	wg.Wait()
	close(responses)
	defer func() {
		for resp := range responses {
			_ = resp.Body.Close()
		}
	}()
	if len(pool.entries) != maxProviderHTTPEgresses || egressTestReferences(pool) != maxProviderHTTPEgresses || rejected.Load() != maxProviderHTTPEgresses {
		t.Fatalf("concurrent admission: entries=%d references=%d rejected=%d", len(pool.entries), egressTestReferences(pool), rejected.Load())
	}
	for resp := range responses {
		_ = resp.Body.Close()
	}
	waitEgressReleased(t, pool)
}

func TestEgressObserverPanicReleasesExit(t *testing.T) {
	pool := setupEgressPoolClient(t, newProviderHTTPTransport(), 1)
	r := NewHTTPRequester("", nil)
	r.ObserveRequest = func(*http.Request) { panic("observer failed before transport") }
	req, err := r.NewRequest(http.MethodGet, "http://unused-upstream.test")
	if err != nil {
		t.Fatal(err)
	}
	var caught any
	func() {
		defer func() { caught = recover() }()
		_, _ = r.Do(req)
	}()
	if caught == nil {
		t.Fatal("observer fixture did not panic")
	}
	if got := egressTestReferences(pool); got != 0 {
		t.Fatalf("recovered observer panic leaked %d egress reference(s)", got)
	}
}
