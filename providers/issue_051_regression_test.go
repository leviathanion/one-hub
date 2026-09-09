package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/base"
	"one-api/providers/gemini"
	"one-api/providers/vertexai"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

const issue051VertexProject = "issue-051-gemini-project"

type issue051Upstream struct {
	stream bool
	kind   string
	calls  int
	bodies [][]byte
	server *httptest.Server
}

func newIssue051Upstream(t *testing.T, kind string, stream bool) *issue051Upstream {
	t.Helper()
	upstream := &issue051Upstream{kind: kind, stream: stream}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read %s upstream request: %v", kind, err)
			return
		}
		upstream.calls++
		upstream.bodies = append(upstream.bodies, append([]byte(nil), body...))

		w.Header().Set("Content-Type", "application/json")
		if kind == "gemini" {
			frame := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+frame+"\n\n")
				return
			}
			_, _ = io.WriteString(w, frame)
			return
		}

		if kind == "ali-native" {
			frame := `{"request_id":"issue-051-ali","output":{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]},"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+frame+"\n\n")
				return
			}
			_, _ = io.WriteString(w, frame)
			return
		}

		frame := `{"id":"issue-051-compatible","object":"chat.completion","model":"qwen-plus","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+frame+"\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, frame)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func issue051Context(t *testing.T, path, raw string) *gin.Context {
	t.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(raw))
	ctx.Request.Header.Set("Content-Type", "application/json")
	return ctx
}

func issue051DrainStream(t *testing.T, stream requester.StreamReaderInterface[string]) {
	t.Helper()
	if stream == nil {
		return
	}
	data, errors := stream.Recv()
	defer stream.Close()
	for data != nil || errors != nil {
		select {
		case _, ok := <-data:
			if !ok {
				data = nil
			}
		case err, ok := <-errors:
			if !ok {
				errors = nil
				continue
			}
			if err != nil && err != io.EOF {
				t.Fatalf("provider stream failed: %v", err)
			}
		}
	}
}

func issue051GeminiProvider(t *testing.T, upstream *issue051Upstream, vertex bool, raw string, stream bool) (base.ProviderInterface, *gemini.GeminiChatRequest) {
	t.Helper()
	ctx := issue051Context(t, "/gemini/v1beta/models/gemini-2.5-flash:generateContent", raw)
	request := &gemini.GeminiChatRequest{}
	if err := common.UnmarshalBodyReusable(ctx, request); err != nil {
		t.Fatalf("decode Gemini native request: %v", err)
	}
	request.Model = "gemini-2.5-flash"
	request.Stream = stream

	proxy := ""
	channel := &model.Channel{
		Type:    config.ChannelTypeGemini,
		Key:     "issue-051-key",
		Proxy:   &proxy,
		BaseURL: func() *string { value := upstream.server.URL; return &value }(),
	}
	if vertex {
		channel.Type = config.ChannelTypeVertexAI
		baseURL := upstream.server.URL + "/%s/%s/%s/%s:%s"
		channel.BaseURL = &baseURL
		channel.Other = `{"region":"global","project_id":"` + issue051VertexProject + `"}`
		cache.InitCacheManager()
		cacheKey := geminiTokenCacheKey()
		if err := cache.SetCache(cacheKey, "issue-051-token", time.Minute); err != nil {
			t.Fatalf("seed Vertex token cache: %v", err)
		}
		t.Cleanup(func() { _ = cache.DeleteCache(cacheKey) })
	}

	provider := createProvider(channel)
	if provider == nil {
		t.Fatal("native Gemini factory did not create a provider")
	}
	provider.SetContext(ctx)
	provider.SetOriginalModel(request.Model)
	provider.SetUsage(&types.Usage{})
	return provider, request
}

func geminiTokenCacheKey() string {
	return vertexai.TokenCacheKey + ":" + issue051VertexProject
}

func TestIssue051GeminiNativeSearchAliasesAreRejectedBeforeUpstream(t *testing.T) {
	for _, vertex := range []bool{false, true} {
		providerName := "Gemini"
		if vertex {
			providerName = "Vertex"
		}
		for _, stream := range []bool{false, true} {
			for _, field := range []string{"googleSearch", "google_search"} {
				name := fmt.Sprintf("%s/%s/%s", providerName, map[bool]string{false: "unary", true: "SSE"}[stream], field)
				t.Run(name, func(t *testing.T) {
					upstream := newIssue051Upstream(t, "gemini", stream)
					previousClient := requester.HTTPClient
					requester.HTTPClient = upstream.server.Client()
					t.Cleanup(func() { requester.HTTPClient = previousClient })
					raw := fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"text":"search"}]}],"tools":[{"%s":{}}]}`, field)
					provider, request := issue051GeminiProvider(t, upstream, vertex, raw, stream)
					native := provider.(gemini.GeminiChatInterface)
					if stream {
						streamResponse, apiErr := native.CreateGeminiChatStream(request)
						if apiErr == nil || apiErr.Code != "gemini_grounding_billing_unsupported" {
							t.Fatalf("unpriced %s search was not rejected: err=%+v", field, apiErr)
						}
						if streamResponse != nil {
							t.Fatal("rejected Gemini search returned a provider stream")
						}
					} else {
						response, apiErr := native.CreateGeminiChat(request)
						if apiErr == nil || apiErr.Code != "gemini_grounding_billing_unsupported" || response != nil {
							t.Fatalf("unpriced %s search was not rejected: response=%+v err=%+v", field, response, apiErr)
						}
					}
					if upstream.calls != 0 {
						t.Fatalf("rejected %s search reached upstream %d times", field, upstream.calls)
					}
				})
			}
		}
	}
}

func TestIssue051GeminiNativeNoSearchKeepsRawWireAndSucceeds(t *testing.T) {
	for _, vertex := range []bool{false, true} {
		providerName := "Gemini"
		if vertex {
			providerName = "Vertex"
		}
		for _, stream := range []bool{false, true} {
			name := fmt.Sprintf("%s/%s", providerName, map[bool]string{false: "unary", true: "SSE"}[stream])
			t.Run(name, func(t *testing.T) {
				upstream := newIssue051Upstream(t, "gemini", stream)
				previousClient := requester.HTTPClient
				requester.HTTPClient = upstream.server.Client()
				t.Cleanup(func() { requester.HTTPClient = previousClient })
				raw := `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"future_field":{"keep":true}}`
				provider, request := issue051GeminiProvider(t, upstream, vertex, raw, stream)
				native := provider.(gemini.GeminiChatInterface)
				if stream {
					streamResponse, apiErr := native.CreateGeminiChatStream(request)
					if apiErr != nil {
						t.Fatalf("normal native Gemini stream failed: %+v", apiErr)
					}
					issue051DrainStream(t, streamResponse)
				} else {
					response, apiErr := native.CreateGeminiChat(request)
					if apiErr != nil || response == nil {
						t.Fatalf("normal native Gemini request failed: response=%+v err=%+v", response, apiErr)
					}
				}
				if upstream.calls != 1 || !bytes.Equal(upstream.bodies[0], []byte(raw)) {
					t.Fatalf("normal %s request changed or skipped raw wire: calls=%d body=%s", providerName, upstream.calls, upstream.bodies[0])
				}
			})
		}
	}
}

type issue051AliCase struct {
	name         string
	compatible   bool
	custom       string
	allowExtra   bool
	mapModel     bool
	pluginSearch bool
	rawExtra     string
	wantEnabled  bool
}

func issue051AliCases() []issue051AliCase {
	return []issue051AliCase{
		{name: "native CustomParameter", custom: `{"parameters":{"enable_search":true}}`, wantEnabled: true},
		{name: "native per_model after mapping", custom: `{"per_model":true,"qwen-plus":{"parameters":{"enable_search":true}}}`, mapModel: true, wantEnabled: true},
		{name: "native final false", custom: `{"parameters":{"enable_search":false}}`, allowExtra: true, pluginSearch: true, rawExtra: `,"parameters":{"enable_search":false},"future_field":{"keep":true}`},
		{name: "native raw nested search ignored by typed parameters", allowExtra: true, rawExtra: `,"parameters":{"enable_search":true},"future_field":{"keep":true}`},
		{name: "native pre_add materialized", custom: `{"pre_add":true,"parameters":{"enable_search":true}}`, allowExtra: true, rawExtra: `,"parameters":{"enable_search":true},"future_field":{"keep":true}`},
		{name: "compatible AllowExtraBody", compatible: true, allowExtra: true, rawExtra: `,"enable_search":true,"future_field":{"keep":true}`, wantEnabled: true},
		{name: "compatible CustomParameter per_model", compatible: true, custom: `{"per_model":true,"qwen-plus":{"enable_search":true}}`, mapModel: true, wantEnabled: true},
		{name: "compatible pre_add materialized", compatible: true, custom: `{"pre_add":true,"enable_search":true}`, allowExtra: true, rawExtra: `,"enable_search":true`, wantEnabled: true},
		{name: "compatible final false", compatible: true, custom: `{"enable_search":true}`, allowExtra: true, rawExtra: `,"enable_search":false,"future_field":{"keep":true}`},
	}
}

func issue051AliChannel(upstream *issue051Upstream, testCase issue051AliCase) *model.Channel {
	proxy := ""
	channel := &model.Channel{
		Type:           config.ChannelTypeAli,
		Key:            "issue-051-ali-key",
		Proxy:          &proxy,
		BaseURL:        func() *string { value := upstream.server.URL; return &value }(),
		AllowExtraBody: testCase.allowExtra,
	}
	pluginData := model.PluginType{}
	if testCase.compatible {
		pluginData["use_openai_api"] = map[string]interface{}{"enable": true}
	}
	if testCase.pluginSearch {
		pluginData["web_search"] = map[string]interface{}{"enable": true}
	}
	if len(pluginData) > 0 {
		plugin := datatypes.NewJSONType(pluginData)
		channel.Plugin = &plugin
	}
	if testCase.custom != "" {
		channel.CustomParameter = &testCase.custom
	}
	if testCase.mapModel {
		mapping := `{"client-qwen":"qwen-plus"}`
		channel.ModelMapping = &mapping
	}
	return channel
}

func TestIssue051AliSearchGateUsesFinalNativeAndCompatibleBody(t *testing.T) {
	for _, testCase := range issue051AliCases() {
		for _, stream := range []bool{false, true} {
			name := fmt.Sprintf("%s/%s", testCase.name, map[bool]string{false: "unary", true: "SSE"}[stream])
			t.Run(name, func(t *testing.T) {
				kind := "ali-native"
				if testCase.compatible {
					kind = "ali-compatible"
				}
				upstream := newIssue051Upstream(t, kind, stream)
				previousClient := requester.HTTPClient
				requester.HTTPClient = upstream.server.Client()
				t.Cleanup(func() { requester.HTTPClient = previousClient })

				modelName := "qwen-plus"
				if testCase.mapModel {
					modelName = "client-qwen"
				}
				raw := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}],"stream":%t%s}`, modelName, stream, testCase.rawExtra)
				var request types.ChatCompletionRequest
				if err := json.Unmarshal([]byte(raw), &request); err != nil {
					t.Fatalf("decode Ali Chat request: %v", err)
				}
				fields := make(map[string]json.RawMessage)
				if err := json.Unmarshal([]byte(raw), &fields); err != nil {
					t.Fatalf("decode Ali raw fields: %v", err)
				}
				channel := issue051AliChannel(upstream, testCase)
				provider := createProvider(channel)
				if provider == nil {
					t.Fatal("Ali factory did not create a provider")
				}
				canonicalModel, err := provider.ModelMappingHandler(modelName)
				if err != nil {
					t.Fatalf("map Ali model for capability: %v", err)
				}
				request.Model = canonicalModel
				if _, err := AssessChatRequest(channel, canonicalModel, &request, fields); err != nil {
					if testCase.wantEnabled {
						ctx := issue051Context(t, "/v1/chat/completions", raw)
						provider.SetContext(ctx)
						provider.SetOriginalModel(modelName)
						provider.SetUsage(&types.Usage{})
						chat, ok := provider.(base.ChatInterface)
						if !ok {
							t.Fatal("Ali provider does not implement ChatInterface")
						}
						request.Model = canonicalModel
						if stream {
							streamResponse, providerErr := chat.CreateChatCompletionStream(&request)
							if streamResponse != nil || providerErr == nil || providerErr.Code != "ali_search_billing_unsupported" {
								t.Fatalf("provider sender accepted enabled search: stream=%v err=%+v", streamResponse, providerErr)
							}
						} else {
							response, providerErr := chat.CreateChatCompletion(&request)
							if response != nil || providerErr == nil || providerErr.Code != "ali_search_billing_unsupported" {
								t.Fatalf("provider sender accepted enabled search: response=%+v err=%+v", response, providerErr)
							}
						}
						if upstream.calls != 0 {
							t.Fatalf("rejected search reached upstream before provider work: %v", err)
						}
						return
					}
					t.Fatalf("final false search was rejected: %v", err)
				}
				if testCase.wantEnabled {
					t.Fatal("final enabled search passed capability gate")
				}

				ctx := issue051Context(t, "/v1/chat/completions", raw)
				provider.SetContext(ctx)
				provider.SetOriginalModel(modelName)
				provider.SetUsage(&types.Usage{})
				chat, ok := provider.(base.ChatInterface)
				if !ok {
					t.Fatal("Ali provider does not implement ChatInterface")
				}
				if stream {
					streamResponse, apiErr := chat.CreateChatCompletionStream(&request)
					if apiErr != nil {
						t.Fatalf("final false Ali stream failed: %+v", apiErr)
					}
					issue051DrainStream(t, streamResponse)
				} else {
					response, apiErr := chat.CreateChatCompletion(&request)
					if apiErr != nil || response == nil {
						t.Fatalf("final false Ali request failed: response=%+v err=%+v", response, apiErr)
					}
				}
				if upstream.calls != 1 || len(upstream.bodies) != 1 {
					t.Fatalf("final false Ali request upstream calls=%d, want 1", upstream.calls)
				}
				var body map[string]interface{}
				if err := json.Unmarshal(upstream.bodies[0], &body); err != nil {
					t.Fatalf("decode final Ali upstream body: %v; body=%s", err, upstream.bodies[0])
				}
				if testCase.compatible {
					if enabled, _ := body["enable_search"].(bool); enabled {
						t.Fatalf("compatible final body enabled search: %s", upstream.bodies[0])
					}
				} else if parameters, _ := body["parameters"].(map[string]interface{}); parameters != nil {
					if enabled, _ := parameters["enable_search"].(bool); enabled {
						t.Fatalf("native final body enabled search: %s", upstream.bodies[0])
					}
				}
				if testCase.allowExtra && !bytes.Contains(upstream.bodies[0], []byte(`"future_field":{"keep":true}`)) {
					t.Fatalf("final false Ali body lost unknown extra field: %s", upstream.bodies[0])
				}
			})
		}
	}
}
