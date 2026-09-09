package openai

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requestctx"
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

type failingOpenAIRequestBody struct{}

func (failingOpenAIRequestBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (failingOpenAIRequestBody) Close() error             { return nil }

func TestOpenAIRemoteMediaPolicyPassesURLs(t *testing.T) {
	mode, err := (OpenAIProviderFactory{}).AssessChatRemoteMedia(nil, &types.ChatCompletionRequest{}, base.ChatRemoteMediaSummary{Items: 1, RemoteURLs: 1})
	if err != nil || mode != base.RemoteMediaPassURL {
		t.Fatalf("OpenAI remote media policy = %v, %v; want PassURL", mode, err)
	}
}

func TestOpenAIGetRequestHeadersUsesSingleAuthHeader(t *testing.T) {
	t.Run("azure classic uses api key only", func(t *testing.T) {
		proxy := ""
		provider := CreateOpenAIProvider(&model.Channel{Key: "azure-key", Type: config.ChannelTypeAzure, Proxy: &proxy}, "https://example.openai.azure.com")
		provider.IsAzure = true

		headers := provider.GetRequestHeaders()
		if got := headers["api-key"]; got != "azure-key" {
			t.Fatalf("expected api-key header, got %q", got)
		}
		if got := headers["Authorization"]; got != "" {
			t.Fatalf("expected no bearer auth header, got %q", got)
		}
	})

	t.Run("azure v1 uses bearer only", func(t *testing.T) {
		proxy := ""
		provider := CreateOpenAIProvider(&model.Channel{Key: "azure-v1-key", Type: config.ChannelTypeAzureV1, Proxy: &proxy}, "https://example.openai.azure.com")
		provider.IsAzure = true

		headers := provider.GetRequestHeaders()
		if got := headers["Authorization"]; got != "Bearer azure-v1-key" {
			t.Fatalf("expected bearer auth header, got %q", got)
		}
		if got := headers["api-key"]; got != "" {
			t.Fatalf("expected no api-key header, got %q", got)
		}
	})

	t.Run("non azure uses bearer only and filters auth model headers", func(t *testing.T) {
		proxy := ""
		modelHeaders := `{"Authorization":"Bearer should-not-send","api-key":"evil-api-key","X-Gateway-Auth":"gateway-token"}`
		provider := CreateOpenAIProvider(&model.Channel{
			Key:          "sk-test",
			Type:         config.ChannelTypeOpenAI,
			Proxy:        &proxy,
			ModelHeaders: &modelHeaders,
		}, "https://api.openai.com")

		headers := provider.GetRequestHeaders()
		if got := headers["Authorization"]; got != "Bearer sk-test" {
			t.Fatalf("expected channel bearer auth header, got %q", got)
		}
		if got := headers["api-key"]; got != "" {
			t.Fatalf("expected no api-key header, got %q", got)
		}
		if got := headers["X-Gateway-Auth"]; got != "gateway-token" {
			t.Fatalf("expected non-auth custom header to remain, got %q", got)
		}
	})
}

func TestOpenAIWireReplayRequiresTrustedEndpoint(t *testing.T) {
	proxy := ""
	official := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy}, "https://api.openai.com")
	if !official.ProviderRawJSONReplay || !official.Requester.ReplayOpenAIErrorEnvelopes || !official.RequireOpenAIStreamTerminal {
		t.Fatal("official OpenAI endpoint must enable same-dialect response replay")
	}

	compatible := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy}, "https://compatible.example/v1")
	if compatible.ProviderRawJSONReplay || compatible.Requester.ReplayOpenAIErrorEnvelopes || compatible.RequireOpenAIStreamTerminal {
		t.Fatal("an OpenAI-compatible base URL must not inherit trusted replay")
	}

	regional := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy}, "https://eu.api.openai.com")
	if regional.ProviderRawJSONReplay || regional.Requester.ReplayOpenAIErrorEnvelopes || regional.RequireOpenAIStreamTerminal {
		t.Fatal("an unsupported data-residency endpoint must not inherit trusted replay")
	}

	azure := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeAzureV1, Proxy: &proxy}, "https://resource.openai.azure.com")
	if !azure.RequireOpenAIStreamTerminal {
		t.Fatal("the Azure OpenAI Chat SSE adapter must require its explicit terminal")
	}
}

func TestSameDialectRequestHeadersUseBusinessAllowlist(t *testing.T) {
	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(),
		Type:  config.ChannelTypeCustom,
		Key:   "provider-key",
		Proxy: &proxy,
	}, "https://compatible.example/v1")
	headers := http.Header{"Authorization": {"Bearer provider-key"}}
	inbound := requestctx.NewHeaderSnapshot(http.Header{
		"Idempotency-Key":          {"idem-compatible"},
		"OpenAI-Beta":              {"responses_multi_agent=v1"},
		"X-Future-Business":        {"must-not-cross-adapter"},
		"X-Forwarded-Access-Token": {"identity.jwt.secret"},
		"Accept-Encoding":          {"gzip"},
	})
	if err := provider.applyOpenAIHTTPHeaders(headers, inbound); err != nil {
		t.Fatalf("apply headers: %v", err)
	}
	if got := headers.Get("OpenAI-Beta"); got != "responses_multi_agent=v1" {
		t.Fatalf("expected registered business header, got %q", got)
	}
	if got := headers.Get("Idempotency-Key"); got != "idem-compatible" {
		t.Fatalf("same-dialect request lost idempotency key: %q", got)
	}
	for _, name := range []string{"X-Future-Business", "X-Forwarded-Access-Token", "Accept-Encoding"} {
		if got := headers.Get(name); got != "" {
			t.Fatalf("non-exact adapter leaked %s=%q", name, got)
		}
	}
}

func TestAzureClassicRawResourceURLMergesAPIQuery(t *testing.T) {
	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{
		Key:   "azure-key",
		Type:  config.ChannelTypeAzure,
		Other: `{"api_version":"2024-10-01-preview"}`,
		Proxy: &proxy,
	}, "https://resource.openai.azure.com")
	provider.IsAzure = true

	got := provider.GetFullRequestURL("/v1/files?purpose=batch&purpose=fine-tune&limit=1&api-version=client-value", "")
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse Azure classic raw resource URL: %v", err)
	}
	if parsed.Path != "/openai/files" {
		t.Fatalf("expected Azure classic resource path, got %q", parsed.Path)
	}
	query := parsed.Query()
	if query.Get("api-version") != "2024-10-01-preview" || query.Get("limit") != "1" {
		t.Fatalf("expected configured api-version to merge with the raw query, got %q", parsed.RawQuery)
	}
	if purposes := query["purpose"]; len(purposes) != 2 || purposes[0] != "batch" || purposes[1] != "fine-tune" {
		t.Fatalf("expected repeated raw query values to remain, got %#v", purposes)
	}
}

func TestAzureV1ResourceURLPreservesEscapedResponseID(t *testing.T) {
	proxy := ""
	for _, test := range []struct {
		name     string
		baseURL  string
		expected string
	}{
		{
			name:     "resource root",
			baseURL:  "https://resource.openai.azure.com",
			expected: "https://resource.openai.azure.com/openai/v1/responses/resp%2Ftenant?limit=1",
		},
		{
			name:     "gateway prefix",
			baseURL:  "https://resource.openai.azure.com/gateway",
			expected: "https://resource.openai.azure.com/gateway/openai/v1/responses/resp%2Ftenant?limit=1",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := CreateOpenAIProvider(&model.Channel{
				Type:  config.ChannelTypeAzureV1,
				Proxy: &proxy,
			}, test.baseURL)
			provider.IsAzure = true

			got := provider.GetFullRequestURL("/v1/responses/resp%2Ftenant?limit=1", "")
			if got != test.expected {
				t.Fatalf("expected escaped response ID to remain one path segment, got %q", got)
			}
			parsed, err := url.Parse(got)
			if err != nil {
				t.Fatalf("parse Azure V1 URL: %v", err)
			}
			if !strings.HasSuffix(parsed.Path, "/responses/resp/tenant") || !strings.HasSuffix(parsed.EscapedPath(), "/responses/resp%2Ftenant") {
				t.Fatalf("unexpected decoded/escaped paths: path=%q escaped=%q", parsed.Path, parsed.EscapedPath())
			}
		})
	}
}

func TestAzureV1URLMergesBaseAndRequestQuery(t *testing.T) {
	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{
		Type:  config.ChannelTypeAzureV1,
		Proxy: &proxy,
	}, "https://resource.openai.azure.com/gateway?tenant=one&api-version=v1")
	provider.IsAzure = true

	got := provider.GetFullRequestURL("/v1/responses?api-version=preview&feature=one&feature=two", "gpt-5")
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse Azure V1 URL: %v", err)
	}
	if parsed.Path != "/gateway/openai/v1/responses" {
		t.Fatalf("unexpected Azure V1 path: %q", parsed.Path)
	}
	query := parsed.Query()
	if query.Get("tenant") != "one" || query.Get("api-version") != "preview" {
		t.Fatalf("base or request query was lost: %q", parsed.RawQuery)
	}
	if versions := query["api-version"]; len(versions) != 1 {
		t.Fatalf("downstream api-version must replace the base selector without ambiguity: %#v", versions)
	}
	if features := query["feature"]; len(features) != 2 || features[0] != "one" || features[1] != "two" {
		t.Fatalf("repeated request query values changed: %#v", features)
	}
}

func TestBuildRawRelayURLKeepsProviderURLSemantics(t *testing.T) {
	proxy := ""
	tests := []struct {
		name         string
		channelType  int
		baseURL      string
		isAzure      bool
		escapedPath  string
		rawQuery     string
		wantPath     string
		wantSelector string
	}{
		{
			name:        "openai preserves escaped resource path",
			channelType: config.ChannelTypeOpenAI,
			baseURL:     "https://api.openai.example",
			escapedPath: "/v1/files/file%2Ftenant",
			rawQuery:    "purpose=assistants&purpose=batch",
			wantPath:    "/v1/files/file%2Ftenant",
		},
		{
			name:         "azure classic owns resource mapping",
			channelType:  config.ChannelTypeAzure,
			baseURL:      "https://resource.openai.azure.com",
			isAzure:      true,
			escapedPath:  "/v1/files",
			rawQuery:     "purpose=batch",
			wantPath:     "/openai/files",
			wantSelector: "2024-10-01-preview",
		},
		{
			name:         "azure v1 owns gateway mapping",
			channelType:  config.ChannelTypeAzureV1,
			baseURL:      "https://resource.openai.azure.com/gateway?api-version=base",
			isAzure:      true,
			escapedPath:  "/v1/files/file%2Ftenant",
			rawQuery:     "api-version=client&purpose=batch",
			wantPath:     "/gateway/openai/v1/files/file%2Ftenant",
			wantSelector: "client",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel := &model.Channel{
				Type:  test.channelType,
				Proxy: &proxy,
				Other: `{"api_version":"2024-10-01-preview"}`,
			}
			provider := CreateOpenAIProvider(channel, test.baseURL)
			provider.IsAzure = test.isAzure
			got, err := provider.BuildRawRelayURL(test.escapedPath, test.rawQuery)
			if err != nil {
				t.Fatalf("BuildRawRelayURL: %v", err)
			}
			parsed, err := url.Parse(got)
			if err != nil {
				t.Fatalf("parse raw relay URL: %v", err)
			}
			if parsed.EscapedPath() != test.wantPath {
				t.Fatalf("escaped path=%q, want %q (URL=%q)", parsed.EscapedPath(), test.wantPath, got)
			}
			if purposes := parsed.Query()["purpose"]; len(purposes) == 0 || purposes[0] != "batch" && purposes[0] != "assistants" {
				t.Fatalf("raw query was lost: %q", parsed.RawQuery)
			}
			if test.wantSelector != "" {
				if versions := parsed.Query()["api-version"]; len(versions) != 1 || versions[0] != test.wantSelector {
					t.Fatalf("api-version=%#v, want one %q (URL=%q)", versions, test.wantSelector, got)
				}
			}
		})
	}
}

func TestBuildRawRelayURLRejectsUnsupportedCapabilityAndInvalidPath(t *testing.T) {
	proxy := ""
	custom := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Proxy: &proxy}, "https://custom.example")
	if _, err := custom.BuildRawRelayURL("/v1/files", ""); err == nil {
		t.Fatal("expected a channel without raw relay capability to be rejected")
	}

	official := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy}, "https://api.openai.example")
	for _, path := range []string{"", "v1/files", "/v1/files?limit=1", "/v1/files#fragment"} {
		if _, err := official.BuildRawRelayURL(path, ""); err == nil {
			t.Fatalf("expected invalid raw relay path %q to be rejected", path)
		}
	}
}

func TestOpenAIChatNativeWirePatchesRawRequestWithoutReconstructingUnions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	raw := `{
		"model":"client-model",
		"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"custom","custom":{"name":"shell","input":"pwd","future":{"kept":true}}}]}],
		"parallel_tool_calls":false,
		"reasoning_effort":"high",
		"max_tokens":7,
		"stream":false,
		"stream_options":null,
		"future_request_field":{"enabled":true,"large_integer":9007199254740993}
	}`
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(raw))
	c.Request.Header.Add("Idempotency-Key", "idem-chat")
	c.Request.Header.Add("X-Future-Business", "one")
	c.Request.Header.Add("X-Future-Business", "two")
	c.Request.Header.Set("Authorization", "Bearer client-secret")
	c.Request.Header.Set("Connection", "keep-alive")
	if _, err := common.CacheRequestBody(c); err != nil {
		t.Fatalf("cache raw chat request: %v", err)
	}

	var request types.ChatCompletionRequest
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatalf("decode chat projection: %v", err)
	}
	request.MaxCompletionTokens = request.MaxTokens
	request.MaxTokens = 0
	request.NormalizeReasoning()
	if request.Reasoning == nil {
		t.Fatal("test precondition failed: normalized projection must contain nested reasoning")
	}

	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy}, "https://api.openai.com")
	provider.SetContext(c)
	provider.SetOriginalModel("client-model")
	httpRequest, apiErr := provider.GetRequestTextBody(config.RelayModeChatCompletions, "mapped-model", &request)
	if apiErr != nil {
		t.Fatalf("build native chat request: %v", apiErr)
	}
	if got := httpRequest.Header.Get("Idempotency-Key"); got != "idem-chat" {
		t.Fatalf("expected Chat idempotency header, got %q", got)
	}
	if got := httpRequest.Header.Values("X-Future-Business"); len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("expected Chat multi-value business header, got %v", got)
	}
	if got := httpRequest.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Fatalf("client credential replaced provider credential: %q", got)
	}
	if got := httpRequest.Header.Get("Connection"); got != "" {
		t.Fatalf("Chat hop-by-hop header leaked: %q", got)
	}
	body, err := io.ReadAll(httpRequest.Body)
	if err != nil {
		t.Fatalf("read native chat request: %v", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("decode native chat request body: %v body=%s", err, body)
	}
	if string(object["model"]) != `"mapped-model"` {
		t.Fatalf("expected only relay-owned model to be patched, got %s", body)
	}
	if string(object["max_tokens"]) != "7" {
		t.Fatalf("client-owned max_tokens changed: %s", body)
	}
	if _, exists := object["max_completion_tokens"]; exists {
		t.Fatalf("typed normalization leaked max_completion_tokens into exact wire: %s", body)
	}
	if string(object["parallel_tool_calls"]) != "false" || string(object["future_request_field"]) != `{"enabled":true,"large_integer":9007199254740993}` {
		t.Fatalf("expected false and future fields to remain, got %s", body)
	}
	if string(object["stream_options"]) != "null" {
		t.Fatalf("client-owned explicit stream_options changed: %s", body)
	}
	if string(object["reasoning_effort"]) != `"high"` {
		t.Fatalf("exact-wire request lost client reasoning_effort, got %s", body)
	}
	if _, exists := object["reasoning"]; exists {
		t.Fatalf("typed normalization must not inject nested reasoning into exact-wire output, got %s", body)
	}
	if !strings.Contains(string(object["messages"]), `"type":"custom"`) || !strings.Contains(string(object["messages"]), `"future":{"kept":true}`) {
		t.Fatalf("expected custom tool-call union to remain raw, got %s", object["messages"])
	}
}

func TestCustomOpenAICompatibleChatPreservesUnknownWireFieldsByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	raw := `{"model":"client-model","messages":[{"role":"user","content":"hi","future_union":{"kind":"new"}}],"future_request_field":{"large_integer":9007199254740993}}`
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(raw))
	if _, err := common.CacheRequestBody(c); err != nil {
		t.Fatalf("cache raw chat request: %v", err)
	}
	var request types.ChatCompletionRequest
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatalf("decode chat projection: %v", err)
	}
	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "sk-test", Proxy: &proxy}, "https://custom.example")
	provider.SetContext(c)
	provider.SetOriginalModel("client-model")
	httpRequest, apiErr := provider.GetRequestTextBody(config.RelayModeChatCompletions, "mapped-model", &request)
	if apiErr != nil {
		t.Fatalf("build custom chat request: %v", apiErr)
	}
	body, err := io.ReadAll(httpRequest.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if string(object["model"]) != `"mapped-model"` || string(object["future_request_field"]) != `{"large_integer":9007199254740993}` || !strings.Contains(string(object["messages"]), `"future_union":{"kind":"new"}`) {
		t.Fatalf("custom same-dialect request lost wire fields: %s", body)
	}
}

func TestCustomOpenAICompatibleChatPreservesClientStreamOptions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	raw := `{"model":"client-model","stream":true,"stream_options":{"include_usage":false,"future_option":{"enabled":true}},"messages":[{"role":"user","content":"hi"}]}`
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(raw))
	if _, err := common.CacheRequestBody(c); err != nil {
		t.Fatalf("cache raw chat request: %v", err)
	}
	var request types.ChatCompletionRequest
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatalf("decode chat projection: %v", err)
	}
	// Custom providers cannot be assumed to support proxy-forced usage, but the
	// client's same-dialect stream_options remain provider-owned wire fields.
	request.StreamOptions = nil
	proxy := ""
	provider := CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "sk-test", Proxy: &proxy}, "https://custom.example")
	provider.SetContext(c)
	httpRequest, apiErr := provider.GetRequestTextBody(config.RelayModeChatCompletions, "mapped-model", &request)
	if apiErr != nil {
		t.Fatalf("build custom stream request: %v", apiErr)
	}
	body, err := io.ReadAll(httpRequest.Body)
	if err != nil {
		t.Fatalf("read custom stream request: %v", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("decode custom stream request: %v", err)
	}
	var streamOptions map[string]json.RawMessage
	if err := json.Unmarshal(object["stream_options"], &streamOptions); err != nil || string(streamOptions["include_usage"]) != "false" || string(streamOptions["future_option"]) != `{"enabled":true}` {
		t.Fatalf("custom same-dialect request lost client stream_options: %s", body)
	}
}

func TestNativeOpenAIJSONWithoutEffectivePatchPreservesBytes(t *testing.T) {
	raw := []byte("{\n  \"model\": \"gpt-5\", \"future\": 1e3, \"optional\": null\n}")
	tests := []struct {
		name string
		mode int
		req  any
	}{
		{name: "chat", mode: config.RelayModeChatCompletions, req: &types.ChatCompletionRequest{Model: "gpt-5"}},
		{name: "completion", mode: config.RelayModeCompletions, req: &types.CompletionRequest{Model: "gpt-5"}},
		{name: "speech", mode: config.RelayModeAudioSpeech, req: &types.SpeechAudioRequest{Model: "gpt-5"}},
		{name: "embeddings", mode: config.RelayModeEmbeddings, req: &types.EmbeddingRequest{Model: "gpt-5"}},
		{name: "moderations", mode: config.RelayModeModerations, req: &types.ModerationRequest{Model: "gpt-5"}},
		{name: "images", mode: config.RelayModeImagesGenerations, req: &types.ImageRequest{Model: "gpt-5", N: 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(string(raw)))
			proxy := ""
			provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy}, "https://api.openai.com")
			provider.SetContext(ctx)
			provider.SetOriginalModel("gpt-5")
			req, apiErr := provider.GetRequestTextBody(test.mode, "gpt-5", test.req)
			if apiErr != nil {
				t.Fatalf("build request: %+v", apiErr)
			}
			got, err := io.ReadAll(req.Body)
			if err != nil || string(got) != string(raw) {
				t.Fatalf("body changed: err=%v\ngot=%q\nwant=%q", err, got, raw)
			}
		})
	}
}

func TestNativeOpenAIJSONAppliesOnlyEffectiveOwnedPatches(t *testing.T) {
	raw := `{"model":"client-model","temperature":0.25,"future":{"nested": {"duplicate":1,"duplicate":2}, "number":1e3}}`
	for _, test := range []struct {
		name      string
		custom    string
		mapped    string
		wantExact bool
		wantTemp  string
		wantModel string
	}{
		{name: "ineffective custom", custom: `{"temperature":1}`, mapped: "client-model", wantExact: true},
		{name: "effective custom", custom: `{"overwrite":true,"temperature":1}`, mapped: "client-model", wantTemp: "1", wantModel: `"client-model"`},
		{name: "model mapping", mapped: "mapped-model", wantTemp: "0.25", wantModel: `"mapped-model"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(raw))
			proxy := ""
			var custom *string
			if test.custom != "" {
				custom = &test.custom
			}
			provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy, CustomParameter: custom}, "https://api.openai.com")
			provider.SetContext(ctx)
			provider.SetOriginalModel("client-model")
			req, apiErr := provider.GetRequestTextBody(config.RelayModeChatCompletions, test.mapped, &types.ChatCompletionRequest{Model: test.mapped})
			if apiErr != nil {
				t.Fatalf("build request: %+v", apiErr)
			}
			got, _ := io.ReadAll(req.Body)
			if test.wantExact {
				if string(got) != raw {
					t.Fatalf("ineffective patch re-encoded body: %q", got)
				}
				return
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(got, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["model"]) != test.wantModel || string(fields["temperature"]) != test.wantTemp || string(fields["future"]) != `{"nested": {"duplicate":1,"duplicate":2}, "number":1e3}` {
				t.Fatalf("owned patch changed unrelated facts: %s", got)
			}
		})
	}
}

func TestNativeOpenAIJSONFallsBackOnlyWhenNoRawPayloadExists(t *testing.T) {
	proxy := ""
	channel := &model.Channel{Type: config.ChannelTypeOpenAI, Key: "sk-test", Proxy: &proxy}

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	provider := CreateOpenAIProvider(channel, "https://api.openai.com")
	provider.SetContext(ctx)
	provider.SetOriginalModel("gpt-5")
	req, apiErr := provider.GetRequestTextBody(config.RelayModeChatCompletions, "gpt-5", &types.ChatCompletionRequest{Model: "gpt-5", Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}}})
	if apiErr != nil {
		t.Fatalf("typed internal request failed: %+v", apiErr)
	}
	body, _ := io.ReadAll(req.Body)
	if len(body) == 0 || !strings.Contains(string(body), `"model":"gpt-5"`) {
		t.Fatalf("empty raw source suppressed typed request: %q", body)
	}
	common.SetReusableRequestBody(ctx, []byte{})
	req, apiErr = provider.GetRequestTextBody(config.RelayModeChatCompletions, "gpt-5", &types.ChatCompletionRequest{Model: "gpt-5", Messages: []types.ChatCompletionMessage{{Role: "user", Content: "again"}}})
	if apiErr != nil {
		t.Fatalf("cached empty body suppressed later typed request: %+v", apiErr)
	}
	body, _ = io.ReadAll(req.Body)
	if len(body) == 0 || !strings.Contains(string(body), "again") {
		t.Fatalf("cached empty body was treated as raw payload: %q", body)
	}

	failedCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	failedCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	failedCtx.Request.Body = failingOpenAIRequestBody{}
	provider = CreateOpenAIProvider(channel, "https://api.openai.com")
	provider.SetContext(failedCtx)
	provider.SetOriginalModel("gpt-5")
	if _, apiErr = provider.GetRequestTextBody(config.RelayModeChatCompletions, "gpt-5", &types.ChatCompletionRequest{Model: "gpt-5"}); apiErr == nil || apiErr.Code != "build_native_request_failed" {
		t.Fatalf("raw read failure silently fell back to typed body: %+v", apiErr)
	}
}
