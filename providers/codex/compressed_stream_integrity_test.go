package codex

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"one-api/common/requester"
	"one-api/types"
)

const codexCompressedStreamTerminal = "event: response.completed\ndata: " +
	`{"type":"response.completed","response":{"id":"resp_stream_integrity","object":"response","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}` + "\n\n"

func codexCompressedStreamFixture(t *testing.T, encoding, prefix, suffix string) ([]byte, int) {
	t.Helper()
	var buffer bytes.Buffer
	var writer interface {
		io.WriteCloser
		Flush() error
	}
	if encoding == "gzip" {
		writer = gzip.NewWriter(&buffer)
	} else {
		var err error
		writer, err = zstd.NewWriter(&buffer, zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := io.WriteString(writer, prefix); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	prefixBytes := buffer.Len()
	if _, err := io.WriteString(writer, suffix); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes(), prefixBytes
}

func codexCompressedStreamProvider(t *testing.T, server *httptest.Server) *CodexProvider {
	t.Helper()
	originalClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalClient })
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	provider.Usage = &types.Usage{}
	provider.Channel.BaseURL = &server.URL
	return provider
}

func TestCodexAggregatedResponseRequiresCompressedStreamIntegrity(t *testing.T) {
	testCodexCompressedStreamIntegrity(t, "aggregate")
}

func TestCodexNativeStreamRequiresCompressedStreamIntegrity(t *testing.T) {
	testCodexCompressedStreamIntegrity(t, "responses stream")
}

func TestCodexChatStreamRequiresCompressedStreamIntegrity(t *testing.T) {
	testCodexCompressedStreamIntegrity(t, "chat stream")
}

func testCodexCompressedStreamIntegrity(t *testing.T, mode string) {
	t.Helper()
	for _, encoding := range []string{"gzip", "zstd"} {
		for _, corruption := range []string{"checksum", "truncated trailer"} {
			for _, delivery := range []string{"single write", "separate trailer"} {
				t.Run(encoding+"/"+corruption+"/"+delivery, func(t *testing.T) {
					body, prefixBytes := codexCompressedStreamFixture(t, encoding, codexCompressedStreamTerminal, "")
					if corruption == "checksum" {
						offset := len(body) - 4
						if encoding == "gzip" {
							offset = len(body) - 8
						}
						body[offset] ^= 1
					} else {
						body = body[:len(body)-1]
					}
					prefixSent := make(chan struct{})
					releaseTrailer := make(chan struct{})
					var releaseOnce sync.Once
					release := func() { releaseOnce.Do(func() { close(releaseTrailer) }) }
					var calls atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						w.Header().Set("Content-Type", "text/event-stream")
						w.Header().Set("Content-Encoding", encoding)
						if delivery == "separate trailer" {
							_, _ = w.Write(body[:prefixBytes])
							w.(http.Flusher).Flush()
							close(prefixSent)
							select {
							case <-releaseTrailer:
							case <-r.Context().Done():
								return
							}
							_, _ = w.Write(body[prefixBytes:])
						} else {
							_, _ = w.Write(body)
						}
					}))
					defer server.Close()
					defer release()
					provider := codexCompressedStreamProvider(t, server)
					request, apiErr := provider.rawResponsesRequestForTest(&types.OpenAIResponsesRequest{Model: "gpt-5", Input: "hello", ConvertChat: mode == "chat stream"})
					if apiErr != nil {
						t.Fatal(apiErr)
					}
					ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
					defer cancel()
					type result struct {
						response  *types.OpenAIResponsesResponses
						err       *types.OpenAIErrorWithStatusCode
						streamErr error
						frames    string
					}
					done := make(chan result, 1)
					go func() {
						if mode == "aggregate" {
							response, err := provider.CreateResponses(ctx, request)
							done <- result{response: response, err: err}
							return
						}
						stream, err := provider.CreateResponsesStream(ctx, request)
						outcome := result{err: err}
						if err == nil {
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
										outcome.streamErr = err
									}
								case err, ok := <-failures:
									if !ok {
										failures = nil
										continue
									}
									if err != nil && err != io.EOF {
										outcome.streamErr = err
									}
								case <-ctx.Done():
									outcome.streamErr = ctx.Err()
									data, failures = nil, nil
								}
							}
							requester.CloseAndDrainStream(stream)
							outcome.frames = frames.String()
						}
						done <- outcome
					}()
					var got result
					finished := false
					if delivery == "separate trailer" {
						select {
						case <-prefixSent:
						case <-ctx.Done():
							t.Fatal("upstream did not flush the terminal event")
						}
						select {
						case got = <-done:
							finished = true
							t.Error("request completed before the compressed trailer arrived")
						case <-time.After(50 * time.Millisecond):
						}
						release()
					}
					if !finished {
						select {
						case got = <-done:
						case <-ctx.Done():
							t.Fatal("request did not finish after the compressed trailer was released")
						}
					}
					if err := ctx.Err(); err != nil {
						t.Fatalf("compressed integrity check did not finish before request cancellation: %v", err)
					}
					if mode == "aggregate" {
						if got.response != nil || got.err == nil || !got.err.UpstreamAccepted || got.err.UpstreamNotAttempted {
							t.Errorf("corrupt stream must fail after acceptance: response_present=%v err=%+v", got.response != nil, got.err)
						}
					} else {
						if got.err == nil && got.streamErr == nil {
							t.Error("corrupt compressed stream ended without an error")
						}
						if strings.Contains(got.frames, "response.completed") || strings.Contains(got.frames, "[DONE]") || strings.Contains(got.frames, `"finish_reason":"stop"`) {
							t.Errorf("corrupt compressed stream delivered a successful terminal: %s", got.frames)
						}
					}
					if provider.Usage.PromptTokens != 0 || provider.Usage.CompletionTokens != 0 || provider.Usage.TotalTokens != 0 || provider.Usage.HasProviderUsage() {
						t.Errorf("corrupt terminal supplied trusted usage: input=%d output=%d total=%d provider_reported=%v", provider.Usage.PromptTokens, provider.Usage.CompletionTokens, provider.Usage.TotalTokens, provider.Usage.HasProviderUsage())
					}
					if calls.Load() != 1 {
						t.Errorf("compressed stream triggered %d requests, want exactly one", calls.Load())
					}
				})
			}
		}
	}
}

func TestCodexCompressedStreamDeliversPrefixBeforeCompletion(t *testing.T) {
	const prefix = "event: response.created\ndata: " +
		`{"type":"response.created","response":{"id":"resp_stream_integrity","status":"in_progress"}}` + "\n\n" +
		"event: response.output_text.delta\ndata: " +
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}` + "\n\n"
	for _, scenario := range []struct {
		encoding    string
		convertChat bool
	}{{"gzip", false}, {"zstd", false}, {"gzip", true}, {"zstd", true}} {
		encoding := scenario.encoding
		name := encoding
		if scenario.convertChat {
			name += "/chat"
		}
		t.Run(name, func(t *testing.T) {
			body, prefixBytes := codexCompressedStreamFixture(t, encoding, prefix, codexCompressedStreamTerminal)
			releaseTerminal := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseTerminal) }) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Encoding", encoding)
				_, _ = w.Write(body[:prefixBytes])
				w.(http.Flusher).Flush()
				select {
				case <-releaseTerminal:
				case <-r.Context().Done():
					return
				}
				_, _ = w.Write(body[prefixBytes:])
			}))
			defer server.Close()
			defer release()
			provider := codexCompressedStreamProvider(t, server)
			request, apiErr := provider.rawResponsesRequestForTest(&types.OpenAIResponsesRequest{Model: "gpt-5", Input: "hello", ConvertChat: scenario.convertChat})
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			stream, apiErr := provider.CreateResponsesStream(ctx, request)
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			defer requester.CloseAndDrainStream(stream)
			data, failures := stream.Recv()
			deltaSeen, terminalSeen := false, false
			for data != nil || failures != nil {
				select {
				case frame, ok := <-data:
					if !ok {
						data = nil
						continue
					}
					if strings.Contains(frame, "response.output_text.delta") || scenario.convertChat && strings.Contains(frame, `"content":"hello"`) {
						deltaSeen = true
						release()
					}
					if strings.Contains(frame, "response.completed") || scenario.convertChat && strings.Contains(frame, `"finish_reason":"stop"`) {
						terminalSeen = true
					}
					if err := stream.ObserveResponsesEvent(frame); err != nil {
						t.Fatal(err)
					}
				case err, ok := <-failures:
					if !ok {
						failures = nil
						continue
					}
					if err != nil && err != io.EOF {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatalf("compressed stream did not progress: delta_seen=%v terminal_seen=%v", deltaSeen, terminalSeen)
				}
			}
			if !deltaSeen || !terminalSeen {
				t.Fatalf("stream lost events: delta_seen=%v terminal_seen=%v", deltaSeen, terminalSeen)
			}
			if provider.Usage.TotalTokens != 5 || !provider.Usage.HasProviderUsage() {
				t.Fatalf("verified terminal lost usage: %+v", provider.Usage)
			}
		})
	}
}
