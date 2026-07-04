package requester

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"one-api/common/utils"
)

func TestNewRequestWithContextPreservesProxyContext(t *testing.T) {
	requester := NewHTTPRequester("http://proxy.example:8080", nil)
	type contextKey string
	ctx := context.WithValue(context.Background(), contextKey("request-id"), "req-1")

	req, err := requester.NewRequest(http.MethodGet, "https://example.com", requester.WithContext(ctx))
	if err != nil {
		t.Fatalf("NewRequest returned error: %v", err)
	}
	if got := req.Context().Value(contextKey("request-id")); got != "req-1" {
		t.Fatalf("expected caller context value to be preserved, got %v", got)
	}
	if got := req.Context().Value(utils.ProxyHTTPAddrKey); got != "http://proxy.example:8080" {
		t.Fatalf("expected proxy context value to be preserved, got %v", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type trackingReadCloser struct {
	reader io.Reader
	closed bool
}

func (b *trackingReadCloser) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *trackingReadCloser) Close() error {
	b.closed = true
	return nil
}

func TestSendRequestOutputRespClosesOriginalBodyOnDecodeFailure(t *testing.T) {
	originalHTTPClient := HTTPClient
	body := &trackingReadCloser{reader: strings.NewReader(`{"broken"`)}
	HTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       body,
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	t.Cleanup(func() {
		HTTPClient = originalHTTPClient
	})

	requester := NewHTTPRequester("", nil)
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	var decoded struct {
		OK bool `json:"ok"`
	}
	resp, apiErr := requester.SendRequest(req, &decoded, true)
	if apiErr == nil || apiErr.Code != "decode_response_failed" {
		t.Fatalf("expected decode failure, resp=%v err=%+v", resp, apiErr)
	}
	if !body.closed {
		t.Fatal("expected original response body to be closed on decode failure")
	}
}
