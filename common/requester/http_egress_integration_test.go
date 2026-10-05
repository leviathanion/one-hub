package requester

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/utils"
)

// These tests observe the upstream TCP peer, not merely Proxy/Dial callbacks:
// the latter never run when a transport reuses a connection from another exit.
func TestHTTPEgressSeparatesProxyExits(t *testing.T) {
	for _, kind := range []string{"socks5", "http"} {
		for _, h2 := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/h2=%t", kind, h2), func(t *testing.T) {
				var exits sync.Map
				a := newEgressProxy(t, kind, "A", &exits, false)
				b := newEgressProxy(t, kind, "B", &exits, false)
				up := newEgressUpstream(t, h2, &exits, nil)
				initEgressHTTPClient(t, up)
				proxyURLs := map[string]string{"A": a.URL(), "B": b.URL(), "direct": ""}
				for i, label := range []string{"A", "A", "B", "direct", "B"} {
					// A fresh requester must still share the same exit's connection.
					reused := checkEgressRequest(t, proxyURLs[label], up.URL, context.Background(), label, h2)
					if (i == 1 || i == 4) && !reused {
						t.Fatalf("same-exit request %d did not reuse its connection", i)
					}
				}
				if a.dials.Load() != 1 || b.dials.Load() != 1 {
					t.Fatalf("unexpected tunnel counts: A=%d B=%d", a.dials.Load(), b.dials.Load())
				}
			})
		}
	}
}

func TestHTTPEgressExplicitProxyOverridesInheritedContext(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("h2=%t", h2), func(t *testing.T) {
			var exits sync.Map
			socks := newEgressProxy(t, "socks5", "SOCKS", &exits, false)
			connect := newEgressProxy(t, "http", "CONNECT", &exits, false)
			up := newEgressUpstream(t, h2, &exits, nil)
			initEgressHTTPClient(t, up)
			// Both legacy keys can be inherited. The requester's explicit
			// configuration must replace both, including an empty direct config.
			ctx := context.WithValue(context.Background(), utils.ProxySock5AddrKey, socks.URL())
			ctx = context.WithValue(ctx, utils.ProxyHTTPAddrKey, connect.URL())
			for _, tc := range []struct{ proxy, want string }{
				{"", "direct"},
				{socks.URL(), "SOCKS"},
				{connect.URL(), "CONNECT"},
				{"", "direct"},
			} {
				checkEgressRequest(t, tc.proxy, up.URL, ctx, tc.want, h2)
			}
		})
	}
}

func TestHTTPEgressSeparatesProxyAuthentication(t *testing.T) {
	for _, kind := range []string{"socks5", "http"} {
		for _, h2 := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/h2=%t", kind, h2), func(t *testing.T) {
				var exits sync.Map
				proxy := newEgressProxy(t, kind, "auth", &exits, true)
				up := newEgressUpstream(t, h2, &exits, nil)
				initEgressHTTPClient(t, up)
				for i, credentials := range [][2]string{{"Alice", "first"}, {"Alice", "first"}, {"Alice", "second"}, {"alice", "first"}, {"Alice", "first"}} {
					u, err := url.Parse(proxy.URL())
					if err != nil {
						t.Fatal(err)
					}
					u.User = url.UserPassword(credentials[0], credentials[1])
					want := "auth/" + credentials[0] + ":" + credentials[1]
					reused := checkEgressRequest(t, u.String(), up.URL, context.Background(), want, h2)
					if (i == 1 || i == 4) && !reused {
						t.Fatal("identical proxy credentials did not reuse a connection")
					}
				}
				if got := proxy.dials.Load(); got != 3 {
					t.Fatalf("authentication identities need three distinct tunnels, got %d", got)
				}
			})
		}
	}
}

func TestHTTPEgressEmptyWorkPreservesNoReplayPolicy(t *testing.T) {
	for _, kind := range []string{"socks5", "http"} {
		t.Run(kind, func(t *testing.T) {
			var exits sync.Map
			proxy := newEgressProxy(t, kind, "A", &exits, false)
			var calls atomic.Int32
			up := newEgressUpstream(t, true, &exits, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/work" {
					writeEgressResponse(w, req, &exits)
					return
				}
				calls.Add(1)
				body, err := io.ReadAll(req.Body)
				if err != nil || len(body) != 0 || req.ContentLength != 0 || len(req.TransferEncoding) != 0 {
					t.Errorf("empty work framing changed: body=%q length=%d encoding=%v err=%v", body, req.ContentLength, req.TransferEncoding, err)
				}
				if req.ProtoMajor != 1 || req.Header.Get("Idempotency-Key") != "empty-operation" {
					t.Errorf("empty work policy changed: proto=%s key=%q", req.Proto, req.Header.Get("Idempotency-Key"))
				}
				if got := observedEgress(&exits, req.RemoteAddr); got != "A" {
					t.Errorf("empty work reached %q, want A", got)
				}
				panic(http.ErrAbortHandler) // Accepted request, response lost.
			})
			initEgressHTTPClient(t, up)
			checkEgressRequest(t, proxy.URL(), up.URL, context.Background(), "A", true)
			r := NewHTTPRequester(proxy.URL(), nil)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var writes atomic.Int32
			var reused atomic.Bool
			ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
				WroteRequest: func(httptrace.WroteRequestInfo) { writes.Add(1) },
				GotConn:      func(info httptrace.GotConnInfo) { reused.Store(info.Reused) },
			})
			req, err := r.NewRequest(http.MethodPost, up.URL+"/work", r.WithContext(ctx), r.WithHeader(map[string]string{"Idempotency-Key": "empty-operation"}))
			if err != nil {
				t.Fatal(err)
			}
			resp, apiErr := r.SendRequestRaw(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if apiErr == nil || !apiErr.UpstreamAmbiguous || apiErr.UpstreamNotAttempted {
				t.Fatalf("expected ambiguous accepted work, got response=%v error=%+v", resp, apiErr)
			}
			if calls.Load() != 1 || writes.Load() != 1 || reused.Load() || proxy.dials.Load() != 2 {
				t.Fatalf("empty work replayed or reused normal pool: calls=%d writes=%d reused=%t tunnels=%d", calls.Load(), writes.Load(), reused.Load(), proxy.dials.Load())
			}
		})
	}
}

func initEgressHTTPClient(t *testing.T, upstream *httptest.Server) {
	t.Helper()
	oldClient, oldTransports := HTTPClient, defaultProviderHTTPTransports
	InitHttpClient()
	transports := defaultProviderHTTPTransports
	ca := x509.NewCertPool()
	ca.AddCert(upstream.Certificate())
	transports.normal.TLSClientConfig = &tls.Config{RootCAs: ca}
	t.Cleanup(func() {
		transports.egresses.closeIdleConnections()
		transports.normal.CloseIdleConnections()
		HTTPClient, defaultProviderHTTPTransports = oldClient, oldTransports
	})
}

func newEgressUpstream(t *testing.T, h2 bool, exits *sync.Map, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	if handler == nil {
		handler = func(w http.ResponseWriter, req *http.Request) { writeEgressResponse(w, req, exits) }
	}
	up := httptest.NewUnstartedServer(handler)
	up.EnableHTTP2 = h2
	up.StartTLS()
	t.Cleanup(up.Close)
	return up
}

func observedEgress(exits *sync.Map, remoteAddr string) string {
	if label, ok := exits.Load(remoteAddr); ok {
		return label.(string)
	}
	return "direct"
}

func writeEgressResponse(w http.ResponseWriter, req *http.Request, exits *sync.Map) {
	_, _ = io.Copy(io.Discard, req.Body)
	_, _ = io.WriteString(w, observedEgress(exits, req.RemoteAddr))
}

func checkEgressRequest(t *testing.T, proxy, target string, parent context.Context, want string, h2 bool) bool {
	t.Helper()
	r := NewHTTPRequester(proxy, nil)
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	var reused atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused.Store(info.Reused) }})
	req, err := r.NewRequest(http.MethodPost, target, r.WithContext(ctx), r.WithBody([]byte(`{"input":"egress test"}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp, apiErr := r.SendRequestRaw(req)
	if apiErr != nil {
		t.Fatalf("send via %s: %+v", want, apiErr)
	}
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read response: read=%v close=%v", readErr, closeErr)
	}
	wantProtocol := "HTTP/1.1"
	if h2 {
		wantProtocol = "HTTP/2.0"
	}
	if string(body) != want || resp.Proto != wantProtocol {
		t.Fatalf("exit=%q protocol=%s, want exit=%q protocol=%s", body, resp.Proto, want, wantProtocol)
	}
	return reused.Load()
}

type egressProxy struct {
	kind     string
	listener net.Listener
	dials    atomic.Int32
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
}

func (p *egressProxy) URL() string { return p.kind + "://" + p.listener.Addr().String() }

func newEgressProxy(t testing.TB, kind, label string, exits *sync.Map, authenticated bool) *egressProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &egressProxy{kind: kind, listener: l, conns: make(map[net.Conn]struct{})}
	acceptedDone := make(chan struct{})
	go func() {
		defer close(acceptedDone)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.conns[conn] = struct{}{}
			p.mu.Unlock()
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				defer conn.Close()
				defer func() { p.mu.Lock(); delete(p.conns, conn); p.mu.Unlock() }()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				reader := bufio.NewReader(conn)
				var target, identity string
				var handshakeErr error
				if kind == "socks5" {
					target, identity, handshakeErr = readEgressSOCKS(conn, reader, authenticated)
				} else {
					target, identity, handshakeErr = readEgressCONNECT(reader, authenticated)
				}
				if handshakeErr != nil {
					return
				}
				up, err := net.DialTimeout("tcp", target, time.Second)
				if err != nil {
					return
				}
				defer up.Close()
				_ = up.SetDeadline(time.Now().Add(5 * time.Second))
				exit := label
				if authenticated {
					exit += "/" + identity
				}
				exits.Store(up.LocalAddr().String(), exit)
				p.dials.Add(1)
				if kind == "socks5" {
					_, err = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				} else {
					_, err = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
				}
				if err != nil {
					return
				}
				_ = conn.SetDeadline(time.Time{})
				_ = up.SetDeadline(time.Time{})
				done := make(chan struct{})
				go func() { _, _ = io.Copy(up, reader); _ = up.Close(); close(done) }()
				_, _ = io.Copy(conn, up)
				_ = conn.Close()
				<-done
			}()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		<-acceptedDone
		p.mu.Lock()
		for conn := range p.conns {
			_ = conn.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
	})
	return p
}

func readEgressCONNECT(reader *bufio.Reader, authenticated bool) (string, string, error) {
	req, err := http.ReadRequest(reader)
	if err != nil {
		return "", "", err
	}
	defer req.Body.Close()
	if req.Method != http.MethodConnect {
		return "", "", fmt.Errorf("expected CONNECT")
	}
	identity := ""
	if authenticated {
		header := req.Header.Get("Proxy-Authorization")
		if !strings.HasPrefix(header, "Basic ") {
			return "", "", fmt.Errorf("missing proxy authentication")
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Basic "))
		if err != nil {
			return "", "", err
		}
		identity = string(decoded)
	}
	return req.Host, identity, nil
}

func readEgressSOCKS(conn net.Conn, reader io.Reader, authenticated bool) (string, string, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return "", "", err
	}
	if header[0] != 5 {
		return "", "", fmt.Errorf("expected SOCKS5")
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, methods); err != nil {
		return "", "", err
	}
	method := byte(0)
	if authenticated {
		method = 2
	}
	if !bytes.Contains(methods, []byte{method}) {
		return "", "", fmt.Errorf("expected SOCKS auth method %d", method)
	}
	if _, err := conn.Write([]byte{5, method}); err != nil {
		return "", "", err
	}
	identity := ""
	if authenticated {
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			return "", "", err
		}
		if header[0] != 1 {
			return "", "", fmt.Errorf("expected username/password auth")
		}
		user := make([]byte, int(header[1]))
		if _, err := io.ReadFull(reader, user); err != nil {
			return "", "", err
		}
		var n [1]byte
		if _, err := io.ReadFull(reader, n[:]); err != nil {
			return "", "", err
		}
		password := make([]byte, int(n[0]))
		if _, err := io.ReadFull(reader, password); err != nil {
			return "", "", err
		}
		identity = string(user) + ":" + string(password)
		if _, err := conn.Write([]byte{1, 0}); err != nil {
			return "", "", err
		}
	}
	var request [4]byte
	if _, err := io.ReadFull(reader, request[:]); err != nil {
		return "", "", err
	}
	if request[0] != 5 || request[1] != 1 {
		return "", "", fmt.Errorf("expected SOCKS CONNECT")
	}
	var host string
	switch request[3] {
	case 1, 4:
		n := 4
		if request[3] == 4 {
			n = 16
		}
		ip := make([]byte, n)
		if _, err := io.ReadFull(reader, ip); err != nil {
			return "", "", err
		}
		host = net.IP(ip).String()
	case 3:
		var n [1]byte
		if _, err := io.ReadFull(reader, n[:]); err != nil {
			return "", "", err
		}
		name := make([]byte, int(n[0]))
		if _, err := io.ReadFull(reader, name); err != nil {
			return "", "", err
		}
		host = string(name)
	default:
		return "", "", fmt.Errorf("unexpected SOCKS address type")
	}
	var port [2]byte
	if _, err := io.ReadFull(reader, port[:]); err != nil {
		return "", "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), identity, nil
}
