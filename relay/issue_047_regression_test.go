package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/azure"
	"one-api/providers/azure_v1"
	"one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const issue047RelayModel = "whisper-1"

type issue047RelayTranscriptionFactory struct {
	name    string
	channel int
	new     func(*model.Channel) base.ProviderInterface
}

func issue047RelayTranscriptionFactories() []issue047RelayTranscriptionFactory {
	return []issue047RelayTranscriptionFactory{
		{
			name:    "openai",
			channel: config.ChannelTypeOpenAI,
			new: func(channel *model.Channel) base.ProviderInterface {
				return openai.OpenAIProviderFactory{}.Create(channel)
			},
		},
		{
			name:    "azure",
			channel: config.ChannelTypeAzure,
			new: func(channel *model.Channel) base.ProviderInterface {
				return azure.AzureProviderFactory{}.Create(channel)
			},
		},
		{
			name:    "azure-v1",
			channel: config.ChannelTypeAzureV1,
			new: func(channel *model.Channel) base.ProviderInterface {
				return azure_v1.AzureV1ProviderFactory{}.Create(channel)
			},
		},
		{
			name:    "custom-openai-compatible",
			channel: config.ChannelTypeCustom,
			new: func(channel *model.Channel) base.ProviderInterface {
				return openai.OpenAIProviderFactory{}.Create(channel)
			},
		},
	}
}

func TestIssue047RelayJSONFallbackKeepsRawBodyAndChargesDuration(t *testing.T) {
	rawResponse := `{"text":"hello","model":"whisper-actual","duration":9,"usage":{"type":"duration","seconds":9},"future":{"keep":true}}`
	for _, factory := range issue047RelayTranscriptionFactories() {
		for _, stream := range []bool{false, true} {
			t.Run(factory.name+"/stream="+map[bool]string{false: "false", true: "true"}[stream], func(t *testing.T) {
				setupIssue047RelayBilling(t, issue047RelayModel, issue047RelayDurationPrice(issue047RelayModel))
				var upstreamCalls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					upstreamCalls.Add(1)
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.Header().Set("X-Request-Id", "i047-json-request")
					w.Header().Set("Cache-Control", "private, max-age=1")
					w.Header().Set("Content-Language", "en")
					w.Header().Set("Digest", "sha-256=i047")
					_, _ = io.WriteString(w, rawResponse)
				}))
				t.Cleanup(server.Close)
				previousHTTPClient := requester.HTTPClient
				requester.HTTPClient = server.Client()
				t.Cleanup(func() { requester.HTTPClient = previousHTTPClient })

				proxy, baseURL := "", server.URL
				channel := &model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: factory.channel, Key: "i047-relay-key", Proxy: &proxy, BaseURL: &baseURL}
				if factory.channel == config.ChannelTypeAzure {
					channel.Other = `{"api_version":"2024-10-01-preview"}`
				}
				provider := factory.new(channel)
				ctx := issue047RelayTranscriptionContext(t, stream)
				provider.SetContext(ctx)
				provider.SetOriginalModel(issue047RelayModel)
				relay := &relayTranscriptions{
					relayBase: relayBase{c: ctx, provider: provider, modelName: issue047RelayModel},
					request:   types.AudioRequest{Model: issue047RelayModel, ResponseFormat: "json", Stream: stream},
				}

				apiErr, _ := RelayHandler(relay)
				if apiErr != nil {
					t.Fatalf("JSON fallback relay failed: %+v body=%s", apiErr, recorderBody(ctx))
				}
				if got := upstreamCalls.Load(); got != 1 {
					t.Fatalf("JSON fallback replayed the provider request: calls=%d", got)
				}
				body := recorderBody(ctx)
				if got := ctx.Writer.Status(); got != http.StatusOK {
					t.Fatalf("JSON fallback downstream status=%d want=%d", got, http.StatusOK)
				}
				if body != rawResponse {
					t.Fatalf("JSON fallback changed downstream body: got=%q want=%q", body, rawResponse)
				}
				var decoded map[string]any
				if err := json.Unmarshal([]byte(body), &decoded); err != nil || decoded["future"] == nil || decoded["text"] != "hello" {
					t.Fatalf("downstream JSON is not parseable or lost unknown fields: err=%v body=%s", err, body)
				}
				for name, want := range map[string]string{
					"Content-Type":     "application/json; charset=utf-8",
					"X-Request-Id":     "i047-json-request",
					"Cache-Control":    "private, max-age=1",
					"Content-Language": "en",
					"Digest":           "sha-256=i047",
				} {
					if got := ctx.Writer.Header().Get(name); got != want {
						t.Fatalf("downstream header %s=%q want %q; headers=%v", name, got, want, ctx.Writer.Header())
					}
				}
				assertIssue047RelayCharge(t, 9, 0, 0)
			})
		}
	}
}

func TestIssue047RelayTokenSSERemainsSSEAndChargesReportedTokens(t *testing.T) {
	const rawSSE = "event: transcript.text.delta\ndata: {\"type\":\"transcript.text.delta\",\"delta\":\"hello\"}\n\n" +
		"event: transcript.text.done\ndata: {\"type\":\"transcript.text.done\",\"model\":\"whisper-sse\",\"usage\":{\"type\":\"tokens\",\"input_tokens\":14,\"output_tokens\":45,\"total_tokens\":59,\"input_token_details\":{\"text_tokens\":0,\"audio_tokens\":14}}}\n\n"
	setupIssue047RelayBilling(t, issue047RelayModel, &model.Price{Model: issue047RelayModel, Type: model.TokensPriceType, Input: 1, Output: 1})
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "i047-sse-request")
		_, _ = io.WriteString(w, rawSSE)
	}))
	t.Cleanup(server.Close)
	previousHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousHTTPClient })

	proxy, baseURL := "", server.URL
	provider := openai.OpenAIProviderFactory{}.Create(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "i047-sse-key", Proxy: &proxy, BaseURL: &baseURL})
	ctx := issue047RelayTranscriptionContext(t, true)
	provider.SetContext(ctx)
	provider.SetOriginalModel(issue047RelayModel)
	relay := &relayTranscriptions{
		relayBase: relayBase{c: ctx, provider: provider, modelName: issue047RelayModel},
		request:   types.AudioRequest{Model: issue047RelayModel, ResponseFormat: "json", Stream: true},
	}

	apiErr, _ := RelayHandler(relay)
	if apiErr != nil {
		t.Fatalf("token SSE relay failed: %+v body=%s", apiErr, recorderBody(ctx))
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("token SSE request was replayed: calls=%d", upstreamCalls.Load())
	}
	if got := ctx.Writer.Status(); got != http.StatusOK {
		t.Fatalf("token SSE downstream status=%d want=%d", got, http.StatusOK)
	}
	if body := recorderBody(ctx); body != rawSSE {
		t.Fatalf("token SSE wire changed: got=%q want=%q", body, rawSSE)
	}
	if got := ctx.Writer.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("token SSE downstream content type=%q", got)
	}
	usage := provider.GetUsage()
	if usage == nil || !usage.HasProviderUsage() || usage.PromptTokens != 14 || usage.CompletionTokens != 45 || usage.TotalTokens != 59 {
		t.Fatalf("token SSE usage did not reach relay billing: %+v", usage)
	}
	assertIssue047RelayCharge(t, 59, 14, 45)
}

func TestIssue047RelayProviderErrorSSEIsNotAcceptedAsSuccess(t *testing.T) {
	const rawError = "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"transcription failed\",\"code\":\"provider_failed\"}}\n\n"
	setupIssue047RelayBilling(t, issue047RelayModel, &model.Price{Model: issue047RelayModel, Type: model.TokensPriceType, Input: 1, Output: 1})
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, rawError)
	}))
	t.Cleanup(server.Close)
	previousHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousHTTPClient })

	proxy, baseURL := "", server.URL
	provider := openai.OpenAIProviderFactory{}.Create(&model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "i047-error-key", Proxy: &proxy, BaseURL: &baseURL})
	ctx := issue047RelayTranscriptionContext(t, true)
	provider.SetContext(ctx)
	provider.SetOriginalModel(issue047RelayModel)
	relay := &relayTranscriptions{
		relayBase: relayBase{c: ctx, provider: provider, modelName: issue047RelayModel},
		request:   types.AudioRequest{Model: issue047RelayModel, ResponseFormat: "json", Stream: true},
	}

	apiErr, _ := RelayHandler(relay)
	if apiErr == nil || !apiErr.UpstreamAccepted {
		t.Fatalf("provider error SSE was accepted as success: %+v body=%s", apiErr, recorderBody(ctx))
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("provider error SSE was replayed: calls=%d", upstreamCalls.Load())
	}
	if body := recorderBody(ctx); strings.Count(body, "event: error") != 1 || !strings.Contains(body, "provider_failed") {
		t.Fatalf("provider error SSE was not delivered once as an error event: %q", body)
	}
	if usage := provider.GetUsage(); usage != nil && usage.HasProviderUsage() {
		t.Fatalf("provider error usage became billable evidence: %+v", usage)
	}
	assertIssue047RelayCharge(t, 0, 0, 0)
}

func setupIssue047RelayBilling(t *testing.T, modelName string, price *model.Price) {
	t.Helper()
	setupRelayTestDB(t, &model.User{}, &model.Token{}, &model.Log{})
	if err := model.DB.Create(&model.User{
		Id: 1, Username: "i047-user", Password: "password123", AccessToken: "i047-access",
		Quota: 100000, Group: "default", Status: config.UserStatusEnabled, Role: config.RoleCommonUser,
	}).Error; err != nil {
		t.Fatalf("create I047 user fixture: %v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id: 1, UserId: 1, Key: "i047-token", Name: "i047-token", Status: config.TokenStatusEnabled,
		ExpiredTime: -1, RemainQuota: 100000, Group: "default",
	}).Error; err != nil {
		t.Fatalf("create I047 token fixture: %v", err)
	}

	oldPricing := model.PricingInstance
	oldBatch, oldRedis, oldLog, oldReserve := config.BatchUpdateEnabled, config.RedisEnabled, config.LogConsumeEnabled, config.PreConsumedQuota
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{modelName: price}}
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	config.LogConsumeEnabled = true
	config.PreConsumedQuota = 50
	t.Cleanup(func() {
		model.PricingInstance = oldPricing
		config.BatchUpdateEnabled = oldBatch
		config.RedisEnabled = oldRedis
		config.LogConsumeEnabled = oldLog
		config.PreConsumedQuota = oldReserve
	})
}

func issue047RelayDurationPrice(modelName string) *model.Price {
	extra := datatypes.NewJSONType(map[string]float64{config.UsageExtraInputAudioTranscription: 1})
	return &model.Price{Model: modelName, Type: model.TokensPriceType, Input: 1, Output: 1, ExtraRatios: &extra}
}

func assertIssue047RelayCharge(t *testing.T, want, wantPrompt, wantCompletion int) {
	t.Helper()
	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 100000-want || token.RemainQuota != 100000-want || user.UsedQuota != want || token.UsedQuota != want {
		t.Fatalf("I047 SQL balances mismatch: user=%+v token=%+v want=%d", user, token, want)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if want == 0 {
		if len(logs) != 0 {
			t.Fatalf("zero-usage provider error wrote consume log: %+v", logs)
		}
		return
	}
	if len(logs) != 1 || logs[0].Quota != want || logs[0].PromptTokens != wantPrompt || logs[0].CompletionTokens != wantCompletion {
		t.Fatalf("I047 SQL consume log mismatch: %+v want quota=%d prompt=%d completion=%d", logs, want, wantPrompt, wantCompletion)
	}
}

func issue047RelayTranscriptionContext(t *testing.T, stream bool) *gin.Context {
	t.Helper()
	var raw bytes.Buffer
	writer := multipart.NewWriter(&raw)
	boundary := writer.Boundary()
	if err := writer.WriteField("model", issue047RelayModel); err != nil {
		t.Fatal(err)
	}
	if stream {
		if err := writer.WriteField("stream", "true"); err != nil {
			t.Fatal(err)
		}
	}
	file, err := writer.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("wave-bytes")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("i047_recorder", recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(raw.Bytes())).WithContext(context.Background())
	ctx.Request.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	ctx.Request.ContentLength = int64(raw.Len())
	if _, err := common.CacheRequestBody(ctx); err != nil {
		t.Fatal(err)
	}
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("group_ratio", 1.0)
	groupctx.SetRoutingGroup(ctx, "default", groupctx.RoutingGroupSourceUserGroup)
	return ctx
}

func recorderBody(ctx *gin.Context) string {
	if ctx == nil {
		return ""
	}
	if recorder, ok := ctx.Get("i047_recorder"); ok {
		if responseRecorder, ok := recorder.(*httptest.ResponseRecorder); ok {
			return responseRecorder.Body.String()
		}
	}
	return ""
}
