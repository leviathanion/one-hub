package claude

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
	"one-api/common/providerendpoint"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"
)

func TestCustomMessagesEndpointPreservesWireAndStopsDisabledWork(t *testing.T) {
	for _, mode := range []string{"default", "relative", "absolute", "disabled", "upstream_error"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			const raw = `{"model":"claude-test","max_tokens":64,"messages":[],"future":{"value":1e3}}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, _ := io.ReadAll(r.Body)
				if string(body) != raw {
					t.Errorf("request wire changed: %s", body)
				}
				want := "/root/v1/messages"
				if mode == "relative" {
					want = "/root/custom/messages?tenant=a&tenant=b"
				}
				if mode == "absolute" {
					want = "/independent/messages"
				}
				if r.RequestURI != want {
					t.Errorf("request URI=%q, want %q", r.RequestURI, want)
				}
				if r.Header.Get("x-api-key") != "provider-key" {
					t.Error("channel credential not applied")
				}
				w.Header().Set("Content-Type", "application/json")
				if mode == "upstream_error" {
					w.WriteHeader(429)
					_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"retry later"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-test","content":[],"usage":{"input_tokens":1,"output_tokens":1},"future":true}`))
			}))
			defer server.Close()
			previousHTTP := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousHTTP })
			original, request := newNativeClaudeProviderForTest(t, server, raw, nil)
			proxy, baseURL := "", server.URL+"/root"
			setting := providerendpoint.Setting{Enabled: mode != "disabled"}
			if mode == "relative" {
				setting.UpstreamURL = "/custom/messages?tenant=a&tenant=b"
			}
			if mode == "absolute" {
				setting.UpstreamURL = server.URL + "/independent/messages"
				baseURL = "http://127.0.0.1:1"
			}
			plugin := model.NewCustomEndpointPlugin()
			plugin.Data()["endpoints"][providerendpoint.Messages] = setting.Data()
			channel := &model.Channel{Type: config.ChannelTypeCustom, Key: "provider-key", BaseURL: &baseURL, Proxy: &proxy, Plugin: plugin}
			provider := CreateClaudeProvider(channel, "")
			provider.SetContext(original.Context)
			provider.SetOriginalModel(request.Model)
			provider.SetUsage(&types.Usage{})
			response, apiErr := provider.CreateClaudeChat(request)
			if mode == "disabled" {
				if apiErr == nil || calls != 0 {
					t.Fatal("disabled endpoint performed provider work")
				}
				return
			}
			if mode == "upstream_error" {
				if apiErr == nil || apiErr.StatusCode != 429 || calls != 1 {
					t.Fatalf("upstream error changed or retried: %+v calls=%d", apiErr, calls)
				}
				return
			}
			if apiErr != nil || response == nil || calls != 1 {
				t.Fatalf("provider request failed: %+v calls=%d", apiErr, calls)
			}
		})
	}
}
