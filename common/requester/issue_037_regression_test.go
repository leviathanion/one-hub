package requester

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// newI037DisconnectServer 用一个真实的 HTTP/1.1 持久连接复现：上游已读完
// 请求体后，在写响应前断开连接。测试不依赖 sleep，而是等待预热连接进入 Idle。
type i037DisconnectObservation struct {
	calls                 int32
	firstBody             atomic.Value
	firstKey              atomic.Value
	firstXKey             atomic.Value
	firstContentLength    int64
	firstTransferEncoding atomic.Value
}

func newI037DisconnectServer(t *testing.T) (*httptest.Server, *i037DisconnectObservation) {
	t.Helper()
	observation := &i037DisconnectObservation{}
	idle := make(chan struct{}, 1)

	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/warmup" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read upstream request body: %v", err)
			return
		}
		call := atomic.AddInt32(&observation.calls, 1)
		if call == 1 {
			// The first observation is deliberately ambiguous: the handler has
			// consumed the complete request before disconnecting.
			observation.firstBody.Store(string(body))
			observation.firstKey.Store(req.Header.Get("Idempotency-Key"))
			observation.firstXKey.Store(req.Header.Get("X-Idempotency-Key"))
			atomic.StoreInt64(&observation.firstContentLength, req.ContentLength)
			observation.firstTransferEncoding.Store(append([]string(nil), req.TransferEncoding...))
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("upstream response writer does not support hijacking")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack upstream connection: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	server := httptest.NewUnstartedServer(handler)
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateIdle {
			select {
			case idle <- struct{}{}:
			default:
			}
		}
	}
	server.Start()
	t.Cleanup(server.Close)

	warmup, err := server.Client().Get(server.URL + "/warmup")
	if err != nil {
		t.Fatalf("warm up keep-alive connection: %v", err)
	}
	_ = warmup.Body.Close()
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("warm-up request did not establish an idle keep-alive connection")
	}

	return server, observation
}

func i037RequesterForServer(t *testing.T, server *httptest.Server) *HTTPRequester {
	t.Helper()
	client := server.Client()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httptest client transport=%T, want *http.Transport", client.Transport)
	}
	original := HTTPClient
	HTTPClient = client
	t.Cleanup(func() { HTTPClient = original })
	requester := NewHTTPRequester("", nil)
	if requester.transports == nil || requester.transports.normal != transport {
		t.Fatal("requester did not capture the current transport factory pair")
	}
	return requester
}

type i037H2ResetServer struct {
	listener    net.Listener
	url         string
	protocols   chan string
	acceptDone  chan struct{}
	connections sync.Map
	wg          sync.WaitGroup
	calls       atomic.Int32
	resetSent   atomic.Bool
}

func newI037H2ResetServer(t *testing.T) (*i037H2ResetServer, *providerHTTPTransportSet) {
	t.Helper()
	certTemplate := httptest.NewUnstartedServer(http.NotFoundHandler())
	certTemplate.EnableHTTP2 = true
	certTemplate.StartTLS()
	clientTransport, ok := certTemplate.Client().Transport.(*http.Transport)
	if !ok {
		certTemplate.Close()
		t.Fatalf("httptest TLS client transport=%T, want *http.Transport", certTemplate.Client().Transport)
	}
	certificates := append([]tls.Certificate(nil), certTemplate.TLS.Certificates...)
	certTemplate.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen TLS ALPN test server: %v", err)
	}
	tlsListener := tls.NewListener(listener, &tls.Config{
		Certificates: certificates,
		NextProtos:   []string{http2.NextProtoTLS, "http/1.1"},
	})
	clientTransport.Protocols = &http.Protocols{}
	clientTransport.Protocols.SetHTTP1(true)
	clientTransport.Protocols.SetHTTP2(true)
	server := &i037H2ResetServer{
		listener:   tlsListener,
		url:        "https://" + listener.Addr().String(),
		protocols:  make(chan string, 4),
		acceptDone: make(chan struct{}),
	}
	t.Cleanup(func() {
		_ = tlsListener.Close()
		server.connections.Range(func(key, _ any) bool {
			_ = key.(net.Conn).Close()
			return true
		})
		<-server.acceptDone
		server.wg.Wait()
	})
	go server.acceptLoop()
	warmupClient := &http.Client{Transport: clientTransport}
	warmup, err := warmupClient.Get(server.url + "/warmup")
	if err != nil {
		t.Fatalf("warm up h2 connection: %v", err)
	}
	_ = warmup.Body.Close()
	if protocol := server.waitProtocol(t); protocol != http2.NextProtoTLS {
		t.Fatalf("h2 warm-up negotiated %q, want h2", protocol)
	}

	return server, newProviderHTTPTransportSet(clientTransport)
}

func (s *i037H2ResetServer) acceptLoop() {
	defer close(s.acceptDone)
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.connections.Store(conn, struct{}{})
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveConn(conn)
		}()
	}
}

func (s *i037H2ResetServer) serveConn(raw net.Conn) {
	defer s.connections.Delete(raw)
	conn, ok := raw.(*tls.Conn)
	if !ok {
		_ = raw.Close()
		return
	}
	defer conn.Close()
	if err := conn.Handshake(); err != nil {
		return
	}
	protocol := conn.ConnectionState().NegotiatedProtocol
	select {
	case s.protocols <- protocol:
	default:
	}
	if protocol == http2.NextProtoTLS {
		s.serveH2(conn)
		return
	}
	s.serveHTTP1(conn)
}

func (s *i037H2ResetServer) serveHTTP1(conn net.Conn) {
	request, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return
	}
	if _, err := io.ReadAll(request.Body); err != nil {
		return
	}
	s.calls.Add(1)
}

func (s *i037H2ResetServer) serveH2(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(conn, preface); err != nil || string(preface) != http2.ClientPreface {
		return
	}
	framer := http2.NewFramer(conn, conn)
	framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	if err := framer.WriteSettings(); err != nil {
		return
	}
	activeStream := uint32(0)
	activePath := ""
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			return
		}
		switch frame := frame.(type) {
		case *http2.SettingsFrame:
			if !frame.IsAck() {
				if err := framer.WriteSettingsAck(); err != nil {
					return
				}
			}
		case *http2.PingFrame:
			if !frame.IsAck() {
				if err := framer.WritePing(true, frame.Data); err != nil {
					return
				}
			}
		case *http2.MetaHeadersFrame:
			activeStream = frame.StreamID
			activePath = h2Path(frame)
			if frame.StreamEnded() {
				if !s.finishH2Request(framer, activeStream, activePath) {
					return
				}
				if s.calls.Load() >= 2 {
					return
				}
			}
		case *http2.HeadersFrame:
			activeStream = frame.StreamID
			if frame.StreamEnded() {
				if !s.finishH2Request(framer, activeStream, activePath) {
					return
				}
				if s.calls.Load() >= 2 {
					return
				}
			}
		case *http2.DataFrame:
			if frame.StreamID == activeStream && frame.StreamEnded() {
				if !s.finishH2Request(framer, activeStream, activePath) {
					return
				}
				if s.calls.Load() >= 2 {
					return
				}
			}
		}
	}
}

func h2Path(frame *http2.MetaHeadersFrame) string {
	for _, field := range frame.Fields {
		if field.Name == ":path" {
			return field.Value
		}
	}
	return ""
}

func (s *i037H2ResetServer) finishH2Request(framer *http2.Framer, streamID uint32, path string) bool {
	if path == "/warmup" {
		return writeH2Response(framer, streamID, http.StatusNoContent)
	}
	call := s.calls.Add(1)
	if call == 1 {
		s.resetSent.Store(true)
		return framer.WriteRSTStream(streamID, http2.ErrCodeProtocol) == nil
	}
	return writeH2Response(framer, streamID, http.StatusOK)
}

func writeH2Response(framer *http2.Framer, streamID uint32, status int) bool {
	var responseHeaders bytes.Buffer
	encoder := hpack.NewEncoder(&responseHeaders)
	if err := encoder.WriteField(hpack.HeaderField{Name: ":status", Value: fmt.Sprint(status)}); err != nil {
		return false
	}
	return framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: responseHeaders.Bytes(),
		EndHeaders:    true,
		EndStream:     true,
	}) == nil
}

func (s *i037H2ResetServer) waitProtocol(t *testing.T) string {
	t.Helper()
	select {
	case protocol := <-s.protocols:
		return protocol
	case <-time.After(time.Second):
		t.Fatal("TLS ALPN server did not observe a connection")
		return ""
	}
}

func TestFixI037WorkPOSTDisconnectIsNeverImplicitlyReplayed(t *testing.T) {
	for _, test := range []struct {
		name       string
		headerName string
		key        string
		body       string
	}{
		{name: "with idempotency key", headerName: "Idempotency-Key", key: "client-operation-1", body: `{"model":"gpt-5","input":"hello"}`},
		{name: "without idempotency key", body: `{"model":"gpt-5","input":"hello"}`},
		{name: "empty body with idempotency key", headerName: "Idempotency-Key", key: "client-empty-1"},
		{name: "empty body with x idempotency key", headerName: "X-Idempotency-Key", key: "client-empty-x-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, observation := newI037DisconnectServer(t)
			requester := i037RequesterForServer(t, server)
			options := []requestOption{requester.WithBody([]byte(test.body))}
			var writeCount atomic.Int32
			var gotConn atomic.Bool
			var gotReused atomic.Bool
			var wroteContentLength, wroteTransferEncoding atomic.Bool
			trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) {
				writeCount.Add(1)
			}, GotConn: func(info httptrace.GotConnInfo) {
				gotConn.Store(true)
				gotReused.Store(info.Reused)
			}}
			if test.body == "" {
				trace.WroteHeaderField = func(key string, _ []string) {
					if key == "Content-Length" {
						wroteContentLength.Store(true)
					}
					if key == "Transfer-Encoding" {
						wroteTransferEncoding.Store(true)
					}
				}
			}
			options = append(options, requester.WithContext(httptrace.WithClientTrace(context.Background(), trace)))
			if test.key != "" {
				options = append(options, requester.WithHeader(map[string]string{test.headerName: test.key}))
			}
			req, err := requester.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", options...)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			if test.body != "" && req.GetBody == nil {
				t.Fatal("test request did not have a replayable body before transport policy")
			}
			getBodyBeforeSend := req.GetBody != nil
			if test.key != "" && req.Header.Get(test.headerName) != test.key {
				t.Fatalf("request key before send=%q, want %q", req.Header.Get(test.headerName), test.key)
			}

			resp, apiErr := requester.SendRequestRaw(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if apiErr == nil || !apiErr.UpstreamAmbiguous || apiErr.UpstreamNotAttempted {
				t.Fatalf("expected one ambiguous submission failure, response=%v error=%+v", resp, apiErr)
			}
			if got := writeCount.Load(); got != 1 {
				t.Fatalf("transport wrote the work operation %d times: %d", 1, got)
			}
			if got := atomic.LoadInt32(&observation.calls); got != 1 {
				t.Fatalf("work POST was implicitly replayed: upstream calls=%d", got)
			}
			if got := observation.firstBody.Load().(string); got != test.body {
				t.Fatalf("upstream received body=%q, want %q", got, test.body)
			}
			firstKey := observation.firstKey.Load().(string)
			firstXKey := observation.firstXKey.Load().(string)
			if test.headerName == "X-Idempotency-Key" {
				if firstXKey != test.key || firstKey != "" {
					t.Fatalf("upstream received idempotency headers=%q/%q, want x-key=%q", firstKey, firstXKey, test.key)
				}
			} else if firstKey != test.key || firstXKey != "" {
				t.Fatalf("upstream received idempotency headers=%q/%q, want key=%q", firstKey, firstXKey, test.key)
			}
			if got := req.GetBody != nil; got != getBodyBeforeSend {
				t.Fatalf("transport policy mutated caller GetBody: before=%t after=%t", getBodyBeforeSend, got)
			}
			if test.body == "" {
				if got := atomic.LoadInt64(&observation.firstContentLength); got != 0 {
					t.Fatalf("empty work request changed Content-Length semantics: %d", got)
				}
				if got := observation.firstTransferEncoding.Load().([]string); len(got) != 0 {
					t.Fatalf("empty work request gained transfer encoding: %v", got)
				}
				if !wroteContentLength.Load() || wroteTransferEncoding.Load() {
					t.Fatalf("empty work wire framing changed: content-length=%t transfer-encoding=%t", wroteContentLength.Load(), wroteTransferEncoding.Load())
				}
				if !gotConn.Load() || gotReused.Load() {
					t.Fatalf("empty work operation did not use a fresh no-keep-alive connection: seen=%t reused=%t", gotConn.Load(), gotReused.Load())
				}
			}
			if test.key != "" && req.Header.Get(test.headerName) != test.key {
				t.Fatalf("transport policy changed caller header to %q", req.Header.Get(test.headerName))
			}
		})
	}
}

func TestFixI037RefusesUnmanagedTransportForEmptyKey(t *testing.T) {
	var calls atomic.Int32
	original := HTTPClient
	HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(http.NoBody), Request: req}, nil
	})}
	t.Cleanup(func() { HTTPClient = original })

	requester := NewHTTPRequester("", nil)
	req, err := requester.NewRequest(http.MethodPost, "https://provider.example/v1/work", requester.WithHeader(map[string]string{
		"Idempotency-Key": "client-empty-unmanaged",
	}))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, apiErr := requester.SendRequestRaw(req)
	if resp != nil || apiErr == nil || !apiErr.UpstreamNotAttempted || apiErr.UpstreamAmbiguous {
		t.Fatalf("expected explicit preflight transport failure, response=%v error=%+v", resp, apiErr)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("unmanaged transport was used for an implicitly replayable request: calls=%d", got)
	}
}

func TestFixI037Go125HTTP2PeerResetDoesNotReplayEmptyWork(t *testing.T) {
	for _, test := range []struct {
		name string
		key  string
	}{
		{name: "without idempotency key"},
		{name: "with idempotency key", key: "client-h2-empty-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Exercise the release Go 1.25 HTTP/2 behavior directly first. The
			// server accepts a complete END_STREAM request, records it, then sends
			// the peer PROTOCOL_ERROR reset that triggered the old replay.
			controlServer, controlTransports := newI037H2ResetServer(t)
			controlClient := &http.Client{Transport: controlTransports.normal}
			controlReq, err := http.NewRequest(http.MethodPost, controlServer.url, nil)
			if err != nil {
				t.Fatalf("build h2 control request: %v", err)
			}
			if test.key != "" {
				controlReq.Header.Set("Idempotency-Key", test.key)
			}
			controlResp, controlErr := controlClient.Do(controlReq)
			if controlResp != nil {
				_ = controlResp.Body.Close()
			}
			// The target request reuses the H2 connection established by the
			// warm-up, so no second ALPN event is expected here.
			if !controlServer.resetSent.Load() {
				t.Fatal("h2 server did not reset the completed request stream")
			}
			controlCalls := controlServer.calls.Load()
			if controlCalls < 1 || controlCalls > 2 {
				t.Fatalf("h2 control request count=%d, want one or the Go 1.25 replay", controlCalls)
			}
			t.Logf("normal h2 control: calls=%d err=%v", controlCalls, controlErr)

			fixedServer, fixedTransports := newI037H2ResetServer(t)
			original := HTTPClient
			HTTPClient = &http.Client{Transport: fixedTransports.normal}
			t.Cleanup(func() { HTTPClient = original })
			requester := NewHTTPRequester("", nil)
			options := []requestOption{requester.WithBody([]byte(""))}
			if test.key != "" {
				options = append(options, requester.WithHeader(map[string]string{"Idempotency-Key": test.key}))
			}
			request, err := requester.NewRequest(http.MethodPost, fixedServer.url, options...)
			if err != nil {
				t.Fatalf("build fixed request: %v", err)
			}
			fixedResp, fixedErr := requester.SendRequestRaw(request)
			if fixedResp != nil {
				_ = fixedResp.Body.Close()
			}
			if fixedErr == nil || !fixedErr.UpstreamAmbiguous || fixedErr.UpstreamNotAttempted {
				t.Fatalf("expected one ambiguous fixed transport failure, response=%v error=%+v", fixedResp, fixedErr)
			}
			fixedProtocol := fixedServer.waitProtocol(t)
			if fixedProtocol == http2.NextProtoTLS {
				t.Fatal("at-most-once sibling negotiated h2")
			}
			if fixedCalls := fixedServer.calls.Load(); fixedCalls != 1 {
				t.Fatalf("fixed empty work request count=%d, want 1", fixedCalls)
			}
			t.Logf("owned sibling: calls=%d protocol=%q", fixedServer.calls.Load(), fixedProtocol)
		})
	}
}

func TestFixI037SafeGETKeepsTransportRetry(t *testing.T) {
	var calls int32
	idle := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/warmup" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		call := atomic.AddInt32(&calls, 1)
		if call == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("upstream response writer does not support hijacking")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack upstream connection: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateIdle {
			select {
			case idle <- struct{}{}:
			default:
			}
		}
	}
	server.Start()
	defer server.Close()

	warmup, err := server.Client().Get(server.URL + "/warmup")
	if err != nil {
		t.Fatalf("warm up keep-alive connection: %v", err)
	}
	_ = warmup.Body.Close()
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("warm-up request did not establish an idle keep-alive connection")
	}

	requester := i037RequesterForServer(t, server)
	requester.UseHTTPProfile(HTTPProfileObservationGET)
	req, err := requester.NewRequest(http.MethodGet, server.URL+"/observation")
	if err != nil {
		t.Fatalf("build observation request: %v", err)
	}
	resp, apiErr := requester.SendRequestRaw(req)
	if apiErr != nil || resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("safe GET retry failed: response=%v error=%+v", resp, apiErr)
	}
	_ = resp.Body.Close()
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected safe GET to retry after stale keep-alive, calls=%d", got)
	}
}

func TestFixI037EachFreshSendKeepsItsOwnOperationBody(t *testing.T) {
	const body = `{"model":"gpt-5","input":"same body"}`
	const key = "client-operation-independent"
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotBody, err := io.ReadAll(req.Body)
		if err != nil {
			t.Errorf("read operation body: %v", err)
			return
		}
		if string(gotBody) != body || req.Header.Get("Idempotency-Key") != key {
			t.Errorf("operation wire changed: body=%q key=%q", gotBody, req.Header.Get("Idempotency-Key"))
		}
		atomic.AddInt32(&calls, 1)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	requester := i037RequesterForServer(t, server)
	for operation := 0; operation < 2; operation++ {
		req, err := requester.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", requester.WithBody([]byte(body)), requester.WithHeader(map[string]string{"Idempotency-Key": key}))
		if err != nil {
			t.Fatalf("build operation %d: %v", operation, err)
		}
		if req.GetBody == nil {
			t.Fatalf("operation %d did not start with a replayable body", operation)
		}
		resp, apiErr := requester.SendRequestRaw(req)
		if apiErr != nil || resp == nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("operation %d failed: response=%v error=%+v", operation, resp, apiErr)
		}
		_ = resp.Body.Close()
		if req.GetBody == nil || req.Header.Get("Idempotency-Key") != key {
			t.Fatalf("operation %d mutated its caller request", operation)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("fresh sends were not independent operations: calls=%d", got)
	}
}

func TestFixI037DoesNotFollowWorkRedirect307Or308(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				atomic.AddInt32(&calls, 1)
				if req.URL.Path == "/v1/chat/completions" {
					w.Header().Set("Location", serverURLPlaceholder())
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			requester := i037RequesterForServer(t, server)
			req, err := requester.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", requester.WithBody([]byte(`{"input":"hello"}`)))
			if err != nil {
				t.Fatalf("build work request: %v", err)
			}
			resp, apiErr := requester.SendRequestRaw(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if apiErr == nil || apiErr.StatusCode != status {
				t.Fatalf("expected original redirect status, response=%v error=%+v", resp, apiErr)
			}
			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Fatalf("work redirect was followed: calls=%d", got)
			}
		})
	}
}

func serverURLPlaceholder() string {
	// The redirect target is intentionally unreachable from the test server. A
	// followed redirect would nevertheless increment the handler's call count
	// only if the client reached a second in-process endpoint, so use a same-host
	// path resolved by the test server's Location handling in the caller below.
	return "/v1/chat/completions/redirected"
}
