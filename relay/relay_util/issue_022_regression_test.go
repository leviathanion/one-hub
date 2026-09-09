package relay_util

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/baidu"
	"one-api/types"
)

func TestIssue022BaiduEmbeddingUsageSettlesThroughLocalOAuthAndSQL(t *testing.T) {
	for testIndex, test := range []struct {
		name       string
		wire       string
		wantReport bool
		wantCharge int64
	}{
		{
			name:       "省略completion零值",
			wire:       `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"usage":{"prompt_tokens":8,"total_tokens":8}}`,
			wantReport: true,
			wantCharge: 8,
		},
		{
			name:       "显式completion零值",
			wire:       `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"usage":{"prompt_tokens":8,"completion_tokens":0,"total_tokens":8}}`,
			wantReport: true,
			wantCharge: 8,
		},
		{
			name: "总数冲突",
			wire: `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"usage":{"prompt_tokens":8,"total_tokens":9}}`,
		},
		{
			name: "负输入",
			wire: `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"usage":{"prompt_tokens":-1,"total_tokens":-1}}`,
		},
		{
			name: "缺usage",
			wire: `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var oauthCalls atomic.Int32
			var embeddingCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/oauth/2.0/token":
					oauthCalls.Add(1)
					_, _ = io.WriteString(w, `{"access_token":"local-token","expires_in":3600}`)
				case "/rpc/2.0/ai_custom/v1/wenxinworkshop/embeddings/Embedding-V1":
					embeddingCalls.Add(1)
					_, _ = io.WriteString(w, test.wire)
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
				Id:      22023 + testIndex,
				Type:    config.ChannelTypeBaidu,
				Key:     "local-client|local-secret",
				Proxy:   &proxy,
				BaseURL: &baseURL,
			}
			provider := (baidu.BaiduProviderFactory{}).Create(channel).(*baidu.BaiduProvider)
			// 仅替换测试实例的 OAuth 主机，避免访问真实 Baidu 服务。
			provider.Config.BaseURL = server.URL
			provider.SetUsage(&types.Usage{PromptTokens: 999})

			request := &types.EmbeddingRequest{Model: "Embedding-V1", Input: "hello"}
			response, apiErr := provider.CreateEmbeddings(request)
			if apiErr != nil || response == nil || response.Usage == nil {
				t.Fatalf("本地百度 Embeddings 请求失败：response=%+v err=%+v", response, apiErr)
			}
			if oauthCalls.Load() != 1 || embeddingCalls.Load() != 1 {
				t.Fatalf("OAuth/Embeddings 请求次数异常：oauth=%d embedding=%d", oauthCalls.Load(), embeddingCalls.Load())
			}
			if response.Usage.HasProviderUsage() != test.wantReport {
				t.Fatalf("provider evidence=%t want=%t usage=%+v", response.Usage.HasProviderUsage(), test.wantReport, response.Usage)
			}
			if !reflect.DeepEqual(*response.Usage, *provider.GetUsage()) {
				t.Fatalf("公开 usage 与 p.Usage 不一致：response=%+v provider=%+v", response.Usage, provider.GetUsage())
			}

			settleProviderFixtureUsage(t, provider.GetUsage(), request.Model, test.wantReport, test.wantCharge)
		})
	}
}
