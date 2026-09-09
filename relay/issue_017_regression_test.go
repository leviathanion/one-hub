package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/openai"
	relayUtil "one-api/relay/relay_util"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	issue017SpeechModel            = "gpt-4o-mini-tts"
	issue017SpeechActualModel      = "gpt-4o-mini-tts-actual"
	issue017SpeechDonePayload      = `{"type":"speech.audio.done","usage":{"type":"tokens","input_tokens":14,"output_tokens":101,"total_tokens":115,"input_token_details":{"text_tokens":4,"audio_tokens":10}},"model":"gpt-4o-mini-tts-actual","service_tier":"priority","future":{"kept":true}}`
	issue017SpeechErrorDonePayload = `{"type":"speech.audio.done","usage":{"type":"tokens","input_tokens":14,"output_tokens":101,"total_tokens":115},"model":"gpt-4o-mini-tts-actual","error":{"type":"provider_error","message":"speech failed"}}`
)

func TestIssue017SpeechUsageReachesRelayAttemptAndSQL(t *testing.T) {
	validWire := "event: speech.audio.delta\ndata: {\"type\":\"speech.audio.delta\",\"delta\":\"AQ==\",\"future\":{\"kept\":true}}\n\n" +
		"event: speech.audio.done\ndata: " + issue017SpeechDonePayload + "\n\n"
	for _, test := range []struct {
		name        string
		stream      bool
		body        string
		contentType string
		confirmed   bool
		charge      int
	}{
		{name: "valid SSE usage", stream: true, body: validWire, contentType: "text/event-stream", confirmed: true, charge: 115},
		{name: "missing usage", stream: true, body: "event: speech.audio.done\ndata: {\"type\":\"speech.audio.done\",\"model\":\"gpt-4o-mini-tts-actual\"}\n\n", contentType: "text/event-stream"},
		{name: "conflicting usage", stream: true, body: "event: speech.audio.done\ndata: {\"type\":\"speech.audio.done\",\"usage\":{\"type\":\"tokens\",\"input_tokens\":14,\"output_tokens\":101,\"total_tokens\":999},\"model\":\"gpt-4o-mini-tts-actual\"}\n\n", contentType: "text/event-stream"},
		{name: "negative usage", stream: true, body: "event: speech.audio.done\ndata: {\"type\":\"speech.audio.done\",\"usage\":{\"type\":\"tokens\",\"input_tokens\":-14,\"output_tokens\":101,\"total_tokens\":87},\"model\":\"gpt-4o-mini-tts-actual\"}\n\n", contentType: "text/event-stream"},
		{name: "binary audio", body: "audio-bytes", contentType: "audio/mpeg"},
	} {
		t.Run(test.name, func(t *testing.T) {
			setupIssue017Billing(t)
			var upstreamCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				w.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			proxy := ""
			baseURL := server.URL
			provider := openai.CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(),
				Type: config.ChannelTypeCustom, Key: "i017-test-key", Proxy: &proxy, BaseURL: &baseURL,
			}, server.URL)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil).WithContext(context.Background())
			ctx.Set("id", 1)
			ctx.Set("token_id", 1)
			ctx.Set("group_ratio", 1.0)
			groupctx.SetRoutingGroup(ctx, "default", groupctx.RoutingGroupSourceUserGroup)
			provider.SetContext(ctx)
			relay := &relaySpeech{
				relayBase: relayBase{c: ctx, provider: provider, modelName: issue017SpeechModel},
				request: types.SpeechAudioRequest{
					Model: issue017SpeechModel, Input: "hello", Voice: []byte(`"alloy"`),
					StreamFormat: map[bool]string{true: "sse", false: ""}[test.stream],
				},
			}

			apiErr, _ := RelayHandler(relay)
			if apiErr != nil {
				t.Fatalf("Speech relay failed: %+v; body=%s", apiErr, recorder.Body.String())
			}
			if upstreamCalls.Load() != 1 {
				t.Fatalf("expected exactly one upstream request, got %d", upstreamCalls.Load())
			}
			usage := provider.GetUsage()
			if test.confirmed {
				if !usage.HasProviderUsage() || usage.PromptTokens != 14 || usage.CompletionTokens != 101 || usage.TotalTokens != 115 {
					t.Fatalf("valid Speech usage was not observed: %+v", usage)
				}
				if usage.ResponseModel != issue017SpeechActualModel || usage.ServiceTier != "priority" || usage.PromptTokensDetails.TextTokens != 4 || usage.PromptTokensDetails.AudioTokens != 10 {
					t.Fatalf("Speech usage attribution/details were not preserved: %+v", usage)
				}
				if recorder.Body.String() != test.body {
					t.Fatalf("Speech SSE wire changed:\nwant %q\n got %q", test.body, recorder.Body.String())
				}
				// A repeated terminal snapshot replaces the previous snapshot; it
				// must not turn 14+101 into 28+202.
				provider.ObserveSpeechEvent([]byte(issue017SpeechDonePayload))
				if usage.TotalTokens != 115 || usage.PromptTokens != 14 || usage.CompletionTokens != 101 {
					t.Fatalf("repeated terminal usage was accumulated: %+v", usage)
				}
			} else if usage.HasProviderUsage() {
				t.Fatalf("missing/conflicting/binary usage became billable evidence: %+v", usage)
			}

			assertIssue017SQLSettlement(t, usage, test.stream, test.confirmed, test.charge)
		})
	}
}

func TestIssue017SpeechObserverRunsBeforeClosedClientDelivery(t *testing.T) {
	var observed atomic.Int32
	var badPayload atomic.Bool
	handler := newAudioSSEHandler(audioSSESpeech, func(payload []byte) {
		if string(payload) != issue017SpeechDonePayload {
			badPayload.Store(true)
		}
		observed.Add(1)
	})
	// A zero emitter models a downstream that cannot accept the event.  The
	// observer must still see the complete terminal payload first.
	for _, line := range []string{
		"event: speech.audio.done\n",
		"data: " + issue017SpeechDonePayload + "\n",
		"\n",
	} {
		rawLine := []byte(line)
		handler.Handle(&rawLine, requester.StreamEmitter[string]{})
	}
	if observed.Load() != 1 || badPayload.Load() {
		t.Fatalf("complete Speech usage was not observed before downstream close: %d", observed.Load())
	}
}

func TestIssue017SpeechDirectObserverRejectsErrorPayload(t *testing.T) {
	for _, payload := range []string{
		`{"type":"error","usage":{"type":"tokens","input_tokens":14,"output_tokens":101,"total_tokens":115}}`,
		issue017SpeechErrorDonePayload,
	} {
		provider := &openai.OpenAIProvider{}
		provider.SetUsage(&types.Usage{})
		provider.ObserveSpeechEvent([]byte(payload))
		if provider.GetUsage().HasProviderUsage() {
			t.Fatalf("error payload became billable through direct observer: %s usage=%+v", payload, provider.GetUsage())
		}
	}
}

func TestIssue017SpeechErrorUsageIsRefunded(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "error event with done payload", body: "event: error\ndata: " + issue017SpeechDonePayload + "\n\n"},
		{name: "done payload with nonnull error", body: "event: speech.audio.done\ndata: " + issue017SpeechErrorDonePayload + "\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			setupIssue017Billing(t)
			var upstreamCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			proxy := ""
			baseURL := server.URL
			provider := openai.CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(),
				Type: config.ChannelTypeCustom, Key: "i017-error-key", Proxy: &proxy, BaseURL: &baseURL,
			}, server.URL)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil).WithContext(context.Background())
			ctx.Set("id", 1)
			ctx.Set("token_id", 1)
			ctx.Set("group_ratio", 1.0)
			groupctx.SetRoutingGroup(ctx, "default", groupctx.RoutingGroupSourceUserGroup)
			provider.SetContext(ctx)
			relay := &relaySpeech{
				relayBase: relayBase{c: ctx, provider: provider, modelName: issue017SpeechModel},
				request: types.SpeechAudioRequest{
					Model: issue017SpeechModel, Input: "hello", Voice: []byte(`"alloy"`), StreamFormat: "sse",
				},
			}
			apiErr, _ := RelayHandler(relay)
			if apiErr == nil {
				t.Fatalf("provider error SSE was accepted: body=%s", recorder.Body.String())
			}
			if upstreamCalls.Load() != 1 {
				t.Fatalf("expected exactly one upstream request, got %d", upstreamCalls.Load())
			}
			if provider.GetUsage().HasProviderUsage() {
				t.Fatalf("error usage became billable evidence: %+v", provider.GetUsage())
			}
			assertIssue017SQLSettlement(t, provider.GetUsage(), true, false, 0)
		})
	}
}

func TestIssue017SpeechCloseIsIdempotent(t *testing.T) {
	setupIssue017Billing(t)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil).WithContext(context.Background())
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("group_ratio", 1.0)
	groupctx.SetRoutingGroup(ctx, "default", groupctx.RoutingGroupSourceUserGroup)
	usage := &types.Usage{
		PromptTokens: 14, CompletionTokens: 101, TotalTokens: 115,
		ResponseModel: issue017SpeechActualModel, ServiceTier: "priority",
	}
	usage.MarkProviderReported()
	attempt, err := relayUtil.NewAttemptQuota(ctx, issue017SpeechModel, 5, relayUtil.BillingAttemptSpec{LogProtocol: relayUtil.LogProtocolHTTPStream})
	if err != nil {
		t.Fatalf("create Speech billing attempt: %v", err)
	}
	if err := attempt.ApplyReserve(ctx.Request.Context()); err != nil {
		t.Fatalf("reserve Speech billing quota: %v", err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatalf("claim Speech submission: %v", err)
	}
	first, err := attempt.CloseFromProviderResult(ctx.Request.Context(), usage, true)
	if err != nil || !first.Confirmed || first.ChargedQuota != 115 {
		t.Fatalf("first Speech settlement failed: result=%+v err=%v", first, err)
	}
	second, err := attempt.CloseFromProviderResult(ctx.Request.Context(), usage, true)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated Close changed Speech settlement: first=%+v second=%+v err=%v", first, second, err)
	}
	assertIssue017BalancesAndLogs(t, 115, true)
}

func setupIssue017Billing(t *testing.T) {
	t.Helper()
	setupRelayTestDB(t, &model.User{}, &model.Token{}, &model.Log{})
	if err := model.DB.Create(&model.User{
		Id: 1, Username: "i017-user", Password: "password123", AccessToken: "i017-access",
		Quota: 100000, Group: "default", Status: config.UserStatusEnabled, Role: config.RoleCommonUser,
	}).Error; err != nil {
		t.Fatalf("create Speech billing user: %v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id: 1, UserId: 1, Key: "i017-token", Name: "i017-token", Status: config.TokenStatusEnabled,
		ExpiredTime: -1, RemainQuota: 100000, Group: "default",
	}).Error; err != nil {
		t.Fatalf("create Speech billing token: %v", err)
	}

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalRedis := config.RedisEnabled
	originalReserve := config.PreConsumedQuota
	originalLog := config.LogConsumeEnabled
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		issue017SpeechModel:       {Model: issue017SpeechModel, Type: model.TokensPriceType, Input: 1, Output: 1},
		issue017SpeechActualModel: {Model: issue017SpeechActualModel, Type: model.TokensPriceType, Input: 1, Output: 1},
	}}
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	config.PreConsumedQuota = 50
	config.LogConsumeEnabled = true
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.RedisEnabled = originalRedis
		config.PreConsumedQuota = originalReserve
		config.LogConsumeEnabled = originalLog
	})
}

func assertIssue017SQLSettlement(t *testing.T, usage *types.Usage, isStream, confirmed bool, charge int) {
	t.Helper()
	assertIssue017BalancesAndLogs(t, charge, confirmed)
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatalf("read Speech consume logs: %v", err)
	}
	if !confirmed {
		return
	}
	if len(logs) != 1 || logs[0].Quota != charge || logs[0].PromptTokens != 14 || logs[0].CompletionTokens != 101 || logs[0].ModelName != issue017SpeechModel || logs[0].IsStream != isStream {
		t.Fatalf("Speech consume log mismatch: logs=%+v usage=%+v", logs, usage)
	}
}

func assertIssue017BalancesAndLogs(t *testing.T, charge int, confirmed bool) {
	t.Helper()
	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("read Speech settled user: %v", err)
	}
	var token model.Token
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("read Speech settled token: %v", err)
	}
	if user.Quota != 100000-charge || user.UsedQuota != charge || token.RemainQuota != 100000-charge || token.UsedQuota != charge {
		t.Fatalf("Speech SQL balances mismatch: user=%+v token=%+v charge=%d", user, token, charge)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatalf("read Speech consume logs: %v", err)
	}
	wantLogs := 0
	if confirmed {
		wantLogs = 1
	}
	if len(logs) != wantLogs {
		t.Fatalf("Speech consume log count=%d want=%d logs=%+v", len(logs), wantLogs, logs)
	}
}
