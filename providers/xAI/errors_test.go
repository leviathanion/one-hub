package xAI

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/common/config"
	"one-api/common/providerresponse"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"
)

func TestErrorResponsePreservesProtocolFields(t *testing.T) {
	for _, test := range []struct {
		name, body, message, code, kind, param string
	}{
		{name: "native error", body: `{"code":"not-found","error":"The model does not exist","team_id":"private-team"}`, message: "The model does not exist", code: "not-found"},
		{name: "OpenAI error", body: `{"error":{"message":"Unsupported reasoning effort","code":"invalid_value","type":"invalid_request_error","param":"reasoning_effort","account_id":"private-account","innererror":{"api_key":"private-key"}}}`, message: "Unsupported reasoning effort", code: "invalid_value", kind: "invalid_request_error", param: "reasoning_effort"},
		{name: "numeric code", body: `{"code":9007199254740993,"error":"Request rejected"}`, message: "Request rejected", code: "9007199254740993"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := RequestErrorHandle(&http.Response{Body: io.NopCloser(strings.NewReader(test.body))})
			if err == nil || err.Message != test.message || err.Type != test.kind || err.Param != test.param {
				t.Fatalf("错误协议字段未保留：%+v", err)
			}
			code, _ := json.Marshal(err.Code)
			if string(code) != `"`+test.code+`"` && string(code) != test.code {
				t.Fatalf("错误码改变：%s", code)
			}
			encoded, _ := json.Marshal(err)
			if strings.Contains(string(encoded), "private-") {
				t.Fatalf("错误投影泄露账户或凭据信息：%s", encoded)
			}
		})
	}
}

func TestUnknownErrorBodyUsesSharedFallback(t *testing.T) {
	for _, body := range []string{`<html>private-account</html>`, `{"message":"private-account"}`, `{"error":null}`, `{"error":{"account_id":"private-account"}}`, `{broken`} {
		err := requester.HandleErrorResp(&http.Response{
			StatusCode: http.StatusBadGateway,
			Body:       io.NopCloser(strings.NewReader(body)),
		}, RequestErrorHandle, false, false)
		if err.StatusCode != http.StatusBadGateway || err.Message != "bad response status code 502" || err.Code != "bad_response_status_code" {
			t.Fatalf("未知错误未使用安全兜底：%+v", err)
		}
	}
}

func TestChatErrorPreservesStatusAndDoesNotRetry(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusTooManyRequests} {
		for _, stream := range []bool{false, true} {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"code":"not-found","error":"The model does not exist","account_id":"private-account"}`)
			}))
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			proxy, url := "", server.URL
			provider := XAIProviderFactory{}.Create(&model.Channel{Type: config.ChannelTypeXAI, Proxy: &proxy, BaseURL: &url, Key: "provider-key"}).(*XAIProvider)
			provider.SetUsage(&types.Usage{})
			request := &types.ChatCompletionRequest{Model: "grok-4.7", Stream: stream, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "Hi"}}}
			var apiErr *types.OpenAIErrorWithStatusCode
			if stream {
				_, apiErr = provider.CreateChatCompletionStream(request)
			} else {
				_, apiErr = provider.CreateChatCompletion(request)
			}
			requester.HTTPClient = previousClient
			server.Close()
			if apiErr == nil || apiErr.StatusCode != status || apiErr.Code != "not-found" || apiErr.ProviderRateLimited != (status == http.StatusTooManyRequests) || apiErr.ResponseHeaders.Get("Retry-After") != "2" {
				t.Fatalf("HTTP 错误语义改变：stream=%v status=%d err=%+v", stream, status, apiErr)
			}
			if calls.Load() != 1 {
				t.Fatalf("错误请求被隐式重试：%d", calls.Load())
			}
			safe := providerresponse.SanitizeAPIError(apiErr, "provider-key")
			if strings.Contains(safe.Message, "private-account") {
				t.Fatalf("对客错误泄露账户信息：%s", safe.Message)
			}
		}
	}
}
