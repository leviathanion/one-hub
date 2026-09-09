package relay_util

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/openrouter"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestI049OpenRouterSearchUsageReachesAttemptAndSQL(t *testing.T) {
	const (
		modelName  = "openrouter/auto"
		unaryBody  = `{"id":"or-unary","object":"chat.completion","model":"openrouter/auto","service_tier":"priority","choices":[{"index":0,"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":0,"total_tokens":30,"server_tool_use":{"web_search_requests":2}}}`
		streamWire = "data: {\"id\":\"or-stream\",\"object\":\"chat.completion.chunk\",\"model\":\"openrouter/auto\",\"service_tier\":\"priority\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"},\"finish_reason\":null}],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":0,\"total_tokens\":30,\"server_tool_use\":{\"web_search_requests\":1}}}\n\ndata: {\"id\":\"or-stream\",\"object\":\"chat.completion.chunk\",\"model\":\"openrouter/auto\",\"service_tier\":\"priority\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":0,\"total_tokens\":30,\"server_tool_use\":{\"web_search_requests\":2}}}\n\ndata: [DONE]\n\n"
	)
	for _, test := range []struct {
		name      string
		stream    bool
		wire      string
		searches  int
		charge    int64
		confirmed bool
	}{
		{name: "stream_2次搜索", stream: true, wire: streamWire, searches: 2, charge: 10030, confirmed: true},
		{name: "unary_2次搜索", wire: unaryBody, searches: 2, charge: 10030, confirmed: true},
		{name: "stream_零搜索", stream: true, wire: openRouterUsageStream(`{"prompt_tokens":30,"completion_tokens":0,"total_tokens":30,"server_tool_use":{"web_search_requests":0}}`), charge: 30, confirmed: true},
		{name: "stream_缺少计数", stream: true, wire: openRouterUsageStream(`{"prompt_tokens":30,"completion_tokens":0,"total_tokens":30}`), charge: 30, confirmed: true},
		{name: "stream_负计数", stream: true, wire: openRouterUsageStream(`{"prompt_tokens":30,"completion_tokens":0,"total_tokens":30,"server_tool_use":{"web_search_requests":-1}}`), charge: 30, confirmed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				_, _ = io.WriteString(w, test.wire)
			}))
			defer server.Close()

			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })
			proxy, baseURL := "", server.URL
			provider := openrouter.OpenRouterProviderFactory{}.Create(&model.Channel{
				Type:    config.ChannelTypeOpenRouter,
				Key:     "test-key",
				Proxy:   &proxy,
				BaseURL: &baseURL,
			}).(*openrouter.OpenRouterProvider)
			provider.SetUsage(&types.Usage{})
			provider.ReasoningHandler = false
			request := &types.ChatCompletionRequest{Model: modelName, Stream: test.stream, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "find this"}}}
			if test.stream {
				stream, apiErr := provider.CreateChatCompletionStream(request)
				if apiErr != nil {
					t.Fatalf("OpenRouter stream 失败：%+v", apiErr)
				}
				drainUsageFixtureStream(t, stream)
			} else {
				response, apiErr := provider.CreateChatCompletion(request)
				if apiErr != nil || response == nil {
					t.Fatalf("OpenRouter unary 失败：response=%+v err=%+v", response, apiErr)
				}
			}

			usage := provider.GetUsage()
			key := types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "")
			if usage.PromptTokens != 30 || usage.CompletionTokens != 0 || usage.TotalTokens != 30 {
				t.Fatalf("token usage 错误：%+v", usage)
			}
			if usage.ExtraBilling[key].CallCount != test.searches || (test.searches > 0) != usage.HasProviderExtraBilling(key) {
				t.Fatalf("搜索证据错误：usage=%+v searches=%d", usage, test.searches)
			}
			settleI049ProviderFixtureUsage(t, usage, modelName, test.stream, test.confirmed, test.charge, test.searches)
		})
	}
}

func openRouterUsageStream(usage string) string {
	return "data: {\"id\":\"or-stream\",\"object\":\"chat.completion.chunk\",\"model\":\"openrouter/auto\",\"service_tier\":\"priority\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":" + usage + "}\n\ndata: [DONE]\n\n"
}

func settleI049ProviderFixtureUsage(t *testing.T, usage *types.Usage, modelName string, isStream, confirmed bool, charge int64, searches int) {
	t.Helper()
	useQuotaReserveTestDB(t)
	if err := model.DB.AutoMigrate(&model.Log{}); err != nil {
		t.Fatalf("迁移消费日志表失败：%v", err)
	}
	insertQuotaReserveFixtures(t, 100000)

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalRedis := config.RedisEnabled
	originalReserve := config.PreConsumedQuota
	originalLog := config.LogConsumeEnabled
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		modelName: {Model: modelName, Type: model.TokensPriceType, Input: 1, Output: 1},
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

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	setQuotaTestRoutingGroup(t, ctx, 1)
	protocol := LogProtocolHTTP
	if isStream {
		protocol = LogProtocolHTTPStream
	}
	attempt, err := NewAttemptQuota(ctx, modelName, 10, BillingAttemptSpec{LogProtocol: protocol})
	if err != nil {
		t.Fatalf("创建 billing attempt 失败：%v", err)
	}
	if err := attempt.ApplyReserve(context.Background()); err != nil {
		t.Fatalf("预扣失败：%v", err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatalf("记录提交失败：%v", err)
	}
	result, err := attempt.CloseFromProviderResult(context.Background(), usage, isStream)
	if err != nil || result.Unsettled || result.Confirmed != confirmed || result.ChargedQuota != charge {
		t.Fatalf("adapter → reducer → SQL 结算错误：result=%+v err=%v usage=%+v", result, err, usage)
	}
	second, err := attempt.CloseFromProviderResult(context.Background(), usage, isStream)
	if err != nil || second != result {
		t.Fatalf("重复收尾改变结算：first=%+v second=%+v err=%v", result, second, err)
	}

	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("读取 user 失败：%v", err)
	}
	var token model.Token
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("读取 token 失败：%v", err)
	}
	if int64(user.Quota) != 100000-charge || int64(token.RemainQuota) != 100000-charge || int64(token.UsedQuota) != charge {
		t.Fatalf("SQL 余额/预扣释放不一致：user=%d token=%d used=%d charge=%d", user.Quota, token.RemainQuota, token.UsedQuota, charge)
	}

	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatalf("读取消费日志失败：%v", err)
	}
	if !confirmed {
		if len(logs) != 0 {
			t.Fatalf("未确认结算不应写消费日志：%+v", logs)
		}
		return
	}
	if len(logs) != 1 {
		t.Fatalf("确认结算应写一条消费日志：%+v", logs)
	}
	log := logs[0]
	if log.Quota != int(charge) || log.PromptTokens != 30 || log.CompletionTokens != 0 || log.ModelName != modelName || log.IsStream != isStream {
		t.Fatalf("消费日志字段错误：%+v", log)
	}
	var extraBilling map[string]ExtraBillingData
	if raw, marshalErr := json.Marshal(log.Metadata.Data()["extra_billing"]); marshalErr == nil && string(raw) != "null" {
		if err := json.Unmarshal(raw, &extraBilling); err != nil {
			t.Fatalf("解析搜索日志明细失败：%v raw=%s", err, raw)
		}
	}
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearch, "")
	if searches == 0 {
		if _, exists := extraBilling[key]; exists {
			t.Fatalf("零搜索不应写搜索日志明细：%+v", extraBilling)
		}
	} else if extraBilling[key].CallCount != searches {
		t.Fatalf("搜索日志次数错误：got=%+v want=%d", extraBilling, searches)
	}
}
