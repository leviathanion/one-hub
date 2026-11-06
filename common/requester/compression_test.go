package requester

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestZstdRequestBodyPreservesBytesAndSkipsUnhelpfulCompression(t *testing.T) {
	incompressible := make([]byte, 4096)
	if _, err := rand.Read(incompressible); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		body     []byte
		encoding string
	}{
		{"small", []byte(`{"future":null}`), ""},
		{"incompressible", incompressible, ""},
		{"json", []byte(`{"number":9007199254740993,"future":null,"input":"` + strings.Repeat("上下文 ", 400) + `"}`), "zstd"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := NewHTTPRequester("", nil)
			req, err := r.NewRequest(http.MethodPost, "https://example.com/responses",
				r.WithZstdBody(test.body), r.WithHeader(map[string]string{
					"Content-Type": "application/json", "Content-Encoding": "old", "Content-Length": "1",
				}))
			if err != nil {
				t.Fatal(err)
			}
			defer req.Body.Close()
			wire, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			if req.Header.Get("Content-Encoding") != test.encoding || req.ContentLength != int64(len(wire)) || req.Header.Get("Content-Length") != "" || req.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("encoding/length mismatch: header=%v length=%d wire=%d", req.Header, req.ContentLength, len(wire))
			}
			plain := wire
			if test.encoding == "zstd" {
				decoder, err := zstd.NewReader(nil)
				if err != nil {
					t.Fatal(err)
				}
				defer decoder.Close()
				plain, err = decoder.DecodeAll(wire, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(plain, test.body) {
				t.Fatal("transport encoding changed request bytes")
			}
		})
	}
}

type compressionRoundTripper func(*http.Request) (*http.Response, error)

func (f compressionRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func installCompressionTransport(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	before := HTTPClient
	HTTPClient = &http.Client{Transport: transport}
	t.Cleanup(func() { HTTPClient = before })
}

func compressedFixture(t *testing.T, encoding string, plain []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	var writer io.WriteCloser
	if encoding == "gzip" {
		writer = gzip.NewWriter(&out)
	} else {
		var err error
		writer, err = zstd.NewWriter(&out, zstd.WithEncoderConcurrency(1))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestResponseCompressionIsOptInAndPreservesStatusAndPayload(t *testing.T) {
	plain := []byte(`{"error":{"code":"future_code","message":"wait","future":9007199254740993}}`)
	for _, encoding := range []string{"gzip", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			wire := compressedFixture(t, encoding, plain)
			var accept string
			installCompressionTransport(t, compressionRoundTripper(func(req *http.Request) (*http.Response, error) {
				accept = req.Header.Get("Accept-Encoding")
				return &http.Response{StatusCode: 429, Request: req, Body: io.NopCloser(bytes.NewReader(wire)), ContentLength: int64(len(wire)), Header: http.Header{
					"Content-Encoding": {encoding}, "Content-Type": {"application/json"}, "Content-Length": {"123"},
					"Etag": {"encoded-tag"}, "Digest": {"encoded-digest"}, "Retry-After": {"7"},
				}}, nil
			}))
			base := NewHTTPRequester("", nil)
			for _, enabled := range []bool{false, true, false} {
				r := base
				if enabled {
					r = base.ForResponseCompression()
				}
				req, _ := http.NewRequest(http.MethodPost, "https://example.com", strings.NewReader("{}"))
				resp, err := r.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "7" {
					t.Fatalf("response changed: status=%d header=%v err=%v", resp.StatusCode, resp.Header, err)
				}
				if enabled {
					if accept != "gzip, zstd" || !bytes.Equal(body, plain) || !resp.Uncompressed || resp.ContentLength != -1 {
						t.Fatalf("response was not decoded: accept=%q body=%q", accept, body)
					}
					for _, key := range []string{"Content-Encoding", "Content-Length", "Etag", "Digest"} {
						if resp.Header.Get(key) != "" {
							t.Errorf("stale representation header %s", key)
						}
					}
				} else if accept != "" || !bytes.Equal(body, wire) || resp.Header.Get("Content-Encoding") != encoding {
					t.Fatal("compression changed the original requester exact-wire behavior")
				}
				if req.Header.Get("Accept-Encoding") != "" {
					t.Fatal("sending changed the caller's request headers")
				}
			}
		})
	}
}

func TestCompressedUnaryJSONRequiresCompleteBody(t *testing.T) {
	for _, encoding := range []string{"gzip", "zstd"} {
		for _, suffix := range []string{"", strings.Repeat(" ", 4096)} {
			plain := []byte(`{"future":9007199254740993,"nullable":null}` + suffix)
			for _, integrity := range []string{"valid", "checksum", "truncated"} {
				for _, outputResp := range []bool{false, true} {
					name := encoding + "/" + integrity
					if suffix != "" {
						name += "/trailing whitespace"
					}
					if outputResp {
						name += "/replay body"
					}
					t.Run(name, func(t *testing.T) {
						wire := compressedFixture(t, encoding, plain)
						switch integrity {
						case "checksum":
							index := len(wire) - 1
							if encoding == "gzip" {
								index = len(wire) - 8
							}
							wire[index] ^= 1
						case "truncated":
							wire = wire[:len(wire)-1]
						}
						installCompressionTransport(t, compressionRoundTripper(func(req *http.Request) (*http.Response, error) {
							return &http.Response{StatusCode: 200, Request: req,
								Header: http.Header{"Content-Encoding": {encoding}}, Body: io.NopCloser(bytes.NewReader(wire))}, nil
						}))
						r := NewHTTPRequester("", nil).ForResponseCompression()
						req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
						var body map[string]json.RawMessage
						resp, apiErr := r.SendRequest(req, &body, outputResp)
						if integrity != "valid" {
							if apiErr == nil || resp != nil || !apiErr.UpstreamAccepted || apiErr.UpstreamNotAttempted {
								t.Fatalf("invalid compressed JSON was accepted: response=%v error=%+v", resp, apiErr)
							}
							return
						}
						if apiErr != nil || string(body["future"]) != "9007199254740993" || string(body["nullable"]) != "null" {
							t.Fatalf("valid JSON changed: body=%v err=%+v", body, apiErr)
						}
						if outputResp {
							defer resp.Body.Close()
							got, err := io.ReadAll(resp.Body)
							if err != nil || !bytes.Equal(got, plain) {
								t.Fatalf("response body changed: bytes=%d want=%d err=%v", len(got), len(plain), err)
							}
						}
					})
				}
			}
		}
	}
}

func TestResponseCompressionRejectsOversizedUnaryBody(t *testing.T) {
	for _, mode := range []string{"string", "json", "json with replay"} {
		t.Run(mode, func(t *testing.T) {
			installCompressionTransport(t, compressionRoundTripper(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Request: req, Header: make(http.Header),
					Body: io.NopCloser(io.MultiReader(strings.NewReader(`{"ok":true}`), io.LimitReader(repeatingResponseReader{}, maxProviderRawJSONBodyBytes+1))),
				}, nil
			}))
			r := NewHTTPRequester("", nil).ForResponseCompression()
			req, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
			var body any = new(map[string]json.RawMessage)
			if mode == "string" {
				body = new(string)
			}
			_, apiErr := r.SendRequest(req, body, mode != "json")
			if apiErr == nil || !apiErr.UpstreamAccepted || !strings.Contains(apiErr.Message, errProviderRawJSONBodyTooLarge.Error()) {
				t.Fatalf("oversized response was truncated or lost accepted execution: %+v", apiErr)
			}
		})
	}
}

type repeatingResponseReader struct{}

func (repeatingResponseReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestCompressedResponseStreamsBeforeEOF(t *testing.T) {
	for _, encoding := range []string{"gzip", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Encoding", encoding)
				w.Header().Set("Content-Type", "text/event-stream")
				var writer interface {
					io.WriteCloser
					Flush() error
				}
				if encoding == "gzip" {
					writer = gzip.NewWriter(w)
				} else {
					var err error
					writer, err = zstd.NewWriter(w, zstd.WithEncoderConcurrency(1))
					if err != nil {
						t.Error(err)
						return
					}
				}
				_, _ = io.WriteString(writer, "data: first\n\n")
				_ = writer.Flush()
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-req.Context().Done():
				}
				_ = writer.Close()
			}))
			defer server.Close()
			defer close(release)
			installCompressionTransport(t, server.Client().Transport)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			r := NewHTTPRequester("", nil).ForResponseCompression().ForHTTPProfile(HTTPProfileLongStream)
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
			resp, err := r.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			line, err := bufio.NewReader(resp.Body).ReadString('\n')
			if err != nil || line != "data: first\n" {
				t.Fatalf("stream waited for EOF or changed bytes: line=%q err=%v", line, err)
			}
		})
	}
}

type compressionReadSignal struct {
	io.ReadCloser
	started chan struct{}
	once    sync.Once
}

func (r *compressionReadSignal) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.ReadCloser.Read(p)
}

func TestClosingCompressedResponseUnblocksRead(t *testing.T) {
	for _, encoding := range []string{"gzip", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer writer.Close()
			source := &compressionReadSignal{ReadCloser: reader, started: make(chan struct{})}
			body := &compressedResponseBody{source: source, encoding: encoding}
			defer body.Close()
			done := make(chan error, 1)
			go func() { _, err := body.Read(make([]byte, 1)); done <- err }()
			select {
			case <-source.started:
			case <-time.After(3 * time.Second):
				t.Fatal("decoder did not start reading")
			}
			if err := body.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("closed stream returned no error")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("close did not unblock the decoder")
			}
		})
	}
}
