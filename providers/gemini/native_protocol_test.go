package gemini

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func newNativeGeminiProviderForTest(t *testing.T, server *httptest.Server, raw string, stream bool) (*GeminiProvider, *GeminiChatRequest) {
	t.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/gemini/v1beta/models/gemini-test:generateContent", strings.NewReader(raw))
	if _, err := common.CacheRequestBody(ctx); err != nil {
		t.Fatalf("cache Gemini request: %v", err)
	}
	request := &GeminiChatRequest{}
	if err := common.UnmarshalBodyReusable(ctx, request); err != nil {
		t.Fatalf("decode Gemini projection: %v", err)
	}
	request.Model = "gemini-test"
	request.Stream = stream
	proxy := ""
	provider := GeminiProviderFactory{}.Create(&model.Channel{Type: config.ChannelTypeGemini, Key: "provider-key", Proxy: &proxy}).(*GeminiProvider)
	provider.Config.BaseURL = server.URL
	provider.SetContext(ctx)
	provider.SetOriginalModel(request.Model)
	provider.SetUsage(&types.Usage{})
	return provider, request
}

func TestNativeGeminiUnaryPreservesUnknownResponseFields(t *testing.T) {
	responseWire := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP","finishMessage":"done","logprobsResult":{"chosenCandidates":[]},"index":0}],"modelVersion":"gemini-test-actual","modelStatus":{"state":"READY"},"urlContextMetadata":{"urlMetadata":[]},"future":{"kept":true}}`)
	var requestWire []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestWire, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(responseWire)
	}))
	t.Cleanup(server.Close)
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })
	raw := `{"contents":[{"role":"user","parts":[{"text":"hi","future":{"kept":true}}]}],"future_request":{"kept":true}}`
	provider, request := newNativeGeminiProviderForTest(t, server, raw, false)
	response, apiErr := provider.CreateGeminiChat(request)
	if apiErr != nil {
		t.Fatalf("native Gemini call failed: %+v", apiErr)
	}
	if !bytes.Equal(requestWire, []byte(raw)) {
		t.Fatalf("native Gemini request changed:\nwant %s\n got %s", raw, requestWire)
	}
	if got := response.ReplayProviderRawJSON(); !bytes.Equal(got, responseWire) {
		t.Fatalf("native Gemini response was reconstructed:\nwant %s\n got %s", responseWire, got)
	}
}

func TestNativeGeminiFutureResponseUnionKeepsRawAndUsage(t *testing.T) {
	raw := []byte(`{ "modelVersion":"gemini-test-actual", "candidates":{"future":true}, "usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}, "future":1e3 }`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer server.Close()
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })
	provider, request := newNativeGeminiProviderForTest(t, server, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, false)
	response, apiErr := provider.CreateGeminiChat(request)
	if apiErr != nil || response == nil || !bytes.Equal(response.ReplayProviderRawJSON(), raw) {
		t.Fatalf("future union blocked native raw delivery: response=%+v err=%+v", response, apiErr)
	}
	if usage := provider.GetUsage(); usage.PromptTokens != 3 || usage.CompletionTokens != 2 || !usage.ProviderReported {
		t.Fatalf("independent provider usage was lost: %+v", usage)
	}
}

func TestNativeGeminiHTTPErrorPreservesWireAndRetryHeader(t *testing.T) {
	errorWire := []byte(`{"error":{"code":429,"message":"slow down","status":"RESOURCE_EXHAUSTED","details":[{"reason":"quota"}],"future":{"kept":true}}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "9")
		w.Header().Set("X-Request-Id", "req-gemini")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write(errorWire)
	}))
	t.Cleanup(server.Close)
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })
	provider, request := newNativeGeminiProviderForTest(t, server, `{"contents":[]}`, false)
	_, apiErr := provider.CreateGeminiChat(request)
	if apiErr == nil || !apiErr.ReplayRawResponse || apiErr.StatusCode != http.StatusTooManyRequests || !bytes.Equal(apiErr.RawBody, errorWire) {
		t.Fatalf("native Gemini error wire changed: %+v", apiErr)
	}
	if apiErr.ResponseHeaders.Get("Retry-After") != "9" || apiErr.ResponseHeaders.Get("X-Request-Id") != "req-gemini" {
		t.Fatalf("native Gemini retry/diagnostic headers lost: %v", apiErr.ResponseHeaders)
	}
}

func TestNativeGeminiStreamPreservesProviderErrorAndRequiresFinishReason(t *testing.T) {
	for _, test := range []struct {
		name      string
		wire      string
		wantError func(error) bool
	}{
		{name: "missing finish reason", wire: "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]},\"index\":0}]}\n\n", wantError: func(err error) bool { return errors.Is(err, requester.ErrStreamProtocolTerminalMissing) }},
		{name: "provider error", wire: "data: {\"error\":{\"code\":503,\"message\":\"failed\",\"status\":\"UNAVAILABLE\"}}\n\n", wantError: func(err error) bool { return err != nil && !errors.Is(err, io.EOF) }},
		{name: "finish reason", wire: "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\",\"index\":0}],\"future\":{\"kept\":true}}\n\n", wantError: func(err error) bool { return errors.Is(err, io.EOF) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.wire)
			}))
			t.Cleanup(server.Close)
			originalHTTPClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })
			provider, request := newNativeGeminiProviderForTest(t, server, `{"contents":[]}`, true)
			stream, apiErr := provider.CreateGeminiChatStream(request)
			if apiErr != nil {
				t.Fatalf("open Gemini stream: %+v", apiErr)
			}
			data, streamErrors := stream.Recv()
			defer stream.Close()
			select {
			case got := <-data:
				if got != test.wire {
					t.Fatalf("Gemini SSE wire changed:\nwant %q\n got %q", test.wire, got)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for Gemini event")
			}
			select {
			case err := <-streamErrors:
				if !test.wantError(err) {
					t.Fatalf("unexpected Gemini terminal error: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for Gemini terminal")
			}
		})
	}
}
