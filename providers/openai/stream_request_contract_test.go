package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"
)

func TestChatCompletionStreamUsageFollowsClientOptions(t *testing.T) {
	for _, completion := range []bool{false, true} {
		for _, test := range []struct {
			name, original      string
			typedUsage, display bool
		}{
			{name: "client option omitted", original: `{"model":"model","stream":true}`, typedUsage: true},
			{name: "client disables usage", original: `{"model":"model","stream":true,"stream_options":{"include_usage":false}}`, typedUsage: true},
			{name: "client enables usage", original: `{"model":"model","stream":true,"stream_options":{"include_usage":true}}`, display: true},
			{name: "internal caller enables usage", typedUsage: true, display: true},
			{name: "internal caller disables usage"},
		} {
			t.Run(fmt.Sprintf("completion=%v/%s", completion, test.name), func(t *testing.T) {
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.original))
				if _, err := common.CacheRequestBody(ctx); err != nil {
					t.Fatal(err)
				}
				if test.original != "" {
					// 模拟渠道 pre_add 改写客户端的展示开关。
					mapped := fmt.Sprintf(`{"model":"model","stream":true,"stream_options":{"include_usage":%v}}`, test.typedUsage)
					common.SetReusableRequestBody(ctx, []byte(mapped))
				}
				var upstreamCalls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upstreamCalls.Add(1)
					body, _ := io.ReadAll(r.Body)
					var envelope struct {
						StreamOptions *types.StreamOptions `json:"stream_options"`
					}
					if json.Unmarshal(body, &envelope) != nil || envelope.StreamOptions == nil || !envelope.StreamOptions.IncludeUsage {
						t.Errorf("provider usage 未开启：%s", body)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
				}))
				defer server.Close()
				previous := requester.HTTPClient
				requester.HTTPClient = server.Client()
				defer func() { requester.HTTPClient = previous }()
				proxy := ""
				preAdd := fmt.Sprintf(`{"pre_add":true,"overwrite":true,"stream_options":{"include_usage":%v}}`, test.typedUsage)
				p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy, CustomParameter: &preAdd}, server.URL)
				p.Context, p.Usage = ctx, &types.Usage{}
				p.SupportStreamOptions = true
				options := &types.StreamOptions{IncludeUsage: test.typedUsage}
				var stream requester.StreamReaderInterface[string]
				var apiErr *types.OpenAIErrorWithStatusCode
				if completion {
					request := &types.CompletionRequest{Model: "model", Stream: true, StreamOptions: options}
					stream, apiErr = p.CreateCompletionStream(request)
					if request.StreamOptions != options {
						t.Fatal("client options 被替换")
					}
				} else {
					request := &types.ChatCompletionRequest{Model: "model", Stream: true, StreamOptions: options}
					stream, apiErr = p.CreateChatCompletionStream(request)
					if request.StreamOptions != options {
						t.Fatal("client options 被替换")
					}
				}
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				output, err := collectNativeSSE(t, stream)
				if !errors.Is(err, io.EOF) || strings.Contains(output, `"usage"`) != test.display || upstreamCalls.Load() != 1 || !p.Usage.HasProviderBaseUsage() || options.IncludeUsage != test.typedUsage {
					t.Fatalf("客户端原始展示意图或内部证据丢失：output=%q usage=%+v err=%v", output, p.Usage, err)
				}
			})
		}
	}
}

func TestNativeCompletionStreamSkipsTypedBodyMerge(t *testing.T) {
	const body = `{"model":"model","stream":true,"prompt":[[1234,5678]],"stream_options":{"include_usage":true,"future":{"n":9007199254740993}}}`
	const terminal = "data: [DONE]\n\n"
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		got, err := io.ReadAll(r.Body)
		if err != nil || string(got) != body {
			t.Errorf("原生请求正文变化：got=%q want=%q err=%v", got, body, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, terminal)
	}))
	defer upstream.Close()
	previous := requester.HTTPClient
	requester.HTTPClient = upstream.Client()
	defer func() { requester.HTTPClient = previous }()

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(body))
	common.SetReusableRequestBody(ctx, []byte(body))
	proxy := ""
	p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy, AllowExtraBody: true}, upstream.URL)
	p.SetProviderRawJSONReplay(true)
	p.SetContext(ctx)
	p.SetUsage(&types.Usage{})
	stream, apiErr := p.CreateCompletionStream(&types.CompletionRequest{
		Model: "model", Stream: true, StreamOptions: &types.StreamOptions{IncludeUsage: true},
	})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	got, err := collectNativeSSE(t, stream)
	if got != terminal || !errors.Is(err, io.EOF) || calls.Load() != 1 {
		t.Fatalf("流式交付错误：wire=%q err=%v providerCalls=%d", got, err, calls.Load())
	}
	cached, _ := ctx.Get(config.GinRequestBodyMapKey)
	if fields, _ := cached.(map[string]any); fields != nil {
		t.Fatal("原生请求构造缓存了仅供 typed 合并使用的正文 map")
	}
}

func TestChatCompletionPostCustomPreservesPreparedStreamMode(t *testing.T) {
	previous := requester.HTTPClient
	requester.HTTPClient = &http.Client{}
	defer func() { requester.HTTPClient = previous }()
	for _, completion := range []bool{false, true} {
		for _, native := range []bool{false, true} {
			for _, test := range []struct {
				name, custom string
				stream       bool
			}{
				{"stream disabled", `{"overwrite":true,"stream":false}`, true},
				{"stream removed", `{"remove_params":["stream"]}`, true},
				{"unary enabled", `{"overwrite":true,"stream":true}`, false},
			} {
				t.Run(fmt.Sprintf("completion=%v/native=%v/%s", completion, native, test.name), func(t *testing.T) {
					proxy := ""
					p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy, CustomParameter: &test.custom}, "https://unused.invalid")
					if native {
						ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
						body := fmt.Sprintf(`{"model":"model","stream":%v,"future":{"large":9007199254740993}}`, test.stream)
						ctx.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
						common.SetReusableRequestBody(ctx, []byte(body))
						p.Context = ctx
					}
					mode := config.RelayModeChatCompletions
					var typed any = &types.ChatCompletionRequest{Model: "model", Stream: test.stream}
					if completion {
						mode = config.RelayModeCompletions
						typed = &types.CompletionRequest{Model: "model", Stream: test.stream}
					}
					req, usageEnabled, apiErr := p.getRequestTextBody(mode, "model", func() any { return typed }, test.stream)
					if apiErr != nil {
						t.Fatal(apiErr)
					}
					defer req.Body.Close()
					body, _ := io.ReadAll(req.Body)
					var envelope struct {
						Stream  bool                 `json:"stream"`
						Options *types.StreamOptions `json:"stream_options"`
						Future  json.RawMessage      `json:"future"`
					}
					if json.Unmarshal(body, &envelope) != nil || envelope.Stream != test.stream || (envelope.Options != nil && envelope.Options.IncludeUsage) != test.stream || usageEnabled != test.stream {
						t.Fatalf("post custom changed prepared mode or usage eligibility: %s", body)
					}
					if native && string(envelope.Future) != `{"large":9007199254740993}` {
						t.Fatalf("unknown raw field changed: %s", body)
					}
				})
			}
		}
	}
}

func TestChatCompletionFutureStreamModeRemainsUpstreamOwned(t *testing.T) {
	previous := requester.HTTPClient
	requester.HTTPClient = &http.Client{}
	defer func() { requester.HTTPClient = previous }()
	for _, completion := range []bool{false, true} {
		for _, stream := range []string{`null`, `"future"`, `{"mode":"future"}`, `1`} {
			t.Run(fmt.Sprintf("completion=%v/stream=%s", completion, stream), func(t *testing.T) {
				body := `{"model":"model","stream":` + stream + `,"stream_options":null,"future":{"large":9007199254740993}}`
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
				common.SetReusableRequestBody(ctx, []byte(body))
				proxy := ""
				p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy}, "https://unused.invalid")
				p.Context = ctx
				mode := config.RelayModeChatCompletions
				var typed any = &types.ChatCompletionRequest{Model: "model", Stream: true}
				if completion {
					mode = config.RelayModeCompletions
					typed = &types.CompletionRequest{Model: "model", Stream: true}
				}
				req, usageEnabled, apiErr := p.getRequestTextBody(mode, "model", func() any { return typed }, true)
				if apiErr != nil {
					t.Fatal(apiErr)
				}
				defer req.Body.Close()
				got, err := io.ReadAll(req.Body)
				if err != nil || usageEnabled || string(got) != body {
					t.Fatalf("future stream 形态不应触发 mode 校验或 usage 注入：body=%s err=%v", got, err)
				}
			})
		}
	}
}

func TestChatCompletionIncludeUsageUnionPreservesUpstreamValidation(t *testing.T) {
	for _, completion := range []bool{false, true} {
		for _, include := range []string{`"future"`, `[]`, `{"mode":"future"}`, `1`} {
			t.Run(fmt.Sprintf("completion=%v/include=%s", completion, include), func(t *testing.T) {
				body := `{"model":"model","stream":true,"stream_options":{"include_usage":` + include + `,"future":{"large":9007199254740993}}}`
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					got, _ := io.ReadAll(r.Body)
					if string(got) != body {
						t.Errorf("future include_usage was overwritten: %s", got)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnprocessableEntity)
					_, _ = io.WriteString(w, `{"error":{"code":"invalid_usage","type":"invalid_request_error","param":"stream_options.include_usage","message":"future usage form rejected"}}`)
				}))
				defer server.Close()
				previous := requester.HTTPClient
				requester.HTTPClient = server.Client()
				defer func() { requester.HTTPClient = previous }()
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
				common.SetReusableRequestBody(ctx, []byte(body))
				proxy := ""
				p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy}, server.URL)
				p.Context, p.Usage = ctx, &types.Usage{}
				p.SupportStreamOptions = true
				p.SetProviderRawJSONReplay(true)
				p.SetOpenAIErrorEnvelopeReplay(true)
				var apiErr *types.OpenAIErrorWithStatusCode
				if completion {
					_, apiErr = p.CreateCompletionStream(&types.CompletionRequest{Model: "model", Stream: true})
				} else {
					_, apiErr = p.CreateChatCompletionStream(&types.ChatCompletionRequest{Model: "model", Stream: true})
				}
				if apiErr == nil || apiErr.StatusCode != http.StatusUnprocessableEntity || apiErr.Code != "invalid_usage" || !strings.HasSuffix(apiErr.Message, "future usage form rejected") || !apiErr.ReplayRawResponse || string(apiErr.RawBody) != `{"error":{"code":"invalid_usage","type":"invalid_request_error","param":"stream_options.include_usage","message":"future usage form rejected"}}` || calls.Load() != 1 {
					t.Fatalf("upstream validation semantics changed: err=%+v calls=%d", apiErr, calls.Load())
				}
			})
		}
	}
}

func TestProviderUsageMergeRequiresFinalStreamAndKnownBoolean(t *testing.T) {
	for _, stream := range []string{"", `,"stream":false`, `,"stream":null`, `,"stream":"future"`} {
		body := []byte(`{"model":"model","stream_options":null` + stream + `}`)
		got, enabled := mergeProviderStreamUsage(body)
		if enabled || got != nil {
			t.Fatalf("non-stream body mutated: %s", got)
		}
	}
	body := []byte(`{"stream":true,"stream_options":{"include_usage":null,"future":9007199254740993}}`)
	got, enabled := mergeProviderStreamUsage(body)
	if !enabled || !strings.Contains(string(got), `"include_usage":true`) || !strings.Contains(string(got), `"future":9007199254740993`) {
		t.Fatalf("nullable bool or raw sibling lost: %s", got)
	}
}

func TestProviderUsageMergePreservesSurroundingWireBytes(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{` { "messages" : [{ "n": 9.00E20 }], "\u0073tream" : true  } `, ` { "messages" : [{ "n": 9.00E20 }], "\u0073tream" : true,"stream_options":{"include_usage":true}  } `},
		{`{"stream_options" : null ,"stream":true}`, `{"stream_options" : {"include_usage":true} ,"stream":true}`},
		{`{"stream":true,"stream_options":{   }}`, `{"stream":true,"stream_options":{"include_usage":true   }}`},
		{`{"stream":true,"stream_options":{ "future": {"n":1,"n":2} }}`, `{"stream":true,"stream_options":{ "future": {"n":1,"n":2},"include_usage":true }}`},
		{`{"stream":true,"stream_\u006fptions":{"include_\u0075sage" : false,"n":9007199254740993}}`, `{"stream":true,"stream_\u006fptions":{"include_\u0075sage" : true,"n":9007199254740993}}`},
	} {
		original := []byte(tc.body)
		got, enabled := mergeProviderStreamUsage(original)
		if !enabled || string(got) != tc.want || string(original) != tc.body {
			t.Errorf("用量开关以外的字节改变：原文 %q，得到 %q，预期 %q", original, got, tc.want)
		}
	}
}

func TestProviderUsageMergeLeavesUnchangedAndAmbiguousBodiesInPlace(t *testing.T) {
	for _, tc := range []struct {
		body    string
		enabled bool
	}{
		{`{"stream":true,"stream_options":{"include_usage":true}}`, true},
		{`{"stream":true,"stream_options":["future"]}`, false},
		{`{"stream":true,"stream_options":{"include_usage":{"future":true}}}`, false},
		{`{"stream":true,"stream":false}`, false},
		{`{"stream":true,"stream_options":{"include_usage":true,"\u0069nclude_usage":false}}`, false},
		{`{"stream":true,"stream_options":{"future":1,"future":2}}`, false},
		{`{"stream":true,"future":[}`, false},
		{`{"stream":true} null`, false},
	} {
		original := []byte(tc.body)
		got, enabled := mergeProviderStreamUsage(original)
		if got != nil || enabled != tc.enabled || string(original) != tc.body {
			t.Errorf("预期沿用 %s、采集=%v，得到 %s、采集=%v", tc.body, tc.enabled, got, enabled)
		}
	}
}

func TestTypedStreamOptionsWithoutCanonicalOptions(t *testing.T) {
	proxy := ""
	p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeDeepseek, Proxy: &proxy, AllowExtraBody: true}, "https://unused.example")
	obfuscation := false
	options := &types.StreamOptions{IncludeObfuscation: &obfuscation}
	if p.streamOptionsForTypedBody(options, true) != options {
		t.Fatal("内部调用丢失 typed stream_options")
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"stream":true}`))
	p.Context = ctx
	if p.streamOptionsForTypedBody(options, true) != options {
		t.Fatal("原始 body 缺省 stream_options 时应保留内部 typed 选项")
	}
}
