package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"one-api/common/config"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	"one-api/common/requester"
	"one-api/common/responsesws"
	"one-api/common/wsconn"
	"one-api/common/wsconn/wstest"
	"one-api/model"
	"one-api/providers"
	providersBase "one-api/providers/base"
	claudeProvider "one-api/providers/claude"
	"one-api/providers/deepseek"
	"one-api/providers/gemini"
	"one-api/providers/openai"
	runtimerealtime "one-api/runtime/realtime"
	"one-api/types"
)

func TestProviderRedactionHTTPDelivery(t *testing.T) {
	const credential = "provider-secret-12345"
	for _, test := range []struct {
		name       string
		stream     bool
		status     int
		body       string
		wantStatus int
		want       string
	}{
		{"JSON", false, 200, `{"id":"chat_1","account_id":"acct-secret","choices":[{"message":{"content":"provider\u002dsecret-12345"}}],"future":9007199254740993}`, 200, `"future":9007199254740993`},
		{"SSE", true, 200, "event: chunk\r\ndata: {\"id\":\"chat_1\",\"account_id\":\"acct-secret\",\"choices\":[{\"delta\":{\"content\":\"provider\\u002dsecret-12345\"}}],\"future\":9007199254740993}\r\n\r\ndata: [DONE]\r\n\r\n", 200, "event: chunk\r\ndata:"},
		{"error replay", false, 400, `{"error":{"message":"Missing required header: OpenAI-Beta","code":"invalid_header","param":"session","id_token":"acct-secret","client_assertion":"acct-secret","x_api_key":"acct-secret"},"future":9007199254740993}`, 400, `"param":"session"`},
		{"error sibling extension", false, 400, `{"error":{"message":"bad input","code":"invalid_request"},"debug":{"access_token":"acct-secret"}}`, 400, `"code":"invalid_request"`},
		{"SSE error sibling extension", true, 200, "data: {\"error\":{\"message\":\"bad input\",\"code\":\"invalid_request\"},\"debug\":{\"access_token\":\"acct-secret\"}}\n\n", 200, `"code":"invalid_request"`},
		{"typed error", false, 400, `{"error":{"message":"bad input","code":{"detail":"provider-secret-12345"},"param":"session","innererror":{"id_token":"acct-secret","number":9007199254740993}}}`, 400, `"param":"session"`},
		{"duplicate JSON", false, 200, `{"id":"chat_1","account_id":"acct-secret","account_id":"[redacted]","choices":[]}`, 200, `"account_id":"acct-secret"`},
		{"duplicate SSE", true, 200, "data: {\"account_id\":\"acct-secret\",\"account_id\":\"[redacted]\"}\n\n", 200, `"account_id":"acct-secret"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				if req.Header.Get("Authorization") != "Bearer "+credential {
					w.WriteHeader(401)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if test.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				w.Header().Set("Digest", "old-body-digest")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer upstream.Close()
			previousClient := requester.HTTPClient
			requester.HTTPClient = upstream.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })
			engine := gin.New()
			engine.POST("/v1/chat/completions", func(c *gin.Context) {
				proxy := ""
				p := openai.CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: credential, Proxy: &proxy}, upstream.URL)
				p.SetContext(c)
				p.SetUsage(&types.Usage{})
				p.SetProviderRawJSONReplay(true)
				p.SetOpenAIErrorEnvelopeReplay(test.name != "typed error")
				request := &types.ChatCompletionRequest{Model: "gpt-5", Stream: test.stream, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}}}
				var apiErr *types.OpenAIErrorWithStatusCode
				if test.stream {
					var stream requester.StreamReaderInterface[string]
					stream, apiErr = p.CreateChatCompletionStream(request)
					if apiErr == nil {
						_, apiErr = responseStreamClient(c, stream, nil)
					}
				} else {
					var response *types.ChatCompletionResponse
					response, apiErr = p.CreateChatCompletion(request)
					if apiErr == nil {
						apiErr = responseJsonClient(c, response)
					}
				}
				if apiErr != nil && !c.GetBool(streamErrorAlreadyRenderedContextKey) {
					if test.wantStatus == 502 && (!apiErr.UpstreamAccepted || relayAttemptShouldRetry(NewRelayChat(c), apiErr, config.ChannelTypeOpenAI)) {
						t.Error("安全失败丢失上游执行事实或打开重试")
					}
					relayResponseWithOpenAIErr(c, apiErr)
				}
			})
			downstream := httptest.NewServer(engine)
			defer downstream.Close()
			client := downstream.Client()
			client.Timeout = 3 * time.Second
			response, err := client.Post(downstream.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != test.wantStatus || !strings.Contains(string(body), test.want) {
				t.Fatalf("交付结果错误: status=%d body=%s", response.StatusCode, body)
			}

			if test.status == 200 && (test.name == "JSON" || test.name == "SSE" || strings.HasPrefix(test.name, "duplicate")) {
				if string(body) != test.body {
					t.Fatalf("成功正文未逐字节透传: want=%q got=%q", test.body, body)
				}
			}
			if test.name == "error replay" && strings.Contains(string(body), "acct-secret") {
				t.Fatalf("错误诊断未脱敏: %s", body)
			}
			if calls.Load() != 1 {
				t.Fatalf("安全处理导致重新提交: %d", calls.Load())
			}
		})
	}
}

func TestProviderRedactionFallbackDoesNotAbortCommittedSSE(t *testing.T) {
	raw := "data: {\"type\":\"error\",\"error\":{\"message\":\"one\",\"message\":\"two\"}}\n\n"
	engine := gin.New()
	engine.GET("/stream", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.WriteString("data: {\"safe\":true}\n\n")
		c.Writer.Flush()
		_, _ = c.Writer.WriteString(redactProviderSSEEvent(raw))
		_, _ = c.Writer.WriteString("data: [DONE]\n\n")
	})
	server := httptest.NewServer(engine)
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "data: {\"safe\":true}\n\n"+raw+"data: [DONE]\n\n" {
		t.Fatalf("脱敏失败干扰交付: %q %v", body, err)
	}
}

type redactionWSUpstream struct {
	responsesWSTestSession
	*providerresponse.CredentialSnapshot
}

func TestProviderRedactionNativeWSDelivery(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		name := "credential"
		if malformed {
			name = "duplicate"
		}
		t.Run(name, func(t *testing.T) {
			client, server := wstest.Pair(t)
			defer client.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})
			defer server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("GET", "/v1/responses", nil)
			actor := NewResponsesWSSessionActor(c)
			pump := NewResponsesWSManagedPump(server, actor)
			defer pump.cancel()
			actor.SetPump(pump)
			actor.upstream.session = &redactionWSUpstream{CredentialSnapshot: providerresponse.NewCredentialSnapshot([]string{"provider-secret-12345"})}
			payload := []byte(`{"type":"response.future","delta":"provider\u002dsecret-12345","future":9007199254740993}`)
			if malformed {
				payload = []byte(`{"account_id":"secret","account_id":"[redacted]"}`)
			}
			err := actor.emitProviderFrameForAttempt(nil, responsesws.NewTextFrame(payload), "test")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, got, err := client.ReadInitial(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(payload) {
				t.Fatalf("普通或歧义帧被脱敏改变: %s", got)
			}
		})
	}
}

func TestProviderRedactionRealtimeUsesOriginalConnectionCredentials(t *testing.T) {
	client, server := wstest.Pair(t)
	defer client.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})
	defer server.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort})
	actor := newRealtimeRelayActor(server, relayTestRealtimeSession{}, time.Minute)
	defer actor.cancel()
	frame := runtimerealtime.NewTextFrame([]byte(`{"type":"error","error":{"message":"old\u002dconnection-secret","code":"invalid_request"}}`))
	credentials := []string{"old-connection-secret"}
	snapshot := providerresponse.NewCredentialSnapshot(credentials)
	credentials[0] = "new-connection-secret"
	if !actor.deliverEventFrame(runtimerealtime.RecvEvent{Frame: &frame, Origin: runtimerealtime.RealtimePayloadOriginProvider, Credentials: snapshot}) {
		t.Fatal("安全消息未交付")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, got, err := client.ReadInitial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"type":"error","error":{"message":"[redacted]","code":"invalid_request"}}` {
		t.Fatalf("旧消息未使用原连接凭据: %s", got)
	}
}

func TestProviderRedactionRealDeepseekResponsesCrossProtocolCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "provider-secret-12345"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer "+secret {
			t.Errorf("unexpected auth %q", req.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-chat\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"provider-secret-12345\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chat_1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	old := requester.HTTPClient
	requester.HTTPClient = upstream.Client()
	defer func() { requester.HTTPClient = old }()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	enableResponsesTestDeadline(c)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	proxy := ""
	baseURL := upstream.URL
	channel := &model.Channel{Type: config.ChannelTypeDeepseek, CompatibleResponse: true, Key: secret, BaseURL: &baseURL, Proxy: &proxy}
	path, ok := providers.ResolveAdapterSupport(channel).DataPath(providersBase.OperationResponsesCreate)
	if !ok || path != providersBase.DataPathCrossProtocol {
		t.Fatalf("route unavailable %v %v", path, ok)
	}
	p := (deepseek.DeepseekProviderFactory{}).Create(channel)
	p.SetContext(c)
	p.SetUsage(&types.Usage{})
	store := false
	r := &relayResponses{relayBase: relayBase{c: c, provider: p, modelName: "deepseek-chat"}, selectedDataPath: path, responsesRequest: types.OpenAIResponsesRequest{Model: "deepseek-chat", Stream: true, Store: &store}, preparedChatRequest: &types.ChatCompletionRequest{Model: "deepseek-chat", Stream: true, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hi"}}}}
	apiErr, _ := r.send()
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	if len(requestctx.ProviderCredentials(c)) == 0 {
		t.Fatal("actual HTTP auth snapshot absent")
	}
	if !strings.Contains(recorder.Body.String(), secret) {
		t.Fatalf("real provider cross-protocol path changed model content; body=%s", recorder.Body.String())
	}
}

type redactionClaudeStreamProvider struct {
	providersBase.BaseProvider
	stream requester.StreamReaderInterface[string]
}

func (p *redactionClaudeStreamProvider) GetRequestHeaders() map[string]string { return nil }
func (p *redactionClaudeStreamProvider) CreateClaudeChat(*claudeProvider.ClaudeRequest) (*claudeProvider.ClaudeResponse, *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}
func (p *redactionClaudeStreamProvider) CreateClaudeChatStream(*claudeProvider.ClaudeRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	return p.stream, nil
}
func TestProviderRedactionClaudeAmbiguousEventPassesThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, payload := range []string{`{"type":"error","error":{"message":"one","message":"two"}}`, `{"type":"message_start","message":{"id":"one","id":"two"}}`} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		stream := &fakeRelayStream{dataChan: make(chan string, 1), errChan: make(chan error)}
		stream.dataChan <- "data: " + payload + "\n\n"
		close(stream.dataChan)
		close(stream.errChan)
		p := &redactionClaudeStreamProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{}, Usage: &types.Usage{}}, stream: stream}
		r := &relayClaudeOnly{relayBase: relayBase{c: c, provider: p}, claudeRequest: &claudeProvider.ClaudeRequest{Model: "claude", Stream: true}}
		apiErr, _ := r.send()
		if recorder.Body.String() != "data: "+payload+"\n\n" {
			t.Fatalf("脱敏干扰 Claude 原始事件: %q err=%v", recorder.Body.String(), apiErr)
		}
	}
}

type redactionGeminiStreamProvider struct {
	providersBase.BaseProvider
	stream requester.StreamReaderInterface[string]
}

func (p *redactionGeminiStreamProvider) GetRequestHeaders() map[string]string { return nil }
func (p *redactionGeminiStreamProvider) CreateGeminiChat(*gemini.GeminiChatRequest) (*gemini.GeminiChatResponse, *types.OpenAIErrorWithStatusCode) {
	return nil, nil
}
func (p *redactionGeminiStreamProvider) CreateGeminiChatStream(*gemini.GeminiChatRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	return p.stream, nil
}
func TestProviderRedactionGeminiAmbiguousEventPassesThrough(t *testing.T) {
	for _, payload := range []string{`{"error":{"message":"one","message":"two"}}`, `{"candidates":[],"candidates":[]}`} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini:streamGenerateContent", nil)
		stream := &fakeRelayStream{dataChan: make(chan string, 1), errChan: make(chan error)}
		stream.dataChan <- "data: " + payload + "\n\n"
		close(stream.dataChan)
		close(stream.errChan)
		p := &redactionGeminiStreamProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{}, Usage: &types.Usage{}}, stream: stream}
		r := &relayGeminiOnly{relayBase: relayBase{c: c, provider: p}, geminiRequest: &gemini.GeminiChatRequest{Model: "gemini", Stream: true}}
		apiErr, _ := r.send()
		if recorder.Body.String() != "data: "+payload+"\n\n" {
			t.Fatalf("脱敏干扰 Gemini 原始事件: %q err=%v", recorder.Body.String(), apiErr)
		}
	}
}
