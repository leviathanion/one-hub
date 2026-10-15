package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/antigravity"
	"one-api/providers/antigravity/wire"
	providersBase "one-api/providers/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

const antigravityRelayResponse = `{"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":2,"totalTokenCount":17}}}`

func antigravityRelayChannel(t *testing.T, id int, endpoint string, expired bool) *model.Channel {
	t.Helper()
	channel := capabilitySelectionChannel(id, config.ChannelTypeAntigravity, toolUsageEvidenceHTTPModel)
	channel.BaseURL = &endpoint
	expiry := time.Now().Add(time.Hour).Unix()
	if expired {
		expiry = time.Now().Add(-time.Hour).Unix()
	}
	creds := &antigravity.Credentials{AccessToken: "ag-access-token", RefreshToken: "ag-refresh-token", Subject: "ag-subject", ProjectID: "ag-project", ExpiresAt: expiry}
	var err error
	channel.Key, err = creds.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	return channel
}

func antigravityIngressContext(t *testing.T, path, body string, channels []*model.Channel) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	ctx := capabilitySelectionContext(t, path, body, channels)
	recorder := httptest.NewRecorder()
	writing, _ := gin.CreateTestContext(recorder)
	ctx.Writer = writing.Writer
	enableResponsesTestDeadline(ctx)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("group_ratio", 1.0)
	ctx.Set("requestStartTime", time.Now())
	groupctx.SetRoutingGroup(ctx, "default", groupctx.RoutingGroupSourceUserGroup)
	if strings.HasPrefix(path, "/gemini/") {
		ctx.Params = gin.Params{{Key: "version", Value: "v1beta"}, {Key: "model", Value: strings.TrimPrefix(path, "/gemini/v1beta/models/")}}
	}
	return ctx, recorder
}

func antigravityIngressBilling(t *testing.T, channels ...*model.Channel) {
	t.Helper()
	setupResponsesHTTPBillingFixture(t)
	if err := model.DB.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatal(err)
	}
	for _, channel := range channels {
		if err := model.DB.Create(channel).Error; err != nil {
			t.Fatal(err)
		}
	}
	model.PricingInstance.Prices[toolUsageEvidenceHTTPModel].Input = 1
	model.PricingInstance.Prices[toolUsageEvidenceHTTPModel].Output = 2
}

func assertAntigravityUsageSettled(t *testing.T) {
	t.Helper()
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].PromptTokens != 10 || logs[0].CompletionTokens != 7 || logs[0].Quota <= 0 {
		t.Fatalf("provider usage未进入唯一结算：%+v", logs)
	}
	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.UsedQuota != logs[0].Quota || user.RequestCount != 1 {
		t.Fatalf("结算未保持一次记账：user=%+v log=%+v", user, logs[0])
	}
}

func TestAntigravityFourIngressesUseFactoryAndSettleProviderUsage(t *testing.T) {
	for _, protocol := range []string{"chat", "claude", "gemini", "responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", protocol, stream), func(t *testing.T) {
				var calls atomic.Int32
				const actualModel = "actual-priced-model"
				response := strings.Replace(antigravityRelayResponse, `"response":{`, `"response":{"modelVersion":"`+actualModel+`",`, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Header.Get("Authorization") != "Bearer ag-access-token" {
						t.Errorf("unexpected provider auth")
					}
					if strings.Contains(r.URL.Path, "streamGenerateContent") {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: "+response+"\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, response)
					}
				}))
				t.Cleanup(server.Close)
				previousClient := requester.HTTPClient
				requester.HTTPClient = server.Client()
				t.Cleanup(func() { requester.HTTPClient = previousClient })
				channel := antigravityRelayChannel(t, 901, server.URL, false)
				antigravityIngressBilling(t, channel)
				model.PricingInstance.Prices[actualModel] = &model.Price{Model: actualModel, Type: model.TokensPriceType, Input: 3, Output: 4}
				path, body := "/v1/chat/completions", fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":%t}`, toolUsageEvidenceHTTPModel, stream)
				switch protocol {
				case "claude":
					path = "/claude/v1/messages"
					body = fmt.Sprintf(`{"model":%q,"max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":null}]}],"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":null},"tools":[{"name":"lookup","input_schema":{"type":"object"},"cache_control":null,"eager_input_streaming":false}],"stream":%t}`, toolUsageEvidenceHTTPModel, stream)
				case "responses":
					path = "/v1/responses"
					body = fmt.Sprintf(`{"model":%q,"input":"hi","store":false,"stream":%t}`, toolUsageEvidenceHTTPModel, stream)
				case "gemini":
					action := "generateContent"
					if stream {
						action = "streamGenerateContent"
					}
					path = "/gemini/v1beta/models/" + toolUsageEvidenceHTTPModel + ":" + action
					body = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"future":1e+3}`
				}
				ctx, recorder := antigravityIngressContext(t, path, body, []*model.Channel{channel})
				var selected RelayBaseInterface
				previousHandler := relayHandlerFunc
				relayHandlerFunc = func(r RelayBaseInterface) (*types.OpenAIErrorWithStatusCode, bool) {
					selected = r
					return RelayHandler(r)
				}
				t.Cleanup(func() { relayHandlerFunc = previousHandler })
				Relay(ctx)
				if recorder.Code != 200 || calls.Load() != 1 || !strings.Contains(recorder.Body.String(), "hello") {
					t.Fatalf("入口未完成单次发送：status=%d calls=%d body=%s", recorder.Code, calls.Load(), recorder.Body.String())
				}
				if selected == nil {
					t.Fatal("未进入统一relay执行链")
				}
				if _, ok := selected.getProvider().(*antigravity.AntigravityProvider); !ok {
					t.Fatalf("factory选择错误：%T", selected.getProvider())
				}
				if r, ok := selected.(*relayResponses); ok && (r.selectedCreateTarget != providersBase.ResponsesCreateTargetProvider || r.preparedChatRequest != nil || channel.CompatibleResponse) {
					t.Fatalf("Responses误入Chat fallback：%+v", r)
				}
				assertAntigravityUsageSettled(t)
				var log model.Log
				if err := model.DB.Where("type = ?", model.LogTypeConsume).First(&log).Error; err != nil {
					t.Fatal(err)
				}
				if log.Quota != 10*3+7*4 || log.Metadata.Data()["billing_model"] != actualModel {
					t.Fatalf("未按上游实际模型价格结算：%+v metadata=%+v", log, log.Metadata.Data())
				}
			})
		}
	}
}

type antigravityBlockedTransport struct{ calls atomic.Int32 }

func (r *antigravityBlockedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls.Add(1)
	return nil, errors.New("unexpected provider or OAuth HTTP")
}

func TestAntigravityUnsupportedIngressStopsBeforeCredentialsOrProviderWork(t *testing.T) {
	carrier, err := wire.EncodeCarrier([]wire.CarrierPart{{Index: 0, Kind: "signature", Signature: json.RawMessage(`"opaque"`)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, path, body string
		channelType      int
	}{
		{"stored responses", "/v1/responses", `{"model":"gpt-5.6","input":"hi","store":true}`, config.ChannelTypeAntigravity},
		{"default stored responses", "/v1/responses", `{"model":"gpt-5.6","input":"hi"}`, config.ChannelTypeAntigravity},
		{"unknown carrier version", "/v1/chat/completions", `{"model":"gpt-5.6","messages":[{"role":"assistant","content":"hi","extra_content":{"antigravity_state":"ag9.invalid"}}]}`, config.ChannelTypeAntigravity},
		{"foreign carrier", "/v1/chat/completions", fmt.Sprintf(`{"model":"gpt-5.6","messages":[{"role":"assistant","content":"hi","extra_content":{"antigravity_state":%q}}]}`, carrier), config.ChannelTypeOpenAI},
		{"cross protocol search", "/claude/v1/messages", `{"model":"gpt-5.6","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`, config.ChannelTypeAntigravity},
		{"hidden summary continuation", "/claude/v1/messages", `{"model":"gpt-5.6","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"adaptive","display":"omitted"}}`, config.ChannelTypeAntigravity},
		{"unverified effort", "/claude/v1/messages", `{"model":"gpt-5.6","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"high"}}`, config.ChannelTypeAntigravity},
		{"cache lifetime", "/claude/v1/messages", `{"model":"gpt-5.6","max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`, config.ChannelTypeAntigravity},
		{"eager delivery", "/claude/v1/messages", `{"model":"gpt-5.6","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f","input_schema":{},"eager_input_streaming":true}]}`, config.ChannelTypeAntigravity},
		{"eager header default", "/claude/v1/messages", `{"model":"gpt-5.6","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f","input_schema":{},"eager_input_streaming":null}]}`, config.ChannelTypeAntigravity},
	} {
		t.Run(test.name, func(t *testing.T) {
			blocked := &antigravityBlockedTransport{}
			previous := requester.HTTPClient
			requester.HTTPClient = &http.Client{Transport: blocked}
			t.Cleanup(func() { requester.HTTPClient = previous })
			channel := antigravityRelayChannel(t, 902, "https://unreachable.invalid", true)
			channel.Type = test.channelType
			ctx, recorder := antigravityIngressContext(t, test.path, test.body, []*model.Channel{channel})
			Relay(ctx)
			if recorder.Code < 400 || blocked.calls.Load() != 0 || ctx.GetInt("channel_id") != 0 || !strings.Contains(recorder.Body.String(), "unsupported_capability") {
				t.Fatalf("unsupported跨过provider边界：status=%d calls=%d channel=%d body=%s", recorder.Code, blocked.calls.Load(), ctx.GetInt("channel_id"), recorder.Body.String())
			}
		})
	}
}

func TestAntigravityNativeErrorDoesNotReplayOrSwitchChannel(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"code":429,"message":"quota exceeded","status":"RESOURCE_EXHAUSTED","details":[{"future":1e3}]}}`)
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })
	first := antigravityRelayChannel(t, 903, server.URL, false)
	second := antigravityRelayChannel(t, 904, server.URL, false)
	antigravityIngressBilling(t, first, second)
	previousRetry, previousTimeout := config.RetryTimes, config.RetryTimeOut
	config.RetryTimes, config.RetryTimeOut = 3, 60
	t.Cleanup(func() { config.RetryTimes, config.RetryTimeOut = previousRetry, previousTimeout })
	previousObserver := processChannelRelayErrorFunc
	processChannelRelayErrorFunc = func(context.Context, int, string, *types.OpenAIErrorWithStatusCode, int) {}
	t.Cleanup(func() { processChannelRelayErrorFunc = previousObserver })
	ctx, recorder := antigravityIngressContext(t, "/gemini/v1beta/models/"+toolUsageEvidenceHTTPModel+":generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, []*model.Channel{first, second})
	Relay(ctx)
	if recorder.Code != 429 || calls.Load() != 1 || ctx.GetInt("channel_id") != first.Id || !strings.Contains(recorder.Body.String(), `"future":1e3`) {
		t.Fatalf("原生错误丢失或发生重放：status=%d calls=%d channel=%d body=%s", recorder.Code, calls.Load(), ctx.GetInt("channel_id"), recorder.Body.String())
	}
}

func TestAntigravityInterruptedStreamRetainsUsageWithoutReplayOrSuccessTerminal(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":2,"totalTokenCount":17}}}`+"\n\n")
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })
	first := antigravityRelayChannel(t, 905, server.URL, false)
	second := antigravityRelayChannel(t, 906, server.URL, false)
	antigravityIngressBilling(t, first, second)
	previousRetry, previousTimeout := config.RetryTimes, config.RetryTimeOut
	config.RetryTimes, config.RetryTimeOut = 3, 60
	t.Cleanup(func() { config.RetryTimes, config.RetryTimeOut = previousRetry, previousTimeout })
	previousObserver := processChannelRelayErrorFunc
	processChannelRelayErrorFunc = func(context.Context, int, string, *types.OpenAIErrorWithStatusCode, int) {}
	t.Cleanup(func() { processChannelRelayErrorFunc = previousObserver })
	ctx, recorder := antigravityIngressContext(t, "/v1/chat/completions", `{"model":"gpt-5.6","stream":true,"messages":[{"role":"user","content":"hi"}]}`, []*model.Channel{first, second})
	Relay(ctx)
	body := recorder.Body.String()
	if calls.Load() != 1 || ctx.GetInt("channel_id") != first.Id || !strings.Contains(body, "hello") || !strings.Contains(body, "error") || strings.Contains(body, "[DONE]") {
		t.Fatalf("截断流发生重放或伪成功：calls=%d channel=%d body=%s", calls.Load(), ctx.GetInt("channel_id"), body)
	}
	assertAntigravityUsageSettled(t)
}
