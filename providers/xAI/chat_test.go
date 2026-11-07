package xAI

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"
)

func TestChatStreamRequestsUsageAndPreservesClientWire(t *testing.T) {
	for _, test := range []struct{ name, option string }{
		{"usage omitted", ""},
		{"usage disabled", `,"stream_options":{"include_usage":false,"future_option":123}`},
		{"usage requested", `,"stream_options":{"include_usage":true,"future_option":123}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			option := test.option
			raw := `{"model":"grok-4.7","stream":true,"reasoning_effort":"xhigh","store":false,"future_field":{"n":9007199254740993},"messages":[{"role":"developer","content":"hi","future_message":true}]` + option + `}`
			var sent map[string]json.RawMessage
			usageEvent := `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":10,"completion_tokens_details":{"reasoning_tokens":2,"future_detail":true},"future_usage":true}}` + "\n\n"
			contentEvent := `data: {"choices":[{"index":0,"delta":{"reasoning_content":"thinking","future_delta":true}}],"future_response":123}` + "\n\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
					t.Errorf("读取上游请求：%v", err)
				}
				var opts struct {
					IncludeUsage bool `json:"include_usage"`
				}
				_ = json.Unmarshal(sent["stream_options"], &opts)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, contentEvent)
				if opts.IncludeUsage {
					_, _ = io.WriteString(w, usageEvent)
				}
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer server.Close()
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			defer func() { requester.HTTPClient = previousClient }()
			proxy, url := "", server.URL
			provider := XAIProviderFactory{}.Create(&model.Channel{Type: config.ChannelTypeXAI, Proxy: &proxy, BaseURL: &url, Key: "test"}).(*XAIProvider)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(raw))
			if _, err := common.CacheRequestBody(c); err != nil {
				t.Fatal(err)
			}
			provider.SetContext(c)
			provider.SetUsage(&types.Usage{})
			var request types.ChatCompletionRequest
			if err := json.Unmarshal([]byte(raw), &request); err != nil {
				t.Fatal(err)
			}
			// 模拟 relay 的内部投影；不应向上游增加客户端未发送的 reasoning 对象。
			request.NormalizeReasoning()
			stream, apiErr := provider.CreateChatCompletionStream(&request)
			if apiErr != nil {
				t.Fatalf("创建流：%+v", apiErr)
			}
			defer requester.CloseAndDrainStream(stream)
			data, errs := stream.Recv()
			var output strings.Builder
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
					} else if !errors.Is(err, io.EOF) {
						t.Fatalf("读取流：%v", err)
					}
				case <-deadline:
					t.Fatal("等待流结束超时")
				}
			}
			var opts map[string]json.RawMessage
			if err := json.Unmarshal(sent["stream_options"], &opts); err != nil || string(opts["include_usage"]) != "true" {
				t.Fatalf("未请求上游 usage：%s", sent["stream_options"])
			}
			if option != "" && string(opts["future_option"]) != "123" {
				t.Fatalf("丢失流式扩展字段：%s", sent["stream_options"])
			}
			if string(sent["reasoning_effort"]) != `"xhigh"` || sent["reasoning"] != nil || string(sent["future_field"]) != `{"n":9007199254740993}` || !strings.Contains(string(sent["messages"]), `"future_message":true`) {
				t.Fatalf("客户端 wire 被改变：%s", sent)
			}
			if !strings.Contains(output.String(), contentEvent) {
				t.Fatalf("响应扩展字段丢失：%s", output.String())
			}
			exposed := strings.Contains(output.String(), `"future_usage":true`)
			if exposed != strings.Contains(option, `"include_usage":true`) {
				t.Fatalf("usage 暴露与客户端约定不一致：%s", output.String())
			}
			if exposed && (!strings.Contains(output.String(), `"completion_tokens":7`) || !strings.Contains(output.String(), `"future_detail":true`) || !strings.Contains(output.String(), `"reasoning_tokens":2`)) {
				t.Fatalf("推理用量未按 OpenAI 口径输出或丢失扩展：%s", output.String())
			}
			if usage := provider.GetUsage(); !usage.HasProviderUsage() || usage.TotalTokens != 10 || usage.CompletionTokens != 7 {
				t.Fatalf("缺失计费用量证据：%+v", usage)
			}
		})
	}
}

func TestChatWireDoesNotEnableOtherResourceProtocols(t *testing.T) {
	proxy := ""
	provider := XAIProviderFactory{}.Create(&model.Channel{Type: config.ChannelTypeXAI, Proxy: &proxy}).(*XAIProvider)
	if provider.SupportsImageResponse() || provider.SupportsStoredChat() {
		t.Fatal("Chat 透传不应开启图片流或存储资源能力")
	}
	if _, err := provider.BuildRawRelayURL("/v1/chat/completions/id", ""); err == nil {
		t.Fatal("Chat 透传不应开启资源转发能力")
	}
}

func TestChatCompletionNormalizesUsageAndPreservesResponseExtensions(t *testing.T) {
	raw := `{"id":"chat-1","model":"grok-4.7","choices":[{"message":{"role":"assistant","content":"OK","future_message":true}}],"future_response":9007199254740993,"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":10,"completion_tokens_details":{"reasoning_tokens":2,"future_detail":true},"cost_in_usd_ticks":123,"future_usage":true}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, raw)
	}))
	defer server.Close()
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	defer func() { requester.HTTPClient = previousClient }()
	proxy, url := "", server.URL
	p := XAIProviderFactory{}.Create(&model.Channel{Type: config.ChannelTypeXAI, Proxy: &proxy, BaseURL: &url}).(*XAIProvider)
	p.SetUsage(&types.Usage{})
	response, apiErr := p.CreateChatCompletion(&types.ChatCompletionRequest{Model: "grok-4.7", Messages: []types.ChatCompletionMessage{{Role: "user", Content: "Hi"}}})
	if apiErr != nil {
		t.Fatalf("非流式请求：%+v", apiErr)
	}
	encoded := response.ReplayProviderRawJSON()
	for _, want := range []string{`"completion_tokens":7`, `"reasoning_tokens":2`, `"future_detail":true`, `"future_message":true`, `"future_response":9007199254740993`, `"cost_in_usd_ticks":123`, `"future_usage":true`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("规范化丢失 %s：%s", want, encoded)
		}
	}
	if !p.GetUsage().HasProviderUsage() || p.GetUsage().CompletionTokens != 7 {
		t.Fatalf("内部用量证据丢失：%+v", p.GetUsage())
	}
}

func TestChatCompletionKeepsSuccessfulResponseWhenUsageProjectionCannotParseExtensions(t *testing.T) {
	for _, test := range []struct{ name, extension string }{
		{"deep opaque extension", `"future":` + strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65)},
		{"duplicate opaque extension", `"future":0,"future":1`},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := `{"id":"chat-1","model":"grok-4.7","choices":[{"message":{"role":"assistant","content":"OK"}}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":10,"completion_tokens_details":{"reasoning_tokens":2}},` + test.extension + `}`
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, raw)
			}))
			defer server.Close()
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			defer func() { requester.HTTPClient = previousClient }()
			proxy, url := "", server.URL
			p := XAIProviderFactory{}.Create(&model.Channel{Type: config.ChannelTypeXAI, Proxy: &proxy, BaseURL: &url}).(*XAIProvider)
			p.SetUsage(&types.Usage{})
			response, apiErr := p.CreateChatCompletion(&types.ChatCompletionRequest{Model: "grok-4.7", Messages: []types.ChatCompletionMessage{{Role: "user", Content: "Hi"}}})
			if apiErr != nil {
				t.Fatalf("用量投影改变了成功响应：%+v", apiErr)
			}
			if got := string(response.ReplayProviderRawJSON()); got != raw {
				t.Fatalf("未原样回放上游成功响应：%s", got)
			}
			if calls.Load() != 1 {
				t.Fatalf("成功响应被重复执行：%d", calls.Load())
			}
			if u := p.GetUsage(); !u.HasProviderBaseUsage() || u.CompletionTokens != 7 {
				t.Fatalf("内部计费证据被响应投影失败清除：%+v", u)
			}
		})
	}
}

func TestChatStreamKeepsCompleteUsageAfterSparseTail(t *testing.T) {
	for _, tail := range []string{`{}`, `{"prompt_tokens":3}`, `{"prompt_tokens":3,"completion_tokens":5}`} {
		t.Run(tail, func(t *testing.T) {
			wire := `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":10,"completion_tokens_details":{"reasoning_tokens":2}}}` + "\n\n" + `data: {"choices":[],"usage":` + tail + `,"future":true}` + "\n\ndata: [DONE]\n\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, wire)
			}))
			defer server.Close()
			previous := requester.HTTPClient
			requester.HTTPClient = server.Client()
			defer func() { requester.HTTPClient = previous }()
			proxy, url := "", server.URL
			p := XAIProviderFactory{}.Create(&model.Channel{Type: config.ChannelTypeXAI, Proxy: &proxy, BaseURL: &url}).(*XAIProvider)
			p.SetUsage(&types.Usage{})
			stream, apiErr := p.CreateChatCompletionStream(&types.ChatCompletionRequest{Model: "grok-4.7", Stream: true, StreamOptions: &types.StreamOptions{IncludeUsage: true}})
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			defer requester.CloseAndDrainStream(stream)
			data, errs := stream.Recv()
			var output strings.Builder
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
					} else if !errors.Is(err, io.EOF) {
						t.Fatal(err)
					}
				case <-deadline:
					t.Fatal("等待累计用量流结束超时")
				}
			}
			if !strings.Contains(output.String(), `"usage":`+tail+`,"future":true`) {
				t.Fatalf("稀疏上游帧未原样交付：%s", output.String())
			}
			if u := p.GetUsage(); !u.HasProviderUsage() || u.PromptTokens != 3 || u.CompletionTokens != 7 || u.TotalTokens != 10 {
				t.Fatalf("稀疏尾帧抹掉合法计费用量：%+v", u)
			}
		})
	}
}
