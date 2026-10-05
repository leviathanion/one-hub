package requester

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"
)

// Fixed retains requester policies with one preconfigured, safe egress. It is
// a cost control, not a replacement for the bounded multi-egress Cached path.
// Allocations include the in-process TLS server and CONNECT proxy. Parallel
// ns/op measures throughput, not individual latency or supplier TTFT. The
// long_stream profile exercises flush/body timers without a simulated delay.
func BenchmarkHTTPEgressWarmReuse(b *testing.B) {
	for _, protocol := range []string{"H1", "H2"} {
		for _, route := range []string{"Direct", "CONNECT"} {
			for _, profile := range []HTTPProfile{HTTPProfileWorkAction, HTTPProfileObservationGET, HTTPProfileLongStream} {
				for _, traffic := range []string{"Serial", "Parallel"} {
					for _, mode := range []string{"Fixed", "Cached"} {
						b.Run(fmt.Sprintf("%s/%s/%s/%s/%s", protocol, route, profile, traffic, mode), func(b *testing.B) {
							benchmarkHTTPEgressWarmReuse(b, protocol == "H2", route, profile, traffic == "Parallel", mode == "Cached")
						})
					}
				}
			}
		}
	}
}

func benchmarkHTTPEgressWarmReuse(b *testing.B, h2 bool, route string, profile HTTPProfile, parallel, cached bool) {
	var accepted atomic.Int64
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		if profile == HTTPProfileLongStream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "data: completed\n\ndata: tail\n\n")
		} else {
			_, _ = io.WriteString(w, `{"ok":true,"payload":"warm TLS local fixture"}`)
		}
	}))
	up.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			accepted.Add(1)
		}
	}
	up.EnableHTTP2 = h2
	up.StartTLS()
	b.Cleanup(up.Close)
	proxyAddress := ""
	if route == "CONNECT" {
		proxyAddress = newEgressProxy(b, "http", "benchmark", &sync.Map{}, false).URL()
	}
	base := newProviderHTTPTransport()
	roots := x509.NewCertPool()
	roots.AddCert(up.Certificate())
	base.TLSClientConfig = &tls.Config{RootCAs: roots}
	oldClient, oldTransports := HTTPClient, defaultProviderHTTPTransports
	transports := newProviderHTTPTransportSet(base)
	HTTPClient, defaultProviderHTTPTransports = &http.Client{Transport: base}, transports
	b.Cleanup(func() {
		transports.egresses.closeIdleConnections()
		base.CloseIdleConnections()
		HTTPClient, defaultProviderHTTPTransports = oldClient, oldTransports
	})
	proxy, _, err := parseProviderProxy(proxyAddress)
	if err != nil {
		b.Fatal(err)
	}
	fixed := base.Clone()
	fixed.Proxy = http.ProxyURL(proxy)
	fixedNoReplay := cloneWithoutKeepAlives(fixed)
	b.Cleanup(func() {
		fixed.CloseIdleConnections()
		fixedNoReplay.CloseIdleConnections()
	})
	r := NewHTTPRequester(proxyAddress, nil)
	r.UseHTTPProfile(profile)
	method, body := http.MethodPost, []byte(`{"input":"warm egress request"}`)
	if profile == HTTPProfileObservationGET {
		method, body = http.MethodGet, nil
	}
	wantProtocol := 1
	if h2 {
		wantProtocol = 2
	}
	perform := func(ctx context.Context) error {
		req, err := r.NewRequest(method, up.URL, r.WithContext(ctx), r.WithBody(body))
		if err != nil {
			return err
		}
		var resp *http.Response
		if cached {
			resp, err = r.Do(req)
		} else {
			client, apiErr := providerHTTPClient(profile, false)
			if apiErr != nil {
				return apiErr
			}
			client.Transport = fixed
			resp, err, _ = doHTTPRequest(client, req, policyForHTTPProfile(profile), fixedNoReplay)
		}
		if err != nil {
			return err
		}
		_, readErr := io.Copy(io.Discard, resp.Body)
		closeErr := resp.Body.Close()
		if resp.ProtoMajor != wantProtocol {
			return fmt.Errorf("protocol %s, want HTTP/%d", resp.Proto, wantProtocol)
		}
		if readErr != nil {
			return readErr
		}
		return closeErr
	}
	var reused atomic.Bool
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused.Store(info.Reused) },
	})
	if err := perform(ctx); err != nil {
		b.Fatal(err)
	}
	reused.Store(false)
	if err := perform(ctx); err != nil {
		b.Fatal(err)
	}
	if !reused.Load() {
		b.Fatal("second request did not reuse its TLS connection")
	}
	connectionsBefore := accepted.Load()
	b.ReportAllocs()
	b.ResetTimer()
	if parallel {
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if err := perform(context.Background()); err != nil {
					b.Error(err)
					return
				}
			}
		})
	} else {
		for i := 0; i < b.N; i++ {
			if err := perform(context.Background()); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(accepted.Load()-connectionsBefore)/float64(b.N), "newconns/op")
}
