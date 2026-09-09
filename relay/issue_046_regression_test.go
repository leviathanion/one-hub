package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/openai"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const issue046RelayModel = "gpt-4o-transcribe-diarize"

func TestIssue046RelayFinalGateAcceptsDiarizedJSONForSharedFactories(t *testing.T) {
	for _, test := range []struct {
		name    string
		channel int
	}{
		{name: "openai", channel: config.ChannelTypeOpenAI},
		{name: "azure", channel: config.ChannelTypeAzure},
		{name: "azure-v1", channel: config.ChannelTypeAzureV1},
		{name: "custom-openai-compatible", channel: config.ChannelTypeCustom},
		{name: "siliconflow", channel: config.ChannelTypeSiliconflow},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := issue046RelayTranscriptionContext(t)
			relay := NewRelayTranscriptions(ctx)
			if err := relay.setRequest(); err != nil {
				t.Fatalf("parse diarized transcription request: %v", err)
			}
			proxy, baseURL := "", "http://127.0.0.1:1"
			channel := &model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: test.channel, Key: "i046-key", Proxy: &proxy, BaseURL: &baseURL}
			if test.channel == config.ChannelTypeAzure {
				channel.Other = `{"api_version":"2024-10-01-preview"}`
			}
			capability := currentRequestChannelCapability(ctx)
			if capability == nil {
				t.Fatal("transcription final gate was not registered")
			}
			if err := capability(channel); err != nil {
				t.Fatalf("final transcription gate rejected diarized_json: %v", err)
			}
		})
	}
}

func TestIssue046RelayFinalGateRejectsInvalidFormatAndUnsupportedStream(t *testing.T) {
	for _, test := range []struct {
		name    string
		channel int
		mutate  func(*relayTranscriptions)
	}{
		{
			name:    "invalid format",
			channel: config.ChannelTypeCustom,
			mutate: func(relay *relayTranscriptions) {
				relay.request.ResponseFormat = "xml"
			},
		},
		{
			name:    "siliconflow stream",
			channel: config.ChannelTypeSiliconflow,
			mutate: func(relay *relayTranscriptions) {
				relay.request.Stream = true
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := issue046RelayTranscriptionContext(t)
			relay := NewRelayTranscriptions(ctx)
			if err := relay.setRequest(); err != nil {
				t.Fatalf("parse transcription request: %v", err)
			}
			test.mutate(relay)
			proxy, baseURL := "", "http://127.0.0.1:1"
			channel := &model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: test.channel, Key: "i046-key", Proxy: &proxy, BaseURL: &baseURL}
			if capability := currentRequestChannelCapability(ctx); capability == nil {
				t.Fatal("transcription final gate was not registered")
			} else if err := capability(channel); err == nil {
				t.Fatal("final transcription gate accepted an unsupported request")
			}
		})
	}
}

func TestIssue046RelayDiarizedJSONPreservesSpeakerWireAndChargesDuration(t *testing.T) {
	const rawResponse = `{"task":"transcribe","text":"hello","segments":[{"id":0,"speaker":"A","text":"hello"}],"speaker_labels":[{"speaker":"A","start":0,"end":1}],"model":"diarized-actual","usage":{"type":"duration","seconds":9},"future":{"keep":true}}`
	setupIssue046RelayBilling(t)
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Request-Id", "i046-diarized")
		w.Header().Set("Cache-Control", "private, max-age=1")
		w.Header().Set("Content-Language", "en")
		w.Header().Set("Digest", "sha-256=i046")
		_, _ = io.WriteString(w, rawResponse)
	}))
	t.Cleanup(server.Close)
	previousHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousHTTPClient })

	proxy, baseURL := "", server.URL
	channel := &model.Channel{Plugin: model.NewCustomEndpointPlugin(), Type: config.ChannelTypeCustom, Key: "i046-key", Proxy: &proxy, BaseURL: &baseURL}
	provider := openai.OpenAIProviderFactory{}.Create(channel)
	ctx := issue046RelayTranscriptionContext(t)
	provider.SetContext(ctx)
	provider.SetOriginalModel(issue046RelayModel)
	relay := NewRelayTranscriptions(ctx)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("parse diarized transcription request: %v", err)
	}
	relay.provider = provider
	relay.modelName = issue046RelayModel
	capability := currentRequestChannelCapability(ctx)
	if capability == nil {
		t.Fatal("transcription final gate was not registered")
	}
	if err := capability(channel); err != nil {
		t.Fatalf("final gate rejected valid diarized_json: %v", err)
	}

	apiErr, _ := RelayHandler(relay)
	if apiErr != nil {
		t.Fatalf("diarized JSON relay failed: %+v", apiErr)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("diarized JSON relay replayed provider request: calls=%d", upstreamCalls.Load())
	}
	if got := ctx.Writer.Status(); got != http.StatusOK {
		t.Fatalf("diarized JSON relay status=%d want=%d", got, http.StatusOK)
	}
	body := issue046RelayRecorderBody(ctx)
	if body != rawResponse {
		t.Fatalf("diarized JSON relay re-encoded body: got=%q want=%q", body, rawResponse)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil || decoded["segments"] == nil || decoded["speaker_labels"] == nil || decoded["future"] == nil {
		t.Fatalf("speaker/unknown fields were lost from relay JSON: err=%v body=%s", err, body)
	}
	for name, want := range map[string]string{
		"Content-Type":     "application/json; charset=utf-8",
		"X-Request-Id":     "i046-diarized",
		"Cache-Control":    "private, max-age=1",
		"Content-Language": "en",
		"Digest":           "sha-256=i046",
	} {
		if got := ctx.Writer.Header().Get(name); got != want {
			t.Fatalf("relay response header %s=%q want %q; headers=%v", name, got, want, ctx.Writer.Header())
		}
	}
	usage := provider.GetUsage()
	if usage == nil || usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 ||
		!usage.ProviderIndependentUsageUnits[config.UsageExtraInputAudioTranscription] ||
		usage.ExtraUsageUnits[config.UsageExtraInputAudioTranscription] != 9 || usage.ResponseModel != "diarized-actual" {
		t.Fatalf("diarized usage did not reach Attempt: %+v", usage)
	}
	assertIssue046RelayBilling(t, 9)
}

func setupIssue046RelayBilling(t *testing.T) {
	t.Helper()
	setupRelayTestDB(t, &model.User{}, &model.Token{}, &model.Log{})
	if err := model.DB.Create(&model.User{
		Id: 1, Username: "i046-user", Password: "password123", AccessToken: "i046-access",
		Quota: 100000, Group: "default", Status: config.UserStatusEnabled, Role: config.RoleCommonUser,
	}).Error; err != nil {
		t.Fatalf("create I046 user fixture: %v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id: 1, UserId: 1, Key: "i046-token", Name: "i046-token", Status: config.TokenStatusEnabled,
		ExpiredTime: -1, RemainQuota: 100000, Group: "default",
	}).Error; err != nil {
		t.Fatalf("create I046 token fixture: %v", err)
	}
	extra := datatypes.NewJSONType(map[string]float64{config.UsageExtraInputAudioTranscription: 1})
	oldPricing := model.PricingInstance
	oldBatch, oldRedis, oldLog, oldReserve := config.BatchUpdateEnabled, config.RedisEnabled, config.LogConsumeEnabled, config.PreConsumedQuota
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		issue046RelayModel: {Model: issue046RelayModel, Type: model.TokensPriceType, Input: 1, Output: 1, ExtraRatios: &extra},
	}}
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

func assertIssue046RelayBilling(t *testing.T, want int) {
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
		t.Fatalf("I046 SQL balance mismatch: user=%+v token=%+v want=%d", user, token, want)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Quota != want || logs[0].PromptTokens != 0 || logs[0].CompletionTokens != 0 {
		t.Fatalf("I046 duration SQL log mismatch: %+v want=%d", logs, want)
	}
}

func issue046RelayTranscriptionContext(t *testing.T) *gin.Context {
	t.Helper()
	var raw bytes.Buffer
	writer := multipart.NewWriter(&raw)
	boundary := writer.Boundary()
	if err := writer.WriteField("model", issue046RelayModel); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("response_format", "diarized_json"); err != nil {
		t.Fatal(err)
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
	ctx.Set("i046_recorder", recorder)
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

func issue046RelayRecorderBody(ctx *gin.Context) string {
	if value, ok := ctx.Get("i046_recorder"); ok {
		if recorder, ok := value.(*httptest.ResponseRecorder); ok {
			return recorder.Body.String()
		}
	}
	return ""
}
