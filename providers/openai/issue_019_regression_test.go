package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gorm.io/datatypes"
	"one-api/common/config"
	commonresponses "one-api/common/responses"
	"one-api/model"
	"one-api/types"
)

func TestFixI019_CustomResponsesUsesEffectiveEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name, base, uri, want string
		enabled               bool
	}{
		{"default", "https://base.example", "", "https://base.example/v1/responses", true},
		{"relative", "https://base.example/root/", "/tenant/responses", "https://base.example/root/tenant/responses", true},
		{"double_slash", "https://base.example/root//", "/tenant/responses", "https://base.example/root//tenant/responses", true},
		{"absolute", "https://unused.example", "https://owner.example/tenant/responses", "https://owner.example/tenant/responses", true},
		{"disabled", "https://base.example", "/retained", "", false},
		{"cloudflare", "https://gateway.ai.cloudflare.com/v1/account/gateway/openai/", "/v1/responses", "https://gateway.ai.cloudflare.com/v1/account/gateway/openai/responses", true},
		{"cloudflare_absolute", "https://gateway.ai.cloudflare.com/v1/account/gateway/openai", "https://owner.example/v1/responses", "https://owner.example/v1/responses", true},
		{"query_and_escape", "https://base.example", "/tenant%2Fa/responses?namespace=one&revision=2", "https://base.example/tenant%2Fa/responses?namespace=one&revision=2", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := ""
			plugin := datatypes.NewJSONType(model.PluginType{"endpoints": {"openai.responses": map[string]any{"enabled": tc.enabled, "upstream_url": tc.uri}}})
			channel := &model.Channel{Type: config.ChannelTypeCustom, BaseURL: &tc.base, Proxy: &proxy, Plugin: &plugin}
			provider := CreateOpenAIProvider(channel, channel.GetBaseURL())
			uri, apiErr := provider.GetSupportedAPIUri(config.RelayModeResponses)
			if !tc.enabled {
				if apiErr == nil || uri != "" {
					t.Fatal("disabled endpoint remained usable")
				}
				return
			}
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			if got := provider.GetFullRequestURL(uri, "gpt-4o"); got != tc.want {
				t.Fatalf("URL=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestFixI019_AmbiguousEndpointNeverReachesProvider(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"unexpected","model":"gpt-4o","output":[]}`))
	}))
	defer server.Close()
	proxy := ""
	key := "openai.responses"
	plugin := datatypes.NewJSONType(model.PluginType{"endpoints": {key: map[string]any{"enabled": "true", "upstream_url": server.URL + "/first"}, "0" + key: map[string]any{"enabled": true, "upstream_url": server.URL + "/second"}}})
	channel := &model.Channel{Type: config.ChannelTypeCustom, BaseURL: &server.URL, Key: "test-only", Proxy: &proxy, Plugin: &plugin}
	provider := CreateOpenAIProvider(channel, channel.GetBaseURL())
	provider.SetUsage(&types.Usage{})
	body, err := commonresponses.ParseRawEnvelope([]byte(`{"model":"gpt-4o","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, apiErr := provider.CreateResponses(context.Background(), &commonresponses.Request{Operation: commonresponses.ResponsesCreate, Body: body, Model: "gpt-4o"})
	if apiErr == nil || calls.Load() != 0 {
		t.Fatalf("歧义端点未在上游工作前拒绝: err=%v calls=%d", apiErr, calls.Load())
	}
}

func TestFixI019_CustomResponsesRealtimeModelKeepsWireNamespace(t *testing.T) {
	const baseURL = "http://127.0.0.1:8181/root"
	for _, test := range []struct{ name, uri, want string }{
		{name: "relative", uri: "/tenant/responses", want: "ws://127.0.0.1:8181/root/tenant/responses?model=fixture-realtime"},
		{name: "absolute", uri: baseURL + "/tenant/responses", want: "ws://127.0.0.1:8181/roothttp://127.0.0.1:8181/root/tenant/responses?model=fixture-realtime"},
	} {
		t.Run(test.name, func(t *testing.T) {
			proxy, base := "", baseURL
			plugin := datatypes.NewJSONType(model.PluginType{"endpoints": {"openai.responses": map[string]any{"enabled": true, "upstream_url": test.uri}}})
			channel := &model.Channel{Type: config.ChannelTypeCustom, BaseURL: &base, Proxy: &proxy, Plugin: &plugin, Other: `{"responses_ws_native":true,"responses_ws_self_hosted":true}`}
			provider := CreateOpenAIProvider(channel, channel.GetBaseURL())
			actual, apiErr := provider.responsesWSURL("fixture-realtime")
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			if actual != test.want {
				t.Fatalf("既有模型名分支的wire地址变化: got=%q want=%q", actual, test.want)
			}
		})
	}
}

func TestFixI019_CustomResponsesHTTPAndWSKeepTheirWirePaths(t *testing.T) {
	const baseURL = "http://127.0.0.1:8181/root"
	for _, test := range []struct{ name, uri, httpURL, wsURL string }{
		{name: "relative_leading_space", uri: " /tenant/responses", httpURL: baseURL + "/tenant/responses", wsURL: "ws://127.0.0.1:8181/root%20/tenant/responses"},
		{name: "relative_trailing_space", uri: "/tenant/responses ", httpURL: baseURL + "/tenant/responses", wsURL: "ws://127.0.0.1:8181/root/tenant/responses"},
		{name: "absolute_encoded_space", uri: baseURL + "/tenant/responses%20", httpURL: baseURL + "/tenant/responses%20", wsURL: "ws://127.0.0.1:8181/root/tenant/responses%20"},
	} {
		t.Run(test.name, func(t *testing.T) {
			proxy := ""
			base := baseURL
			plugin := datatypes.NewJSONType(model.PluginType{"endpoints": {"openai.responses": map[string]any{"enabled": true, "upstream_url": test.uri}}})
			channel := &model.Channel{Type: config.ChannelTypeCustom, Key: "test-only", BaseURL: &base, Proxy: &proxy, Plugin: &plugin, Other: `{"responses_ws_native":true,"responses_ws_self_hosted":true}`}
			provider := CreateOpenAIProvider(channel, channel.GetBaseURL())
			body, err := commonresponses.ParseRawEnvelope([]byte(`{"model":"gpt-4o","input":"hi"}`))
			if err != nil {
				t.Fatal(err)
			}
			request, apiErr := provider.buildResponsesCreateRequest(&commonresponses.Request{Operation: commonresponses.ResponsesCreate, Body: body, Model: "gpt-4o"}, &body.Projection, false)
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			defer request.Body.Close()
			if got := request.URL.String(); got != test.httpURL {
				t.Fatalf("HTTP实际URL=%q，期望%q", got, test.httpURL)
			}
			wsURL, apiErr := provider.responsesWSURL("gpt-4o")
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			if wsURL != test.wsURL {
				t.Fatalf("WS实际URL=%q，期望%q", wsURL, test.wsURL)
			}
		})
	}
}
