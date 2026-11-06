package codex

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/zstd"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/types"
)

func TestCodexCompactValidatesCompressedBodyBeforeAcceptingUsage(t *testing.T) {
	const response = `{"id":"resp_compact_integrity","object":"response.compaction","output":[{"id":"cmp_1","type":"compaction","encrypted_content":"ciphertext"}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
	for _, encoding := range []string{"gzip", "zstd"} {
		for _, corruption := range []string{"valid", "checksum", "truncated trailer"} {
			t.Run(encoding+"/"+corruption, func(t *testing.T) {
				var compressed bytes.Buffer
				var writer io.WriteCloser
				if encoding == "gzip" {
					writer = gzip.NewWriter(&compressed)
				} else {
					var err error
					writer, err = zstd.NewWriter(&compressed, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true))
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err := io.WriteString(writer, response); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				body := compressed.Bytes()
				switch corruption {
				case "checksum":
					checksumOffset := len(body) - 4
					if encoding == "gzip" {
						checksumOffset = len(body) - 8
					}
					body[checksumOffset] ^= 1
				case "truncated trailer":
					body = body[:len(body)-1]
				}
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Content-Encoding", encoding)
					_, _ = w.Write(body)
				}))
				defer server.Close()
				originalClient := requester.HTTPClient
				requester.HTTPClient = server.Client()
				defer func() { requester.HTTPClient = originalClient }()
				provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
				provider.Usage = &types.Usage{}
				provider.Channel.BaseURL = &server.URL
				result, apiErr := provider.CompactResponsesForTest(&types.OpenAIResponsesRequest{Model: "gpt-5", Input: "hello"})
				if calls.Load() != 1 {
					t.Fatalf("compressed response triggered %d requests, want exactly one", calls.Load())
				}
				if corruption == "valid" {
					if apiErr != nil || result == nil || result.ID != "resp_compact_integrity" {
						t.Fatalf("valid compressed response rejected: response_present=%v err=%+v", result != nil, apiErr)
					}
					if provider.Usage.PromptTokens != 3 || provider.Usage.CompletionTokens != 2 || provider.Usage.TotalTokens != 5 || !provider.Usage.HasProviderUsage() {
						t.Fatalf("valid compressed response lost usage: %+v", provider.Usage)
					}
					return
				}
				if result != nil || apiErr == nil || !apiErr.UpstreamAccepted || apiErr.UpstreamNotAttempted {
					t.Errorf("corrupt compressed response must fail after acceptance: response_present=%v err=%+v", result != nil, apiErr)
				}
				if provider.Usage.PromptTokens != 0 || provider.Usage.CompletionTokens != 0 || provider.Usage.TotalTokens != 0 || provider.Usage.HasProviderUsage() {
					t.Errorf("corrupt compressed response supplied trusted usage: input=%d output=%d total=%d provider_reported=%v", provider.Usage.PromptTokens, provider.Usage.CompletionTokens, provider.Usage.TotalTokens, provider.Usage.HasProviderUsage())
				}
			})
		}
	}
}

func TestCodexCompressedHTTPPreservesRequestAndResponseSemantics(t *testing.T) {
	const future = `{"number":9007199254740993,"nullable":null,"union":[false,{"future":true}]}`
	const response = `{"id":"resp_compressed","object":"response","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
	const errorBody = `{"error":{"message":"unsupported encoding","type":"invalid_request_error","code":"encoding_rejected","future":null}}`
	for _, test := range []struct {
		name     string
		mode     string
		encoding string
		status   int
		corrupt  bool
	}{
		{"unary gzip", "unary", "gzip", 200, false},
		{"stream zstd", "stream", "zstd", 200, false},
		{"compact gzip", "compact", "gzip", 200, false},
		{"compact zstd", "compact", "zstd", 200, false},
		{"bad request", "unary", "gzip", 400, false},
		{"unsupported encoding", "compact", "zstd", 415, false},
		{"broken stream", "unary", "gzip", 200, true},
		{"broken compact", "compact", "zstd", 200, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Content-Encoding") != "zstd" || r.Header.Get("Accept-Encoding") != "gzip, zstd" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("unexpected transport headers: %v", r.Header)
					w.WriteHeader(400)
					return
				}
				decoder, err := zstd.NewReader(r.Body, zstd.WithDecoderConcurrency(1))
				if err != nil {
					t.Error(err)
					return
				}
				defer decoder.Close()
				body, err := io.ReadAll(decoder)
				if err != nil {
					t.Error(err)
					return
				}
				var fields map[string]json.RawMessage
				if err = json.Unmarshal(body, &fields); err != nil || string(fields["future"]) != future {
					t.Errorf("unknown request fields changed: body=%s err=%v", body, err)
				}
				if test.mode == "compact" {
					if !strings.HasSuffix(r.URL.Path, "/compact") {
						t.Errorf("unexpected compact path %s", r.URL.Path)
					}
				} else if string(fields["stream"]) != "true" {
					t.Error("upstream stream control changed")
				}
				w.Header().Set("Content-Encoding", test.encoding)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "5")
				w.WriteHeader(test.status)
				if test.corrupt {
					_, _ = io.WriteString(w, "invalid compressed bytes")
					return
				}
				var writer io.WriteCloser
				if test.encoding == "gzip" {
					writer = gzip.NewWriter(w)
				} else {
					writer, err = zstd.NewWriter(w, zstd.WithEncoderConcurrency(1))
					if err != nil {
						t.Error(err)
						return
					}
				}
				defer writer.Close()
				if test.status >= 400 {
					_, _ = io.WriteString(writer, errorBody)
				} else if test.mode == "compact" {
					_, _ = io.WriteString(writer, response)
				} else {
					_, _ = io.WriteString(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"+response+"}\n\n")
				}
			}))
			defer server.Close()
			before := requester.HTTPClient
			requester.HTTPClient = server.Client()
			defer func() { requester.HTTPClient = before }()
			provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
			provider.Usage = &types.Usage{}
			provider.Channel.BaseURL = &server.URL
			provider.SetOpenAIErrorEnvelopeReplay(true)
			raw, _ := json.Marshal(strings.Repeat("context text ", 400))
			envelope, err := commonresponses.ParseRawEnvelope([]byte(`{"model":"gpt-5","input":` + string(raw) + `,"future":` + future + `}`))
			if err != nil {
				t.Fatal(err)
			}
			req := &commonresponses.Request{Operation: commonresponses.ResponsesCreate, Model: "gpt-5", Body: envelope}
			var result *types.OpenAIResponsesResponses
			var apiErr *types.OpenAIErrorWithStatusCode
			switch test.mode {
			case "compact":
				req.Operation = commonresponses.ResponsesCompact
				result, apiErr = provider.CompactResponses(context.Background(), req)
			case "stream":
				var stream commonresponses.EventStream
				stream, apiErr = provider.CreateResponsesStream(context.Background(), req)
				if apiErr == nil {
					defer requester.CloseAndDrainStream(stream)
					var frames strings.Builder
					data, failures := stream.Recv()
					for data != nil || failures != nil {
						select {
						case frame, ok := <-data:
							if !ok {
								data = nil
								continue
							}
							frames.WriteString(frame)
							if err := stream.ObserveResponsesEvent(frame); err != nil {
								t.Fatal(err)
							}
						case streamErr, ok := <-failures:
							if !ok {
								failures = nil
								continue
							}
							if streamErr != nil && streamErr != io.EOF {
								t.Fatal(streamErr)
							}
						}
					}
					if !strings.Contains(frames.String(), response) {
						t.Fatalf("response stream changed: %s", frames.String())
					}
				}
			default:
				result, apiErr = provider.CreateResponses(context.Background(), req)
			}
			if calls.Load() != 1 {
				t.Fatalf("compression triggered %d upstream requests", calls.Load())
			}
			if test.corrupt {
				if apiErr == nil || !apiErr.UpstreamAccepted || apiErr.UpstreamNotAttempted {
					t.Fatalf("decompression failure lost accepted execution: %+v", apiErr)
				}
			} else if test.status >= 400 {
				if apiErr == nil || apiErr.StatusCode != test.status || string(apiErr.RawBody) != errorBody || apiErr.ResponseHeaders.Get("Retry-After") != "5" || apiErr.ResponseHeaders.Get("Content-Encoding") != "" {
					t.Fatalf("compressed upstream error changed: %+v", apiErr)
				}
			} else {
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				if test.mode != "stream" && (result == nil || result.ID != "resp_compressed") {
					t.Fatalf("unexpected response: %+v", result)
				}
				if provider.Usage.PromptTokens != 3 || provider.Usage.CompletionTokens != 2 {
					t.Fatalf("compression changed usage: %+v", provider.Usage)
				}
			}
		})
	}
}
