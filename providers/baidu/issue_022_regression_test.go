package baidu

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"
)

func decodeBaiduEmbeddingResponseForTest(t *testing.T, usage string) *BaiduEmbeddingResponse {
	t.Helper()
	payload := `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}]}`
	if usage != "" {
		payload = `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"usage":` + usage + `}`
	}
	var response BaiduEmbeddingResponse
	if err := json.Unmarshal([]byte(payload), &response); err != nil {
		t.Fatalf("解码百度 Embeddings 响应失败：%v", err)
	}
	return &response
}

func TestIssue022BaiduEmbeddingUsageRequiresConsistentInputEvidence(t *testing.T) {
	tests := []struct {
		name       string
		usage      string
		wantPrompt int
		wantReport bool
	}{
		{name: "prompt_total", usage: `{"prompt_tokens":8,"total_tokens":8}`, wantPrompt: 8, wantReport: true},
		{name: "explicit_zero_completion", usage: `{"prompt_tokens":8,"completion_tokens":0,"total_tokens":8}`, wantPrompt: 8, wantReport: true},
		{name: "zero_input", usage: `{"prompt_tokens":0,"total_tokens":0}`, wantPrompt: 0, wantReport: true},
		{name: "missing_usage", wantReport: false},
		{name: "missing_prompt", usage: `{"total_tokens":8}`, wantReport: false},
		{name: "missing_total", usage: `{"prompt_tokens":8}`, wantReport: false},
		{name: "conflicting_total", usage: `{"prompt_tokens":8,"total_tokens":9}`, wantReport: false},
		{name: "negative_prompt", usage: `{"prompt_tokens":-1,"total_tokens":-1}`, wantReport: false},
		{name: "negative_total", usage: `{"prompt_tokens":8,"total_tokens":-8}`, wantReport: false},
		{name: "nonzero_completion", usage: `{"prompt_tokens":8,"completion_tokens":1,"total_tokens":9}`, wantReport: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &BaiduProvider{}
			provider.SetUsage(&types.Usage{PromptTokens: 999})
			response := decodeBaiduEmbeddingResponseForTest(t, test.usage)
			converted, apiErr := provider.convertToEmbeddingOpenai(response, &types.EmbeddingRequest{Model: "Embedding-V1"})
			if apiErr != nil || converted == nil || converted.Usage == nil {
				t.Fatalf("转换百度 Embeddings 响应失败：response=%+v err=%+v", converted, apiErr)
			}
			if converted.Usage.HasProviderUsage() != test.wantReport {
				t.Fatalf("provider evidence=%t want=%t usage=%+v", converted.Usage.HasProviderUsage(), test.wantReport, converted.Usage)
			}
			if !reflect.DeepEqual(*converted.Usage, *provider.GetUsage()) {
				t.Fatalf("公开 usage 与 provider usage 不一致：response=%+v provider=%+v", converted.Usage, provider.GetUsage())
			}
			if test.wantReport {
				usage := converted.Usage
				if usage.PromptTokens != test.wantPrompt || usage.CompletionTokens != 0 || usage.TotalTokens != test.wantPrompt ||
					!usage.ProviderTokenFields["prompt_tokens"] ||
					!usage.ProviderTokenFields["completion_tokens"] ||
					!usage.ProviderTokenFields["total_tokens"] {
					t.Fatalf("输入型 usage 证据不完整：%+v", usage)
				}
			} else if converted.Usage.ProviderReported {
				t.Fatalf("无效 usage 被标记为可信：%+v", converted.Usage)
			}
		})
	}
}

func TestIssue022BaiduEmbeddingUsesLocalOAuthAndPublishesUsage(t *testing.T) {
	var oauthCalls atomic.Int32
	var embeddingCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/2.0/token":
			oauthCalls.Add(1)
			if r.Method != http.MethodPost || r.URL.Query().Get("grant_type") != "client_credentials" ||
				r.URL.Query().Get("client_id") != "local-client" || r.URL.Query().Get("client_secret") != "local-secret" {
				t.Errorf("OAuth 桩收到错误请求：%s %s", r.Method, r.URL.String())
			}
			_, _ = io.WriteString(w, `{"access_token":"local-token","expires_in":3600}`)
		case "/rpc/2.0/ai_custom/v1/wenxinworkshop/embeddings/Embedding-V1":
			embeddingCalls.Add(1)
			if r.URL.Query().Get("access_token") != "local-token" {
				t.Errorf("Embeddings 请求缺少 OAuth token")
			}
			_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"usage":{"prompt_tokens":8,"total_tokens":8}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	previousHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousHTTPClient })

	proxy := ""
	baseURL := server.URL
	channel := &model.Channel{
		Id:      22022,
		Type:    config.ChannelTypeBaidu,
		Key:     "local-client|local-secret",
		Proxy:   &proxy,
		BaseURL: &baseURL,
	}
	provider := (BaiduProviderFactory{}).Create(channel).(*BaiduProvider)
	// The production OAuth host is provider-owned; point only this test provider
	// at the local stub so no real OAuth or Baidu endpoint is contacted.
	provider.Config.BaseURL = server.URL
	provider.SetUsage(&types.Usage{PromptTokens: 999})

	response, apiErr := provider.CreateEmbeddings(&types.EmbeddingRequest{Model: "Embedding-V1", Input: "hello"})
	if apiErr != nil || response == nil || response.Usage == nil {
		t.Fatalf("本地百度 Embeddings 请求失败：response=%+v err=%+v", response, apiErr)
	}
	if oauthCalls.Load() != 1 || embeddingCalls.Load() != 1 {
		t.Fatalf("本地 OAuth/Embeddings 请求次数异常：oauth=%d embedding=%d", oauthCalls.Load(), embeddingCalls.Load())
	}
	if !response.Usage.HasProviderUsage() || response.Usage.PromptTokens != 8 || response.Usage.CompletionTokens != 0 || response.Usage.TotalTokens != 8 {
		t.Fatalf("百度输入型 usage 未建立可信证据：%+v", response.Usage)
	}
	if !reflect.DeepEqual(*response.Usage, *provider.GetUsage()) {
		t.Fatalf("公开 usage 与 p.Usage 不一致：response=%+v provider=%+v", response.Usage, provider.GetUsage())
	}
}

func TestIssue022BaiduEmbeddingErrorDoesNotPublishProviderEvidence(t *testing.T) {
	provider := &BaiduProvider{}
	provider.SetUsage(&types.Usage{PromptTokens: 999})
	response := &BaiduEmbeddingResponse{BaiduError: BaiduError{ErrorCode: 1, ErrorMsg: "bad request"}}
	converted, apiErr := provider.convertToEmbeddingOpenai(response, &types.EmbeddingRequest{Model: "Embedding-V1"})
	if converted != nil || apiErr == nil {
		t.Fatalf("百度错误响应未按错误返回：response=%+v err=%+v", converted, apiErr)
	}
	if provider.GetUsage().ProviderReported {
		t.Fatalf("错误响应建立了 provider evidence：%+v", provider.GetUsage())
	}
}
