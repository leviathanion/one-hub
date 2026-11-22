package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/providerendpoint"
	"one-api/common/requestctx"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"
)

func TestDecisionsPreservesWireAndAccountsForProviderUsage(t *testing.T) {
	const input = ` { "input":[{"future": [1e3, 9007199254740993]}], "model":"alias", "questions":[{"future":{"choice": 42}}] } `
	const result = ` { "answers":[{"future":[1e3, 9007199254740993]}], "model":"reported", "usage":{"input_tokens":100,"output_tokens":6,"total_tokens":106,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":10},"output_tokens_details":{"reasoning_tokens":2}} } `
	for _, tc := range []struct {
		name, mapped, params string
		custom               bool
	}{
		{name: "original", mapped: "alias"},
		{name: "mapped", mapped: "decision-realtime-future"},
		{name: "post_add", mapped: "mapped", params: `{"per_model":true,"mapped":{"added":"ok","model":"wrong"}}`},
		{name: "custom_pre_add", mapped: "mapped", params: `{"pre_add":true,"per_model":true,"mapped":{"added":"ok","model":"wrong"}}`, custom: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantPath := "/v1/decisions"
			var wire string
			var headers http.Header
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.RequestURI() != wantPath {
					t.Errorf("request %s %s", r.Method, r.URL)
				}
				body, _ := io.ReadAll(r.Body)
				wire, headers = string(body), r.Header.Clone()
				w.Header().Set("X-Request-ID", "req_607aff")
				w.Header().Set("Cache-Control", "private, max-age=60")
				w.Header().Set("OpenAI-Organization", "org-private")
				w.Header().Set("Set-Cookie", "internal=private")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, result)
			}))
			t.Cleanup(server.Close)
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })
			proxy := ""
			ch := &model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy, Key: "60", CustomParameter: &tc.params}
			if tc.custom {
				ch.Type, ch.Plugin = config.ChannelTypeCustom, model.NewCustomEndpointPlugin()
				wantPath = "/tenant%2Fa/decide?test=1&test=2"
				ch.Plugin.Data()["endpoints"][providerendpoint.Decisions] = (providerendpoint.Setting{Enabled: true, UpstreamURL: server.URL + wantPath}).Data()
			}
			p := CreateOpenAIProvider(ch, server.URL)
			p.SetProviderRawJSONReplay(!tc.custom)
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(input))
			ctx.Request.Header.Set("Content-Digest", "sha-256=:example:")
			ctx.Request.Header.Set("Idempotency-Key", "keep")
			ctx.Request.Header.Set("Authorization", "Bearer client")
			if _, err := common.CacheRequestBody(ctx); err != nil {
				t.Fatal(err)
			}
			p.SetContext(ctx)
			p.SetOriginalModel("alias")
			p.SetUsage(&types.Usage{})
			response, apiErr := p.CreateDecision(tc.mapped, json.RawMessage(input))
			if apiErr != nil || string(response.ReplayProviderRawJSON()) != result {
				t.Fatalf("response=%+v err=%v", response, apiErr)
			}
			if tc.name == "original" {
				if wire != input || headers.Get("Content-Digest") == "" {
					t.Fatalf("original request or digest changed: %q, %v", wire, headers)
				}
			} else {
				if !strings.Contains(wire, `"model":"`+tc.mapped+`"`) || headers.Get("Content-Digest") != "" || !strings.Contains(wire, `[1e3, 9007199254740993]`) || !strings.Contains(wire, `{"choice": 42}`) {
					t.Fatalf("mapped request or digest incorrect: %q, %v", wire, headers)
				}
			}
			if tc.params != "" && !strings.Contains(wire, `"added":"ok"`) {
				t.Errorf("missing channel parameter: %q", wire)
			}
			if headers.Get("Authorization") != "Bearer 60" || headers.Get("Idempotency-Key") != "keep" {
				t.Errorf("request headers: %v", headers)
			}
			responseHeaders := ctx.MustGet(requestctx.ProviderResponseHeadersContextKey).(http.Header)
			if responseHeaders.Get("X-Request-ID") != "req_607aff" || responseHeaders.Get("Cache-Control") != "private, max-age=60" ||
				responseHeaders.Get("OpenAI-Organization") != "" || responseHeaders.Get("Set-Cookie") != "" || ctx.GetInt(requestctx.ProviderResponseStatusContextKey) != http.StatusCreated {
				t.Errorf("response status or headers: %v", responseHeaders)
			}
			u := p.Usage
			if !u.HasProviderUsage() || u.ResponseModel != "reported" || u.PromptTokens != 100 || u.CompletionTokens != 6 || u.TotalTokens != 106 || u.PromptTokensDetails.CachedTokens != 40 || u.PromptTokensDetails.CacheWriteTokens != 10 || u.CompletionTokensDetails.ReasoningTokens != 2 {
				t.Fatalf("usage: %+v", u)
			}
		})
	}
}

func TestDecisionsMissingOrUnknownUsageDoesNotBlockDelivery(t *testing.T) {
	previousClient := requester.HTTPClient
	requester.HTTPClient = http.DefaultClient
	t.Cleanup(func() { requester.HTTPClient = previousClient })
	proxy := ""
	for _, fields := range []string{"", `,"usage":null`, `,"usage":{}`, `,"usage":"future"`, `,"usage":{"input_tokens":12,"output_tokens":"invalid"}`} {
		wire := `{ "model":"actual", "answers":{"future":true}` + fields + ` }`
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, wire)
		}))
		p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy}, server.URL)
		p.SetUsage(&types.Usage{})
		response, apiErr := p.CreateDecision("alias", json.RawMessage(`{"model":"alias"}`))
		server.Close()
		if apiErr != nil || string(response.ReplayProviderRawJSON()) != wire || p.Usage.ProviderReported || p.Usage.ResponseModel != "actual" {
			t.Fatalf("wire=%s err=%+v usage=%+v", wire, apiErr, p.Usage)
		}
	}
}

func TestDecisionsObservesUsageIndependentlyOfOpaqueJSON(t *testing.T) {
	const modelField = `"model":"actual"`
	const tokens = `"input_tokens":12,"output_tokens":3`
	const usageField = `"usage":{` + tokens + `,"input_tokens_details":{"cached_tokens":1}}`
	deep := strings.Repeat("[", 65) + "null" + strings.Repeat("]", 65)
	for _, test := range []struct {
		name, fields, wantModel string
		wantTokens, wantCache   bool
	}{
		{"答案扩展内部重名", modelField + `,"answers":[{"type":"choice","choice":"yes","extension":{"tag":1,"tag":2}}],` + usageField, "actual", true, true},
		{"用量扩展内部重名", modelField + `,"usage":{` + tokens + `,"extension":{"tag":1,"tag":2}}`, "actual", true, false},
		{"缓存明细扩展内部重名", modelField + `,"usage":{` + tokens + `,"input_tokens_details":{"cached_tokens":1,"extension":{"tag":1,"tag":2}}}`, "actual", true, true},
		{"深层业务扩展", modelField + `,"extension":` + deep + `,` + usageField, "actual", true, true},
		{"模型字段重名", modelField + `,"model":"another",` + usageField, "", false, false},
		{"用量字段重名", modelField + `,` + usageField + `,"usage":{` + tokens + `}`, "", false, false},
		{"基础用量重名", modelField + `,"usage":{` + tokens + `,"input_tokens":13}`, "actual", false, false},
		{"缓存用量重名", modelField + `,"usage":{` + tokens + `,"input_tokens_details":{"cached_tokens":1,"cached_tokens":2}}`, "actual", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			wire := ` {` + test.fields + `} `
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, wire)
			}))
			t.Cleanup(server.Close)
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })
			proxy := ""
			provider := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy}, server.URL)
			provider.SetUsage(&types.Usage{})
			response, apiErr := provider.CreateDecision("alias", json.RawMessage(`{"model":"alias"}`))
			if apiErr != nil || string(response.ReplayProviderRawJSON()) != wire {
				t.Fatalf("用量观察改变上游正文：response=%+v err=%v", response, apiErr)
			}
			usage := provider.Usage
			if usage.ResponseModel != test.wantModel || usage.HasProviderBaseUsage() != test.wantTokens ||
				usage.ProviderTokenFields[config.UsageExtraCache] != test.wantCache {
				t.Fatalf("上游用量观察结果错误：%+v", usage)
			}
			if test.wantTokens && (usage.PromptTokens != 12 || usage.CompletionTokens != 3) {
				t.Fatalf("基础用量改变：%+v", usage)
			}
			if test.wantCache && usage.PromptTokensDetails.CachedTokens != 1 {
				t.Fatalf("缓存用量改变：%+v", usage)
			}
		})
	}
}

func TestDecisionsPreservesProviderFailureAndAcceptedReadFailure(t *testing.T) {
	previousClient := requester.HTTPClient
	requester.HTTPClient = http.DefaultClient
	t.Cleanup(func() { requester.HTTPClient = previousClient })
	const body = ` { "error":{"type":"invalid_request_error","message":"unknown question"}, "future":true } `
	for _, status := range []int{http.StatusUnprocessableEntity, http.StatusTemporaryRedirect, http.StatusOK} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			if status == http.StatusTemporaryRedirect {
				w.Header().Set("Location", "/second")
			}
			w.WriteHeader(status)
			if status == http.StatusOK {
				_, _ = io.WriteString(w, `{ "answers":`)
				return
			}
			_, _ = io.WriteString(w, body)
		}))
		proxy := ""
		p := CreateOpenAIProvider(&model.Channel{Type: config.ChannelTypeOpenAI, Proxy: &proxy}, server.URL)
		_, apiErr := p.CreateDecision("alias", json.RawMessage(`{"model":"alias"}`))
		server.Close()
		if calls != 1 || apiErr == nil {
			t.Fatalf("status=%d calls=%d err=%+v", status, calls, apiErr)
		}
		if status == http.StatusOK {
			if !apiErr.UpstreamAccepted || apiErr.Code != "decode_response_failed" {
				t.Fatalf("lost acceptance evidence: %+v", apiErr)
			}
		} else if apiErr.StatusCode != status || !apiErr.ReplayRawResponse || string(apiErr.RawBody) != body {
			t.Fatalf("status=%d err=%+v, body=%q", status, apiErr, apiErr.RawBody)
		}
	}
}
