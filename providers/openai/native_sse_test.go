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

	"github.com/gin-gonic/gin"
	"one-api/common"
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
			p.UsageHandler = func(usage *types.Usage) bool { usage.PromptTokens = 30; return false }
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
	handler := &OpenAIStreamHandler{Usage: &types.Usage{}, usageFilter: hideCollectedUsage}
	got, err := collectNativeSSE(t, newExactChatTestStream(t, handler, unknown+usage+done))
	if got != unknown+done || !errors.Is(err, io.EOF) {
		t.Fatalf("hidden usage changed other frames: %q error=%v", got, err)
	}
	if handler.Usage.TotalTokens != 5 {
		t.Fatalf("hidden usage not observed: %+v", handler.Usage)
	}
}

func TestNativeSSEOnlyDecodesBOMAtStreamStart(t *testing.T) {
	const bom = "\uFEFF"
	const usage = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\r\n\r\n"
	const content = "data: {\"choices\":[{\"text\":\"hello\"}],\"usage\":null}\r\n\r\n"
	const filteredContent = "data: {\"choices\":[{\"text\":\"hello\"}]}\r\n\r\n"
	const heartbeat = ": heartbeat\r\n\r\n"
	const done = "data: [DONE]\r\n\r\n"
	tests := []struct {
		name, input, want string
		wantTokens        int
	}{
		{"vendor event", bom + "event: vendor.notice\r\n" + usage, bom + "event: vendor.notice\r\n" + usage, 5},
		{"named error", bom + "event: error\r\n" + usage, bom + "event: error\r\n" + usage, 5},
		{"content with null usage", bom + content + done, bom + filteredContent + done, 0},
		{"hidden first frame", bom + usage + done, bom + done, 5},
		{"blank first line", bom + "\r\n" + usage + done, bom + "\r\n" + done, 5},
		{"literal after heartbeat", heartbeat + bom + usage + done, heartbeat + bom + usage + done, 0},
		{"literal moved to start by filtering", usage + bom + content + done, bom + bom + content + done, 5},
		{"second BOM is literal", bom + bom + content + done, bom + bom + content + done, 0},
	}
	for _, completion := range []bool{false, true} {
		for _, tc := range tests {
			t.Run(fmt.Sprintf("completion=%t/%s", completion, tc.name), func(t *testing.T) {
				h := &OpenAIStreamHandler{Usage: &types.Usage{}, usageFilter: hideCollectedUsage}
				handle := h.HandleExactChatSSE
				if completion {
					handle = h.handleExactCompletionSSE
				}
				resp := &http.Response{Body: io.NopCloser(strings.NewReader(tc.input))}
				stream, apiErr := requester.RequestRawSSEEventStreamWithEmitterOptions(nil, resp, handle, requester.StreamReadOptions{})
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				got, err := collectNativeSSE(t, stream)
				if got != tc.want || !errors.Is(err, io.EOF) || h.Usage.TotalTokens != tc.wantTokens {
					t.Fatalf("流式交付或用量错误: want=%q got=%q wantTokens=%d usage=%+v err=%v", tc.want, got, tc.wantTokens, h.Usage, err)
				}
			})
		}
	}
}

func TestNativeSSEPreservesProviderUsageWithoutKnownCollectionAndPreference(t *testing.T) {
	const usage = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n"
	const done = "data: [DONE]\n\n"
	const disabled = `{"include_usage":false}`
	const opaque = `["future"]`
	for _, test := range []struct {
		name, original, final string
		support               bool
	}{
		{"opaque client options", opaque, opaque, true},
		{"opaque effective options", disabled, opaque, true},
		{"opaque client switch overridden by channel", `{"include_usage":{"future":true}}`, `{"include_usage":true}`, true},
		{"collection unavailable", disabled, disabled, false},
	} {
		for _, completion := range []bool{false, true} {
			t.Run(fmt.Sprintf("completion=%v/%s", completion, test.name), func(t *testing.T) {
				const prefix = `{"model":"model","stream":true,"stream_options":`
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(prefix+test.original+"}"))
				if _, err := common.CacheRequestBody(ctx); err != nil {
					t.Fatal(err)
				}
				final := prefix + test.final + "}"
				common.SetReusableRequestBody(ctx, []byte(final))
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil || string(body) != final {
						t.Errorf("上游请求选项变化：got=%q want=%q err=%v", body, final, err)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, usage+done)
				}))
				defer server.Close()
				previous := requester.HTTPClient
				requester.HTTPClient = server.Client()
				defer func() { requester.HTTPClient = previous }()
				proxy := ""
				p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy}, server.URL)
				p.SetProviderRawJSONReplay(true)
				p.SetContext(ctx)
				p.SetUsage(&types.Usage{})
				p.SupportStreamOptions = test.support
				var stream requester.StreamReaderInterface[string]
				var apiErr *types.OpenAIErrorWithStatusCode
				if completion {
					stream, apiErr = p.CreateCompletionStream(&types.CompletionRequest{Model: "model", Stream: true})
				} else {
					stream, apiErr = p.CreateChatCompletionStream(&types.ChatCompletionRequest{Model: "model", Stream: true})
				}
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				got, err := collectNativeSSE(t, stream)
				if got != usage+done || !errors.Is(err, io.EOF) || p.Usage.TotalTokens != 5 {
					t.Fatalf("上游用量交付或观察错误：wire=%q usage=%+v err=%v", got, p.Usage, err)
				}
			})
		}
	}
}

func TestCompatibleNativeSSEPreservesUsageAlongsideChoices(t *testing.T) {
	const usage = `"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}`
	const tail = "data: {\"choices\":[]," + usage + "}\n\n"
	const done = "data: [DONE]\n\n"
	for _, test := range []struct {
		name, choice string
	}{
		{"chat", `{"index":0,"delta":{"content":""},"finish_reason":"stop"}`},
		{"completion", `{"index":0,"text":"hello","finish_reason":null}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			choice := "data: {\"choices\":[" + test.choice + "]," + usage + "}\n\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, choice+tail+done)
			}))
			t.Cleanup(server.Close)
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			proxy := ""
			p := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "test", Proxy: &proxy}, server.URL)
			p.SetUsage(&types.Usage{})
			var stream requester.StreamReaderInterface[string]
			var apiErr *types.OpenAIErrorWithStatusCode
			if test.name == "completion" {
				stream, apiErr = p.CreateCompletionStream(&types.CompletionRequest{Model: "model", Stream: true, Prompt: "hello"})
			} else {
				stream, apiErr = p.CreateChatCompletionStream(&types.ChatCompletionRequest{Model: "model", Stream: true, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}}})
			}
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			got, err := collectNativeSSE(t, stream)
			if want := choice + done; got != want || !errors.Is(err, io.EOF) {
				t.Fatalf("上游主动返回的正文用量或兼容尾帧交付错误：got=%q want=%q err=%v", got, want, err)
			}
			if p.Usage.TotalTokens != 5 || !p.Usage.HasProviderBaseUsage() {
				t.Fatalf("计费证据错误：%+v", p.Usage)
			}
		})
	}
}

func TestChatConversionOwnsTerminalBoundaryWithoutChangingNativeDelivery(t *testing.T) {
	for _, marker := range []string{
		"data: [DONE]\n\n",
		"\uFEFFdata: [DONE]\n\n",
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
					if wire != strings.TrimPrefix(marker, "\uFEFF") || p.Usage.TotalTokens != 0 || streamErr == nil {
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
	h := &OpenAIStreamHandler{Usage: &types.Usage{}, usageFilter: hideCollectedUsage}
	got, err := collectNativeSSE(t, newExactChatTestStream(t, h, wire))
	if got != wire || !errors.Is(err, io.EOF) || !h.Usage.HasProviderBaseUsage() || h.Usage.TotalTokens != 2 {
		t.Fatalf("error delivery or independent usage observation changed: wire=%q err=%v usage=%+v", got, err, h.Usage)
	}
}
