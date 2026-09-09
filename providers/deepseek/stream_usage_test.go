package deepseek

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
	"one-api/types"
)

// 显式请求流式 usage，并兼容网关返回的独立 usage chunk。
// DeepSeek 官方接口也可在最后一个内容 chunk 返回 usage。
func TestDeepseekStreamRequestsProviderUsage(t *testing.T) {
	wire := `data: {"id":"1","object":"chat.completion.chunk","model":"deepseek-chat","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"1","object":"chat.completion.chunk","model":"deepseek-chat","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8,"prompt_cache_hit_tokens":1,"prompt_cache_miss_tokens":2}}` + "\n\n" +
		"data: [DONE]\n\n"
	var requestBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取 DeepSeek 请求失败：%v", err)
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
	provider := DeepseekProviderFactory{}.Create(&model.Channel{
		Type:    config.ChannelTypeDeepseek,
		Key:     "test-key",
		Proxy:   &proxy,
		BaseURL: &baseURL,
	}).(*DeepseekProvider)
	provider.SetUsage(&types.Usage{})

	stream, apiErr := provider.CreateChatCompletionStream(&types.ChatCompletionRequest{
		Model:    "deepseek-chat",
		Stream:   true,
		Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hi"}},
	})
	if apiErr != nil {
		t.Fatalf("创建 DeepSeek stream 失败：%+v", apiErr)
	}
	drainDeepseekStream(t, stream)

	if !strings.Contains(requestBody, `"include_usage":true`) {
		t.Fatalf("DeepSeek stream 未请求 provider usage：%s", requestBody)
	}
	usage := provider.GetUsage()
	if !usage.HasProviderUsage() || usage.TotalTokens != 8 {
		t.Fatalf("DeepSeek stream usage 未被提取为可计费证据：%+v", usage)
	}
}

func drainDeepseekStream(t *testing.T, stream requester.StreamReaderInterface[string]) {
	t.Helper()
	defer requester.CloseAndDrainStream(stream)
	data, streamErrors := stream.Recv()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for data != nil || streamErrors != nil {
		select {
		case _, ok := <-data:
			if !ok {
				data = nil
			}
		case err, ok := <-streamErrors:
			if !ok {
				streamErrors = nil
				continue
			}
			if !errors.Is(err, io.EOF) {
				t.Fatalf("读取 DeepSeek stream：%v", err)
			}
		case <-timer.C:
			t.Fatal("等待 DeepSeek stream 结束超时")
		}
	}
}
