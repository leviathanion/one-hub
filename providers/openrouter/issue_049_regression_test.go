package openrouter

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/openai"
	"one-api/types"
)

func TestI049OpenRouterStreamProjectsCumulativeSearchUsageWithoutChangingWire(t *testing.T) {
	const (
		first  = `{"id":"or-stream","object":"chat.completion.chunk","model":"actual/openrouter-model","service_tier":"priority","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":null}],"usage":{"prompt_tokens":30,"completion_tokens":0,"total_tokens":30,"server_tool_use":{"web_search_requests":1},"future_usage":{"preserved":true}}}`
		second = `{"id":"or-stream","object":"chat.completion.chunk","model":"actual/openrouter-model","service_tier":"priority","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":0,"total_tokens":30,"server_tool_use":{"web_search_requests":2}}}`
	)
	wire := "data: " + first + "\n\ndata: " + second + "\n\ndata: " + second + "\n\ndata: [DONE]\n\n"
	var requestBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取 OpenRouter 请求失败：%v", err)
		}
		requestBody = string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	}))
	defer server.Close()

	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })
	proxy, baseURL := "", server.URL
	provider := OpenRouterProviderFactory{}.Create(&model.Channel{
		Type:    config.ChannelTypeOpenRouter,
		Key:     "test-key",
		Proxy:   &proxy,
		BaseURL: &baseURL,
	}).(*OpenRouterProvider)
	provider.SetUsage(&types.Usage{})
	// The provider's pre-existing reasoning adapter intentionally reserializes
	// chunks that contain reasoning fields; disable it here to assert that the
	// search observer itself leaves ordinary payload bytes untouched.
	provider.ReasoningHandler = false

	stream, apiErr := provider.CreateChatCompletionStream(&types.ChatCompletionRequest{
		Model:    "openrouter/auto",
		Stream:   true,
		Messages: []types.ChatCompletionMessage{{Role: "user", Content: "find this"}},
	})
	if apiErr != nil {
		t.Fatalf("创建 OpenRouter stream 失败：%+v", apiErr)
	}
	output := drainOpenRouterStream(t, stream)
	if want := first + second + second; output != want {
		t.Fatalf("stream wire 被改写：got=%q want=%q", output, want)
	}
	if !strings.Contains(requestBody, `"stream_options":{"include_usage":true}`) {
		t.Fatalf("OpenRouter stream 未请求 provider usage：%s", requestBody)
	}

	usage := provider.GetUsage()
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "")
	if !usage.HasProviderUsage() || usage.PromptTokens != 30 || usage.CompletionTokens != 0 || usage.TotalTokens != 30 {
		t.Fatalf("stream token usage 错误：%+v", usage)
	}
	if usage.ResponseModel != "actual/openrouter-model" || usage.ServiceTier != "priority" {
		t.Fatalf("stream model/tier 未保留：%+v", usage)
	}
	if usage.ExtraBilling[key].CallCount != 2 || !usage.HasProviderExtraBilling(key) {
		t.Fatalf("累计搜索计数错误或未授权：%+v", usage)
	}
	if usage.ProviderServerToolUse == nil || usage.ProviderServerToolUse.WebSearchRequests != 2 {
		t.Fatalf("provider server tool usage 未保留：%+v", usage.ProviderServerToolUse)
	}
}

func TestI049OpenRouterSearchUsageZeroMissingOrInvalidDoesNotCreateBillingUnit(t *testing.T) {
	for _, test := range []struct {
		name      string
		toolUsage string
	}{
		{name: "zero", toolUsage: `{"web_search_requests":0}`},
		{name: "missing", toolUsage: ""},
		{name: "negative", toolUsage: `{"web_search_requests":-1}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &OpenRouterProvider{}
			provider.Usage = &types.Usage{}
			usage := `{"prompt_tokens":30,"completion_tokens":0,"total_tokens":30`
			if test.toolUsage != "" {
				usage += `,"server_tool_use":` + test.toolUsage
			}
			usage += `}`
			line := []byte(`data: {"id":"or-usage","model":"actual-model","service_tier":"priority","choices":[],"usage":` + usage + `}`)
			dataChan := make(chan string, 1)
			errChan := make(chan error, 1)
			handler := provider.streamHandlerForTest()
			handler(&line, dataChan, errChan)
			if len(errChan) != 0 {
				t.Fatalf("%s usage 不应产生 stream error：%v", test.name, <-errChan)
			}
			if got := <-dataChan; got == "" {
				t.Fatalf("%s usage frame 未透传", test.name)
			}
			key := types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "")
			if _, exists := provider.Usage.ExtraBilling[key]; exists || provider.Usage.HasProviderExtraBilling(key) {
				t.Fatalf("%s usage 制造了搜索计费单位：%+v", test.name, provider.Usage)
			}
		})
	}
}

func TestI049GenericOpenAIStreamHandlerDoesNotTrustOpenRouterSearchMetadata(t *testing.T) {
	usage := &types.Usage{}
	handler := openai.OpenAIStreamHandler{Usage: usage, ExposeProviderUsage: true}
	line := []byte(`data: {"id":"generic","model":"other-provider","choices":[],"usage":{"prompt_tokens":30,"completion_tokens":0,"total_tokens":30,"server_tool_use":{"web_search_requests":2}}}`)
	dataChan := make(chan string, 1)
	errChan := make(chan error, 1)
	handler.HandlerChatStream(&line, dataChan, errChan)
	if len(errChan) != 0 {
		t.Fatalf("通用 OpenAI handler 产生错误：%v", <-errChan)
	}
	if got := <-dataChan; got == "" {
		t.Fatal("通用 OpenAI handler 未透传 payload")
	}
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "")
	if _, exists := usage.ExtraBilling[key]; exists || usage.HasProviderExtraBilling(key) {
		t.Fatalf("通用 handler 错误授权 OpenRouter 搜索计费：%+v", usage)
	}
}

func (p *OpenRouterProvider) streamHandlerForTest() requester.HandlerPrefix[string] {
	chatHandler := openai.OpenAIStreamHandler{
		Usage:               p.Usage,
		ModelName:           "openrouter/auto",
		ExposeProviderUsage: true,
		UsageHandler:        p.observeOpenRouterUsage,
		ReasoningHandler:    p.ReasoningHandler,
	}
	return chatHandler.HandlerChatStream
}

func drainOpenRouterStream(t *testing.T, stream requester.StreamReaderInterface[string]) string {
	t.Helper()
	defer requester.CloseAndDrainStream(stream)
	data, streamErrors := stream.Recv()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var output strings.Builder
	for data != nil || streamErrors != nil {
		select {
		case chunk, ok := <-data:
			if !ok {
				data = nil
				continue
			}
			output.WriteString(chunk)
		case err, ok := <-streamErrors:
			if !ok {
				streamErrors = nil
				continue
			}
			if !errors.Is(err, io.EOF) {
				t.Fatalf("读取 OpenRouter stream：%v", err)
			}
		case <-timer.C:
			t.Fatal("等待 OpenRouter stream 结束超时")
		}
	}
	return output.String()
}
