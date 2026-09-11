package relay

import (
	"context"
	"errors"
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
	commonresponses "one-api/common/responses"
	"one-api/model"
	"one-api/providers/openai"
	relayUtil "one-api/relay/relay_util"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const issue034Model = "gpt-5"

const issue034ResponsesUsage = `"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":3,"text_tokens":97},"output_tokens_details":{"reasoning_tokens":4,"text_tokens":16}}`

func TestFixI034ResponsesUsageReachesAttemptAndSQL(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		downstream    commonresponses.DownstreamDialect
		stream        bool
		wantBodyText  string
		upstreamBody  string
		observeEvents bool
	}{
		{
			name:         "Responses unary",
			downstream:   commonresponses.DownstreamResponses,
			upstreamBody: `{"id":"resp_i034_unary","object":"response","model":"gpt-5","service_tier":"flex","status":"completed","output":[],` + issue034ResponsesUsage + `}`,
		},
		{
			name:          "Responses native stream",
			downstream:    commonresponses.DownstreamResponses,
			stream:        true,
			wantBodyText:  "hello",
			observeEvents: true,
			upstreamBody:  issue034ResponsesStreamBody("resp_i034_stream"),
		},
		{
			name:         "Responses-only Chat stream",
			downstream:   commonresponses.DownstreamChatCompletions,
			stream:       true,
			wantBodyText: "hello",
			upstreamBody: issue034ResponsesStreamBody("resp_i034_chat"),
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, attempt := setupI034Billing(t, testCase.stream)
			var upstreamCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				if testCase.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				_, _ = io.WriteString(w, testCase.upstreamBody)
			}))
			t.Cleanup(server.Close)

			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			proxy := ""
			baseURL := server.URL
			provider := openai.CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(),
				Type: config.ChannelTypeCustom, Key: "i034-provider-key", Proxy: &proxy, BaseURL: &baseURL,
			}, server.URL)
			provider.SetContext(ctx)
			provider.SetUsage(&types.Usage{})

			var delivered string
			if testCase.stream {
				stream, apiErr := provider.CreateResponsesStream(context.Background(), issue034ResponsesRequest(t, testCase.downstream, true))
				if apiErr != nil {
					t.Fatalf("CreateResponsesStream failed: %+v", apiErr)
				}
				delivered = drainI034ResponsesStream(t, stream, testCase.observeEvents)
			} else {
				response, apiErr := provider.CreateResponses(context.Background(), issue034ResponsesRequest(t, testCase.downstream, false))
				if apiErr != nil {
					t.Fatalf("CreateResponses failed: %+v", apiErr)
				}
				if response == nil || response.Status != types.ResponseStatusCompleted {
					t.Fatalf("unary response was not completed: %+v", response)
				}
				if response.Usage == nil || response.Usage.InputTokens != 100 || response.Usage.OutputTokens != 20 || response.Usage.TotalTokens != 120 {
					t.Fatalf("unary provider usage changed: %+v", response.Usage)
				}
				delivered = response.Status
			}

			if upstreamCalls.Load() != 1 {
				t.Fatalf("expected one upstream request, got %d", upstreamCalls.Load())
			}
			if testCase.wantBodyText != "" && !strings.Contains(delivered, testCase.wantBodyText) {
				t.Fatalf("successful stream did not deliver %q: %s", testCase.wantBodyText, delivered)
			}

			usage := provider.GetUsage()
			if !usage.HasProviderUsage() || usage.PromptTokens != 100 || usage.CompletionTokens != 20 || usage.TotalTokens != 120 {
				t.Fatalf("accepted Responses usage did not reach provider Usage: %+v", usage)
			}
			if usage.ResponseModel != issue034Model || usage.ServiceTier != "flex" || usage.PromptTokensDetails.CachedTokens != 3 || usage.CompletionTokensDetails.ReasoningTokens != 4 {
				t.Fatalf("Responses usage attribution/details changed: %+v", usage)
			}

			result, err := attempt.CloseFromProviderResult(ctx.Request.Context(), usage, testCase.stream)
			if err != nil || result.Unsettled || !result.Confirmed || result.ChargedQuota != 120 {
				t.Fatalf("provider usage did not settle through Attempt: result=%+v err=%v usage=%+v", result, err, usage)
			}
			assertI034BalancesAndLog(t, 120, testCase.stream)
		})
	}
}

func TestFixI034ResponsesNegativeStreamUsageDoesNotAuthorizeBilling(t *testing.T) {
	for _, testCase := range []struct {
		name string
		body string
	}{
		{
			name: "missing usage",
			body: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_i034_missing\",\"model\":\"gpt-5\",\"service_tier\":\"flex\",\"status\":\"completed\"}}\n\ndata: [DONE]\n\n",
		},
		{
			name: "invalid usage",
			body: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_i034_invalid\",\"status\":\"completed\",\"usage\":{\"input_tokens\":\"100\",\"output_tokens\":20,\"total_tokens\":120}}}\n\ndata: [DONE]\n\n",
		},
		{
			name: "empty terminal",
			body: "data: {\"type\":\"response.completed\",\"response\":{}}\n\ndata: [DONE]\n\n",
		},
		{
			name: "partial image with usage",
			body: "data: {\"type\":\"response.image_generation_call.partial_image\",\"item_id\":\"image_i034\",\"output_index\":0,\"partial_image_index\":0,\"item\":{\"type\":\"image_generation_call\",\"id\":\"image_i034\",\"status\":\"in_progress\"},\"response\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":20,\"total_tokens\":120}}}\n\ndata: [DONE]\n\n",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, attempt := setupI034Billing(t, true)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, testCase.body)
			}))
			t.Cleanup(server.Close)

			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			proxy := ""
			baseURL := server.URL
			provider := openai.CreateOpenAIProvider(&model.Channel{Plugin: model.NewCustomEndpointPlugin(),
				Type: config.ChannelTypeCustom, Key: "i034-provider-key", Proxy: &proxy, BaseURL: &baseURL,
			}, server.URL)
			provider.SetContext(ctx)
			provider.SetUsage(&types.Usage{})
			stream, apiErr := provider.CreateResponsesStream(context.Background(), issue034ResponsesRequest(t, commonresponses.DownstreamResponses, true))
			if apiErr != nil {
				t.Fatalf("CreateResponsesStream failed: %+v", apiErr)
			}
			_ = drainI034ResponsesStream(t, stream, true)
			usage := provider.GetUsage()
			if usage.ProviderReported || usage.HasProviderUsage() {
				t.Fatalf("negative stream usage became provider evidence: %+v", usage)
			}
			result, err := attempt.CloseFromProviderResult(ctx.Request.Context(), usage, true)
			if err != nil || result.Confirmed || result.Unsettled || result.ChargedQuota != 0 {
				t.Fatalf("negative stream usage was charged: result=%+v err=%v usage=%+v", result, err, usage)
			}
			assertI034BalancesAndLog(t, 0, true)
		})
	}
}

func setupI034Billing(t *testing.T, stream bool) (*gin.Context, *relayUtil.AttemptQuota) {
	t.Helper()
	setupRelayTestDB(t, &model.User{}, &model.Token{}, &model.Log{})
	if err := model.DB.Create(&model.User{
		Id: 1, Username: "i034-user", Password: "password123", AccessToken: "i034-access",
		Quota: 100000, Group: "default", Status: config.UserStatusEnabled, Role: config.RoleCommonUser,
	}).Error; err != nil {
		t.Fatalf("create I034 user fixture: %v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id: 1, UserId: 1, Key: "i034-token", Name: "i034-token", Status: config.TokenStatusEnabled,
		ExpiredTime: -1, RemainQuota: 100000, Group: "default",
	}).Error; err != nil {
		t.Fatalf("create I034 token fixture: %v", err)
	}

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalRedis := config.RedisEnabled
	originalReserve := config.PreConsumedQuota
	originalLog := config.LogConsumeEnabled
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		issue034Model: {Model: issue034Model, Type: model.TokensPriceType, Input: 1, Output: 1},
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

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	path := "/v1/responses"
	if stream {
		path += "?stream=true"
	}
	ctx.Request = httptest.NewRequest(http.MethodPost, path, nil).WithContext(context.Background())
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("group_ratio", 1.0)
	groupctx.SetRoutingGroup(ctx, "default", groupctx.RoutingGroupSourceUserGroup)
	attempt, err := relayUtil.NewAttemptQuota(ctx, issue034Model, 0, relayUtil.BillingAttemptSpec{
		LogProtocol: map[bool]string{true: relayUtil.LogProtocolHTTPStream, false: relayUtil.LogProtocolHTTP}[stream],
	})
	if err != nil {
		t.Fatalf("create I034 Attempt: %v", err)
	}
	if err := attempt.ApplyReserve(ctx.Request.Context()); err != nil {
		t.Fatalf("reserve I034 quota: %v", err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatalf("claim I034 submission: %v", err)
	}
	return ctx, attempt
}

func issue034ResponsesStreamBody(responseID string) string {
	return "data: {\"type\":\"response.created\",\"response\":{\"id\":\"" + responseID + "\",\"model\":\"gpt-5\",\"service_tier\":\"flex\",\"status\":\"in_progress\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"" + responseID + "\",\"model\":\"gpt-5\",\"service_tier\":\"flex\",\"status\":\"completed\"," + issue034ResponsesUsage + "}}\n\n" +
		"data: [DONE]\n\n"
}

func issue034ResponsesRequest(t *testing.T, downstream commonresponses.DownstreamDialect, stream bool) *commonresponses.Request {
	t.Helper()
	envelope, err := commonresponses.ParseRawEnvelope([]byte(`{"model":"gpt-5","input":"hello"}`))
	if err != nil {
		t.Fatalf("parse I034 Responses request: %v", err)
	}
	return &commonresponses.Request{
		Operation: commonresponses.ResponsesCreate,
		Body:      envelope,
		Control:   commonresponses.Control{DownstreamDialect: downstream, Stream: stream},
		Model:     issue034Model,
	}
}

func drainI034ResponsesStream(t *testing.T, stream commonresponses.EventStream, observe bool) string {
	t.Helper()
	if stream == nil {
		t.Fatal("Responses stream is nil")
	}
	defer requester.CloseAndDrainStream(stream)
	dataChan, errChan := stream.Recv()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var delivered strings.Builder
	for dataChan != nil || errChan != nil {
		select {
		case data, ok := <-dataChan:
			if !ok {
				dataChan = nil
				continue
			}
			delivered.WriteString(data)
			if observe {
				if err := stream.ObserveResponsesEvent(data); err != nil {
					t.Fatalf("accepted Responses event failed: %v", err)
				}
			}
		case err, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("Responses stream failed: %v", err)
			}
		case <-timer.C:
			t.Fatal("waiting for I034 Responses stream timed out")
		}
	}
	return delivered.String()
}

func assertI034BalancesAndLog(t *testing.T, charge int, stream bool) {
	t.Helper()
	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("read I034 user settlement: %v", err)
	}
	var token model.Token
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("read I034 token settlement: %v", err)
	}
	if user.Quota != 100000-charge || user.UsedQuota != charge || token.RemainQuota != 100000-charge || token.UsedQuota != charge {
		t.Fatalf("I034 SQL balances mismatch: user=%+v token=%+v charge=%d", user, token, charge)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatalf("read I034 consume logs: %v", err)
	}
	wantLogs := 0
	if charge > 0 {
		wantLogs = 1
	}
	if len(logs) != wantLogs {
		t.Fatalf("I034 consume log count=%d want=%d logs=%+v", len(logs), wantLogs, logs)
	}
	if charge > 0 && (logs[0].Quota != charge || logs[0].PromptTokens != 100 || logs[0].CompletionTokens != 20 || logs[0].ModelName != issue034Model || logs[0].IsStream != stream) {
		t.Fatalf("I034 consume log mismatch: %+v", logs[0])
	}
}
