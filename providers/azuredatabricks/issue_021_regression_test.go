package azuredatabricks

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

func TestI021DatabricksStreamPublishesCumulativeUsageWithoutChangingWire(t *testing.T) {
	const (
		modelName = "databricks-actual"
		first     = `{"id":"db-stream","object":"chat.completion.chunk","model":"databricks-actual","service_tier":"flex","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}],"usage":{"prompt_tokens":100,"completion_tokens":3,"total_tokens":103,"prompt_tokens_details":{"cached_tokens":7,"text_tokens":93},"completion_tokens_details":{"reasoning_tokens":1,"text_tokens":2},"future_usage":{"kept":true}}}`
		second    = `{"id":"db-stream","object":"chat.completion.chunk","model":"databricks-actual","service_tier":"flex","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":8,"text_tokens":92},"completion_tokens_details":{"reasoning_tokens":2,"text_tokens":18}}}`
		empty     = `{"id":"db-stream","object":"chat.completion.chunk","choices":[],"usage":{}}`
	)
	wire := "data: " + first + "\n\ndata: " + second + "\n\ndata: " + empty + "\n\ndata: [DONE]\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, wire)
	}))
	defer server.Close()

	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })
	proxy, baseURL := "", server.URL
	provider := (AzureDatabricksProviderFactory{}).Create(&model.Channel{Key: "test-token", Proxy: &proxy, BaseURL: &baseURL}).(*AzureDatabricksProvider)
	provider.SetUsage(&types.Usage{})

	stream, apiErr := provider.CreateChatCompletionStream(&types.ChatCompletionRequest{
		Model:    modelName,
		Stream:   true,
		Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}},
	})
	if apiErr != nil {
		t.Fatalf("创建 Databricks stream 失败：%+v", apiErr)
	}
	output := drainDatabricksStream(t, stream)
	wantOutput := first + second + empty
	if output != wantOutput {
		t.Fatalf("stream wire 被改写：got=%q want=%q", output, wantOutput)
	}

	usage := provider.GetUsage()
	if !usage.HasProviderUsage() || usage.PromptTokens != 100 || usage.CompletionTokens != 20 || usage.TotalTokens != 120 {
		t.Fatalf("累计 provider usage 错误：%+v", usage)
	}
	if usage.ResponseModel != modelName || usage.ServiceTier != "flex" {
		t.Fatalf("实际 model/tier 未保留：%+v", usage)
	}
	if usage.PromptTokensDetails.CachedTokens != 8 || usage.PromptTokensDetails.TextTokens != 92 || usage.CompletionTokensDetails.ReasoningTokens != 2 || usage.CompletionTokensDetails.TextTokens != 18 {
		t.Fatalf("usage 明细未保留最后一个累计快照：%+v", usage)
	}
}

func TestI021DatabricksStreamRejectsInvalidUsageWithoutEstimate(t *testing.T) {
	for _, test := range []struct {
		name  string
		usage string
	}{
		{name: "缺 prompt", usage: `{"completion_tokens":20,"total_tokens":20}`},
		{name: "缺 completion", usage: `{"prompt_tokens":100,"total_tokens":100}`},
		{name: "缺 total", usage: `{"prompt_tokens":100,"completion_tokens":20}`},
		{name: "负 prompt", usage: `{"prompt_tokens":-1,"completion_tokens":20,"total_tokens":19}`},
		{name: "负 completion", usage: `{"prompt_tokens":100,"completion_tokens":-1,"total_tokens":99}`},
		{name: "负 total", usage: `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":-1}`},
		{name: "总数不一致", usage: `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":999}`},
		{name: "prompt 类型错误", usage: `{"prompt_tokens":"100","completion_tokens":20,"total_tokens":120}`},
		{name: "负明细", usage: `{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"completion_tokens_details":{"reasoning_tokens":-1}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &AzureDatabricksProvider{BaseProvider: base.BaseProvider{}}
			provider.SetUsage(&types.Usage{})
			line := []byte("data: {\"model\":\"actual-model\",\"service_tier\":\"priority\",\"choices\":[],\"usage\":" + test.usage + "}")
			dataChan := make(chan string, 1)
			errChan := make(chan error, 1)
			provider.streamHandler(&line, dataChan, errChan)
			if len(errChan) != 0 {
				t.Fatalf("invalid usage 不应产生 stream 错误：%v", <-errChan)
			}
			if got := <-dataChan; got == "" {
				t.Fatal("invalid usage frame 未透传")
			}
			if provider.GetUsage().HasProviderUsage() {
				t.Fatalf("invalid usage 被授权为 provider evidence：%+v", provider.GetUsage())
			}
			if provider.GetUsage().PromptTokens != 0 || provider.GetUsage().CompletionTokens != 0 || provider.GetUsage().TotalTokens != 0 {
				t.Fatalf("invalid usage 触发了本地估算：%+v", provider.GetUsage())
			}
		})
	}
}

func TestI021DatabricksUnaryAndStreamUsageHaveSameSnapshot(t *testing.T) {
	const body = `{"id":"db-unary","object":"chat.completion","model":"databricks-actual","service_tier":"priority","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":5}}}`
	provider := &AzureDatabricksProvider{}
	provider.SetUsage(&types.Usage{})
	response, err := provider.convertResponse(&http.Response{Body: io.NopCloser(strings.NewReader(body))})
	if err != nil {
		t.Fatalf("unary convertResponse 失败：%v", err)
	}
	if response.Usage == nil || !response.Usage.HasProviderUsage() || !provider.GetUsage().HasProviderUsage() {
		t.Fatalf("unary usage 未形成可信证据：response=%+v provider=%+v", response.Usage, provider.GetUsage())
	}
	if !reflect.DeepEqual(*response.Usage, *provider.GetUsage()) {
		t.Fatalf("unary 公开与内部 usage 不一致：response=%+v provider=%+v", response.Usage, provider.GetUsage())
	}
	if response.ServiceTier != "priority" || provider.GetUsage().ServiceTier != "priority" {
		t.Fatalf("unary service tier 未保留：response=%+v provider=%+v", response, provider.GetUsage())
	}
}

func drainDatabricksStream(t *testing.T, stream requester.StreamReaderInterface[string]) string {
	t.Helper()
	defer requester.CloseAndDrainStream(stream)
	data, streamErrors := stream.Recv()
	var output strings.Builder
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
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
				t.Fatalf("读取 Databricks stream：%v", err)
			}
		case <-timer.C:
			t.Fatal("等待 Databricks stream 结束超时")
		}
	}
	return output.String()
}
