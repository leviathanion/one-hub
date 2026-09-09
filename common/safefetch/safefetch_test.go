package safefetch

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\nfixture")

type remoteAddrConn struct {
	net.Conn
	remote net.Addr
}

func (c *remoteAddrConn) RemoteAddr() net.Addr { return c.remote }

func staticLookup(ips ...string) lookupNetIPFunc {
	parsed := make([]netip.Addr, 0, len(ips))
	for _, raw := range ips {
		parsed = append(parsed, netip.MustParseAddr(raw))
	}
	return func(context.Context, string, string) ([]netip.Addr, error) {
		return append([]netip.Addr(nil), parsed...), nil
	}
}

func serverFetcher(t *testing.T, server *httptest.Server, remoteIP string) (*fetcher, string, *atomic.Int32) {
	t.Helper()
	serverAddr := server.Listener.Addr().String()
	dials := &atomic.Int32{}
	f := newFetcher(staticLookup(remoteIP), func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		conn, err := (&net.Dialer{}).DialContext(ctx, network, serverAddr)
		if err != nil {
			return nil, err
		}
		return &remoteAddrConn{
			Conn:   conn,
			remote: &net.TCPAddr{IP: net.ParseIP(remoteIP), Port: 80},
		}, nil
	})
	t.Cleanup(f.transport.CloseIdleConnections)
	return f, "http://images.example/image", dials
}

func imageServer(handler http.HandlerFunc) *httptest.Server {
	if handler == nil {
		handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes)
		}
	}
	return httptest.NewServer(handler)
}

func TestDefaultTransportHasFixedSafePolicy(t *testing.T) {
	transport := defaultFetcher.transport
	assert.Nil(t, transport.Proxy)
	assert.True(t, transport.DisableCompression)
	assert.False(t, transport.ForceAttemptHTTP2)
	assert.Equal(t, tlsTimeout, transport.TLSHandshakeTimeout)
	assert.Equal(t, responseTimeout, transport.ResponseHeaderTimeout)
	assert.Equal(t, idleTimeout, transport.IdleConnTimeout)
	assert.Equal(t, maxConnsPerHost, transport.MaxConnsPerHost)
	assert.Equal(t, maxIdleConns, transport.MaxIdleConns)
	assert.Equal(t, maxIdleConnsPerHost, transport.MaxIdleConnsPerHost)
	assert.Equal(t, int64(maxHeaderBytes), transport.MaxResponseHeaderBytes)
	assert.Zero(t, defaultFetcher.client.Timeout)
}

func TestFetchAcceptsMatchingBoundedImage(t *testing.T) {
	server := imageServer(nil)
	defer server.Close()
	f, targetURL, _ := serverFetcher(t, server, "93.184.216.34")

	mimeType, body, err := f.fetch(context.Background(), targetURL)
	require.NoError(t, err)
	assert.Equal(t, "image/png", mimeType)
	assert.Equal(t, pngBytes, body)
}

func TestFetchAcceptsSupportedImageMIMEs(t *testing.T) {
	tests := map[string][]byte{
		"image/gif":  []byte("GIF89a\x01\x00\x01\x00"),
		"image/jpeg": []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00fixture"),
		"image/png":  pngBytes,
		"image/webp": []byte("RIFF\x10\x00\x00\x00WEBPVP8 fixture"),
	}
	for mimeType, fixture := range tests {
		t.Run(mimeType, func(t *testing.T) {
			server := imageServer(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", mimeType)
				_, _ = w.Write(fixture)
			})
			defer server.Close()
			f, targetURL, _ := serverFetcher(t, server, "93.184.216.34")

			gotMIME, gotBody, err := f.fetch(context.Background(), targetURL)
			require.NoError(t, err)
			assert.Equal(t, mimeType, gotMIME)
			assert.Equal(t, fixture, gotBody)
		})
	}
}

func TestValidatedImageMIMEUsesBoundedSniffedImageType(t *testing.T) {
	jpegBytes := []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00fixture")
	tests := []struct {
		name        string
		contentType string
		body        []byte
		expected    string
	}{
		{name: "missing declaration", body: pngBytes, expected: "image/png"},
		{name: "generic declaration", contentType: "application/octet-stream", body: pngBytes, expected: "image/png"},
		{name: "jpg alias", contentType: "image/jpg", body: jpegBytes, expected: "image/jpeg"},
		{name: "parameters", contentType: "image/png; name=image.png", body: pngBytes, expected: "image/png"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mimeType, err := validatedImageMIME(test.contentType, test.body)
			require.NoError(t, err)
			assert.Equal(t, test.expected, mimeType)
		})
	}
}

func TestValidatedImageMIMERejectsNonImageAndConflicts(t *testing.T) {
	for name, test := range map[string]struct {
		contentType string
		body        []byte
	}{
		"explicit non-image": {contentType: "text/plain", body: pngBytes},
		"unsupported image":  {contentType: "image/svg+xml", body: pngBytes},
		"conflict":           {contentType: "image/jpeg", body: pngBytes},
		"short generic body": {contentType: "application/octet-stream", body: []byte{1}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := validatedImageMIME(test.contentType, test.body)
			require.Error(t, err)
		})
	}
}

func TestFetchSendsIdentityEncodingAndPreservesHost(t *testing.T) {
	var gotEncoding, gotHost string
	server := imageServer(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Accept-Encoding")
		gotHost = r.Host
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	})
	defer server.Close()
	f, targetURL, _ := serverFetcher(t, server, "93.184.216.34")

	_, _, err := f.fetch(context.Background(), targetURL)
	require.NoError(t, err)
	assert.Equal(t, "identity", gotEncoding)
	assert.Equal(t, "images.example", gotHost)
}

func TestValidateTargetRejectsMalformedAndUnsafeURLs(t *testing.T) {
	f := newFetcher(staticLookup("93.184.216.34"), nil)
	t.Cleanup(f.transport.CloseIdleConnections)

	for _, rawURL := range []string{
		"ftp://images.example/a.png",
		"http://user:password@images.example/a.png",
		"http:///a.png",
		"http://images.example:0/a.png",
		"http://images.example:/a.png",
		"http://images.example:443/a.png",
		"http://images.example:8080/a.png",
		"https://images.example:80/a.png",
		"https://images.example:8443/a.png",
	} {
		t.Run(rawURL, func(t *testing.T) {
			_, _, err := f.validateTarget(context.Background(), rawURL)
			require.Error(t, err)
		})
	}
}

func TestValidateTargetAcceptsOnlyDefaultPorts(t *testing.T) {
	f := newFetcher(staticLookup("93.184.216.34"), nil)
	t.Cleanup(f.transport.CloseIdleConnections)

	tests := map[string]string{
		"http://images.example/image":      "80",
		"http://images.example:80/image":   "80",
		"https://images.example/image":     "443",
		"https://images.example:443/image": "443",
	}
	for rawURL, expectedPort := range tests {
		t.Run(rawURL, func(t *testing.T) {
			_, target, err := f.validateTarget(context.Background(), rawURL)
			require.NoError(t, err)
			assert.Equal(t, expectedPort, target.port)
		})
	}
}

func TestValidateTargetTrimsAndNormalizesURL(t *testing.T) {
	f := newFetcher(staticLookup("93.184.216.34"), nil)
	t.Cleanup(f.transport.CloseIdleConnections)

	u, _, err := f.validateTarget(context.Background(), " \nHtTp://IMAGES.example/image\t ")
	require.NoError(t, err)
	assert.Equal(t, "http", u.Scheme)
	assert.Equal(t, "images.example", u.Host)

	_, _, err = f.validateTarget(context.Background(), "http://images.example/"+strings.Repeat("a", maxURLBytes))
	require.ErrorContains(t, err, "URL")
}

func TestFetchRejectsNilContext(t *testing.T) {
	_, _, err := Fetch(nil, "http://images.example/image")
	require.ErrorContains(t, err, "context")
}

func TestValidateTargetNormalizesIDNA(t *testing.T) {
	var resolvedHost string
	f := newFetcher(func(_ context.Context, network, host string) ([]netip.Addr, error) {
		assert.Equal(t, "ip4", network)
		resolvedHost = host
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, nil)
	t.Cleanup(f.transport.CloseIdleConnections)

	u, target, err := f.validateTarget(context.Background(), "https://BÜCHER.example./image.png")
	require.NoError(t, err)
	assert.Equal(t, "xn--bcher-kva.example", resolvedHost)
	assert.Equal(t, "xn--bcher-kva.example", target.host)
	assert.Equal(t, "xn--bcher-kva.example", u.Host)
}

func TestValidateTargetRejectsAnyUnsafeDNSAnswer(t *testing.T) {
	tests := map[string][]string{
		"mixed public private": {"93.184.216.34", "10.0.0.1"},
		"IPv4 mapped private":  {"::ffff:169.254.169.254"},
		"loopback":             {"127.0.0.1"},
		"link local":           {"169.254.169.254"},
		"unspecified":          {"0.0.0.0"},
		"multicast":            {"224.0.0.1"},
		"CGNAT":                {"100.64.0.1"},
		"documentation range":  {"203.0.113.1"},
		"public IPv6":          {"2606:4700:4700::1111"},
		"IPv6 private":         {"fd00::1"},
		"IPv6 link local":      {"fe80::1"},
		"IPv6 documentation":   {"2001:db8::1"},
		"NAT64 private target": {"64:ff9b::a00:1"},
	}
	for name, rawIPs := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFetcher(staticLookup(rawIPs...), nil)
			t.Cleanup(f.transport.CloseIdleConnections)
			_, _, err := f.validateTarget(context.Background(), "http://images.example/image")
			require.Error(t, err)
		})
	}
}

func TestValidateTargetRejectsAllIPv6Literals(t *testing.T) {
	f := newFetcher(staticLookup("93.184.216.34"), nil)
	t.Cleanup(f.transport.CloseIdleConnections)

	for _, rawURL := range []string{
		"https://[2606:4700:4700::1111]/image",
		"http://[64:ff9b::a00:1]/image",
		"http://[::ffff:93.184.216.34]/image",
	} {
		_, _, err := f.validateTarget(context.Background(), rawURL)
		require.ErrorContains(t, err, "IPv4")
	}
}

func TestValidateTargetAcceptsOnlyPublicIPv4(t *testing.T) {
	f := newFetcher(staticLookup("93.184.216.34"), nil)
	t.Cleanup(f.transport.CloseIdleConnections)

	_, target, err := f.validateTarget(context.Background(), "https://images.example/image")
	require.NoError(t, err)
	assert.Len(t, target.allowed, 1)
}

func TestFetchPreservesTLSServerName(t *testing.T) {
	var gotSNI string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	server.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			gotSNI = hello.ServerName
			return nil, nil
		},
	}
	server.StartTLS()
	defer server.Close()
	serverAddr := server.Listener.Addr().String()
	f := newFetcher(staticLookup("93.184.216.34"), func(ctx context.Context, network, _ string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, serverAddr)
		if err != nil {
			return nil, err
		}
		return &remoteAddrConn{
			Conn:   conn,
			remote: &net.TCPAddr{IP: net.ParseIP("93.184.216.34"), Port: 443},
		}, nil
	})
	// Only this local-certificate test fetcher skips verification. The default
	// immutable transport policy continues to perform normal TLS verification.
	f.transport.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec
	t.Cleanup(f.transport.CloseIdleConnections)

	_, _, err := f.fetch(context.Background(), "https://images.example/image")
	require.NoError(t, err)
	assert.Equal(t, "images.example", gotSNI)
}

func TestFetchDoesNotFollowRedirect(t *testing.T) {
	var requests atomic.Int32
	server := imageServer(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Location", "http://169.254.169.254/latest/meta-data")
		w.WriteHeader(http.StatusFound)
	})
	defer server.Close()
	f, targetURL, _ := serverFetcher(t, server, "93.184.216.34")

	_, _, err := f.fetch(context.Background(), targetURL)
	require.ErrorContains(t, err, "302")
	assert.Equal(t, int32(1), requests.Load())
}

func TestFetchHonorsCancellation(t *testing.T) {
	started := make(chan struct{})
	server := imageServer(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-time.After(time.Second)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	})
	defer server.Close()
	f, targetURL, _ := serverFetcher(t, server, "93.184.216.34")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := f.fetch(ctx, targetURL)
		done <- err
	}()
	<-started
	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Fetch did not stop after cancellation")
	}
}

func TestFetchCancellationStopsDNSResolution(t *testing.T) {
	lookupStarted := make(chan struct{})
	f := newFetcher(func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
		close(lookupStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}, nil)
	t.Cleanup(f.transport.CloseIdleConnections)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := f.fetch(ctx, "https://images.example/image")
		done <- err
	}()
	<-lookupStarted
	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("DNS resolution did not stop after cancellation")
	}
}

func TestValidatedDialerRejectsRemoteAddrMismatchAndCloses(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	var closed atomic.Bool
	conn := &closeTrackingConn{Conn: client, closed: &closed, remote: &net.TCPAddr{IP: net.ParseIP("1.1.1.1"), Port: 80}}
	f := newFetcher(staticLookup("93.184.216.34"), func(context.Context, string, string) (net.Conn, error) {
		return conn, nil
	})
	t.Cleanup(f.transport.CloseIdleConnections)
	target := &requestTarget{
		host: "images.example",
		port: "80",
		allowed: map[netip.Addr]struct{}{
			netip.MustParseAddr("93.184.216.34"): {},
		},
	}
	ctx := context.WithValue(context.Background(), requestTargetKey{}, target)

	_, err := f.transport.DialContext(ctx, "tcp", "images.example:80")
	require.ErrorContains(t, err, "远端地址")
	assert.True(t, closed.Load())
}

func TestRemoteAddrValidationRequiresExpectedIPAndPort(t *testing.T) {
	target := &requestTarget{
		port: "80",
		allowed: map[netip.Addr]struct{}{
			netip.MustParseAddr("93.184.216.34"): {},
		},
	}
	assert.True(t, remoteAddrAllowed(&net.TCPAddr{IP: net.ParseIP("93.184.216.34"), Port: 80}, target))
	assert.False(t, remoteAddrAllowed(&net.TCPAddr{IP: net.ParseIP("93.184.216.34"), Port: 81}, target))
	assert.False(t, remoteAddrAllowed(&net.TCPAddr{IP: net.ParseIP("1.1.1.1"), Port: 80}, target))
}

type closeTrackingConn struct {
	net.Conn
	closed *atomic.Bool
	remote net.Addr
}

func (c *closeTrackingConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func (c *closeTrackingConn) RemoteAddr() net.Addr { return c.remote }

func TestFetchChecksReusedConnectionAgainstCurrentDNSAnswers(t *testing.T) {
	server := imageServer(nil)
	defer server.Close()
	serverAddr := server.Listener.Addr().String()

	var mu sync.Mutex
	current := netip.MustParseAddr("93.184.216.34")
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		mu.Lock()
		defer mu.Unlock()
		return []netip.Addr{current}, nil
	}
	f := newFetcher(lookup, func(ctx context.Context, network, requested string) (net.Conn, error) {
		requestedHost, _, err := net.SplitHostPort(requested)
		if err != nil {
			return nil, err
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, network, serverAddr)
		if err != nil {
			return nil, err
		}
		return &remoteAddrConn{Conn: conn, remote: &net.TCPAddr{IP: net.ParseIP(requestedHost), Port: 80}}, nil
	})
	t.Cleanup(f.transport.CloseIdleConnections)
	targetURL := "http://images.example/image"

	_, _, err := f.fetch(context.Background(), targetURL)
	require.NoError(t, err)
	mu.Lock()
	current = netip.MustParseAddr("1.1.1.1")
	mu.Unlock()
	_, _, err = f.fetch(context.Background(), targetURL)
	require.ErrorContains(t, err, "远端地址")
}

func TestFetchReusesValidatedConnection(t *testing.T) {
	var requests atomic.Int32
	server := imageServer(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("Cookie"))
		assert.Empty(t, r.Header.Get("Proxy-Authorization"))
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	})
	defer server.Close()
	f, targetURL, dials := serverFetcher(t, server, "93.184.216.34")

	for range 2 {
		_, _, err := f.fetch(context.Background(), targetURL)
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), dials.Load())
	assert.Equal(t, int32(2), requests.Load())
}

func TestFetchRejectsStatusEncodingLengthAndMIMEViolations(t *testing.T) {
	tests := map[string]http.HandlerFunc{
		"non 2xx": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
		"encoded": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes)
		},
		"content length": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Content-Length", "10485761")
			w.WriteHeader(http.StatusOK)
		},
		"unlisted MIME": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/svg+xml")
			_, _ = io.WriteString(w, `<svg xmlns="http://www.w3.org/2000/svg"/>`)
		},
		"MIME mismatch": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(pngBytes)
		},
		"short octet stream": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte{1})
		},
	}
	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			server := imageServer(handler)
			defer server.Close()
			f, targetURL, _ := serverFetcher(t, server, "93.184.216.34")
			_, _, err := f.fetch(context.Background(), targetURL)
			require.Error(t, err)
		})
	}
}

func TestFetchRejectsStreamingBodyOverLimit(t *testing.T) {
	server := imageServer(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write(pngBytes)
		flusher.Flush()
		_, _ = io.CopyN(w, repeatingReader('x'), maxBodyBytes)
	})
	defer server.Close()
	f, targetURL, _ := serverFetcher(t, server, "93.184.216.34")

	_, _, err := f.fetch(context.Background(), targetURL)
	require.ErrorContains(t, err, "上限")
}

func TestFetchRejectsOversizedResponseHeaders(t *testing.T) {
	server := imageServer(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("X-Oversized", strings.Repeat("x", maxHeaderBytes+1))
		_, _ = w.Write(pngBytes)
	})
	defer server.Close()
	f, targetURL, _ := serverFetcher(t, server, "93.184.216.34")

	_, _, err := f.fetch(context.Background(), targetURL)
	require.Error(t, err)
}

type repeatingReader byte

func (r repeatingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func TestValidatedDialerRejectsWrongHostPortAndIP(t *testing.T) {
	f := newFetcher(nil, func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("dialer must not be reached")
	})
	t.Cleanup(f.transport.CloseIdleConnections)
	target := &requestTarget{
		host: "images.example",
		port: "443",
		allowed: map[netip.Addr]struct{}{
			netip.MustParseAddr("93.184.216.34"): {},
		},
	}
	ctx := context.WithValue(context.Background(), requestTargetKey{}, target)

	for _, addr := range []string{"other.example:443", "images.example:80", "1.1.1.1:443"} {
		_, err := f.transport.DialContext(ctx, "tcp", addr)
		require.Error(t, err)
	}
	_, err := f.transport.DialContext(ctx, "udp", "images.example:443")
	require.ErrorContains(t, err, "TCP")
	_, err = f.transport.DialContext(context.Background(), "tcp", "images.example:443")
	require.ErrorContains(t, err, "缺少已验证目标")
}

func TestValidatedDialerAcceptsVerifiedNumericIP(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	var dialed string
	f := newFetcher(nil, func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = addr
		return &remoteAddrConn{Conn: client, remote: &net.TCPAddr{IP: net.ParseIP("93.184.216.34"), Port: 443}}, nil
	})
	t.Cleanup(f.transport.CloseIdleConnections)
	target := &requestTarget{
		host: "images.example",
		port: "443",
		allowed: map[netip.Addr]struct{}{
			netip.MustParseAddr("93.184.216.34"): {},
		},
	}
	ctx := context.WithValue(context.Background(), requestTargetKey{}, target)

	conn, err := f.transport.DialContext(ctx, "tcp", "93.184.216.34:443")
	require.NoError(t, err)
	assert.Equal(t, "93.184.216.34:443", dialed)
	require.NoError(t, conn.Close())
}
