package openai

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"
)

func collectNativeSSE(t *testing.T, stream requester.StreamReaderInterface[string]) (string, error) {
	t.Helper()
	defer requester.CloseAndDrainStream(stream)
	data, errs := stream.Recv()
	var output strings.Builder
	var terminal error
	deadline := time.After(3 * time.Second)
	for data != nil || errs != nil {
		select {
		case event, ok := <-data:
			if !ok {
				data = nil
			} else {
				output.WriteString(event)
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
			} else {
				terminal = err
			}
		case <-deadline:
			t.Fatal("native SSE did not finish")
		}
	}
	return output.String(), terminal
}

func TestCompatibleNativeSSEPreservesCompleteEvents(t *testing.T) {
	wire := ": comment\r\nid: 007\r\nretry: 1200\r\nevent: extension\r\ndata: {\"future\":\r\ndata: 12345678901234567890}\r\n\r\n" +
		"data:[DONE]\n\n" + "event: future_after_done\ndata: {\"opaque\":true}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8,\"unknown\":true}}\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	}))
	defer server.Close()
	oldClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	defer func() { requester.HTTPClient = oldClient }()
	for _, completion := range []bool{false, true} {
		name := "chat"
		if completion {
			name = "completion"
		}
		t.Run(name, func(t *testing.T) {
			proxy := ""
			p := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "test", Proxy: &proxy}, server.URL)
			p.Usage = &types.Usage{}
			p.UsageHandler = func(usage *types.Usage) bool { usage.PromptTokens = 30; return true }
			var stream requester.StreamReaderInterface[string]
			var apiErr *types.OpenAIErrorWithStatusCode
			if completion {
				stream, apiErr = p.CreateCompletionStream(&types.CompletionRequest{Model: "model", Prompt: "hello", Stream: true, StreamOptions: &types.StreamOptions{IncludeUsage: true}})
			} else {
				stream, apiErr = p.CreateChatCompletionStream(&types.ChatCompletionRequest{Model: "model", Stream: true, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}}, StreamOptions: &types.StreamOptions{IncludeUsage: true}})
			}
			if apiErr != nil {
				t.Fatalf("create: %+v", apiErr)
			}
			if !requester.IsRawSSEEventStream(stream) {
				t.Fatal("native compatible events lost frame marker")
			}
			got, err := collectNativeSSE(t, stream)
			if got != wire || !errors.Is(err, io.EOF) {
				t.Fatalf("wire changed: %q error=%v", got, err)
			}
			if p.Usage.TotalTokens != 8 {
				t.Fatalf("usage after done not observed: %+v", p.Usage)
			}
		})
	}
}

type nativeSSEReadFailure struct{ io.Reader }

func (r nativeSSEReadFailure) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func TestNativeSSEDoneDoesNotHideTransportFailure(t *testing.T) {
	wire := "data: [DONE]\n\nevent: extension\ndata: opaque\n\n"
	handler := &OpenAIStreamHandler{Usage: &types.Usage{}}
	stream, apiErr := requester.RequestRawSSEEventStreamWithEmitterOptions(nil, &http.Response{Body: io.NopCloser(nativeSSEReadFailure{strings.NewReader(wire)})}, handler.HandleExactChatSSE, requester.StreamReadOptions{})
	if apiErr != nil {
		t.Fatalf("create: %+v", apiErr)
	}
	got, err := collectNativeSSE(t, stream)
	if got != wire || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got=%q error=%v", got, err)
	}
}

func TestNativeSSEHidesOnlyUnrequestedUsageChunk(t *testing.T) {
	unknown := "event: extension\ndata: {\"choices\":{\"future\":true},\"usage\":{\"total_tokens\":2}}\n\n"
	usage := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n"
	done := "data: [DONE]\n\n"
	handler := &OpenAIStreamHandler{Usage: &types.Usage{}, hideProviderUsage: true}
	got, err := collectNativeSSE(t, newExactChatTestStream(t, handler, unknown+usage+done))
	if got != unknown+done || !errors.Is(err, io.EOF) {
		t.Fatalf("hidden usage changed other frames: %q error=%v", got, err)
	}
	if handler.Usage.TotalTokens != 5 {
		t.Fatalf("hidden usage not observed: %+v", handler.Usage)
	}
}

func TestChatConversionOwnsTerminalBoundaryWithoutChangingNativeDelivery(t *testing.T) {
	for _, marker := range []string{
		"data: [DONE]\n\n",
		"event: error\ndata: {\"error\":{\"type\":\"invalid_request_error\",\"code\":\"invalid_value\"}}\n\n",
	} {
		late := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":9,\"total_tokens\":18}}\n\n"
		for _, conversion := range []bool{false, true} {
			t.Run(fmt.Sprintf("conversion=%t marker=%s", conversion, marker), func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, marker+late)
				}))
				defer server.Close()
				old := requester.HTTPClient
				requester.HTTPClient = server.Client()
				defer func() { requester.HTTPClient = old }()
				proxy := ""
				p := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "test", Proxy: &proxy}, server.URL)
				p.SetUsage(&types.Usage{})
				req := &types.ChatCompletionRequest{Model: "model", Stream: true, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}}, StreamOptions: &types.StreamOptions{IncludeUsage: true}}
				var stream requester.StreamReaderInterface[string]
				var apiErr *types.OpenAIErrorWithStatusCode
				if conversion {
					stream, apiErr = p.CreateChatCompletionStreamForConversion(req)
				} else {
					stream, apiErr = p.CreateChatCompletionStream(req)
				}
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				wire, streamErr := collectNativeSSE(t, stream)
				if calls != 1 {
					t.Fatalf("provider calls=%d", calls)
				}
				if conversion {
					if wire != marker || p.Usage.TotalTokens != 0 || streamErr == nil {
						t.Fatalf("conversion crossed terminal: wire=%q usage=%+v err=%v", wire, p.Usage, streamErr)
					}
				} else if wire != marker+late || p.Usage.TotalTokens != 18 || !errors.Is(streamErr, io.EOF) {
					t.Fatalf("native delivery became a conversion: wire=%q usage=%+v err=%v", wire, p.Usage, streamErr)
				}
			})
		}
	}
}

func TestNativeUsageHidingCannotHideProviderError(t *testing.T) {
	wire := "event: error\ndata: {\"error\":{\"message\":\"rejected\"},\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n"
	h := &OpenAIStreamHandler{Usage: &types.Usage{}, hideProviderUsage: true}
	got, err := collectNativeSSE(t, newExactChatTestStream(t, h, wire))
	if got != wire || !errors.Is(err, io.EOF) || h.Usage.HasProviderUsage() {
		t.Fatalf("error hidden or billed: wire=%q err=%v usage=%+v", got, err, h.Usage)
	}
}
