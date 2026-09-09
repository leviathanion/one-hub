package relay_util

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/azuredatabricks"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestI021DatabricksUsageReachesAttemptSettlement(t *testing.T) {
	const (
		modelName  = "databricks-actual"
		unaryBody  = `{"id":"db-unary","object":"chat.completion","model":"databricks-actual","service_tier":"priority","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`
		streamWire = "data: {\"id\":\"db-stream\",\"object\":\"chat.completion.chunk\",\"model\":\"databricks-actual\",\"service_tier\":\"priority\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"db-stream\",\"object\":\"chat.completion.chunk\",\"model\":\"databricks-actual\",\"service_tier\":\"priority\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120}}\n\ndata: [DONE]\n\n"
	)

	for _, test := range []struct {
		name    string
		stream  bool
		wire    string
		confirm bool
		charge  int64
	}{
		{name: "stream真实usage", stream: true, wire: streamWire, confirm: true, charge: 120},
		{name: "unary真实usage", wire: unaryBody, confirm: true, charge: 120},
		{name: "stream缺usage", stream: true, wire: "data: {\"id\":\"db-missing\",\"model\":\"databricks-actual\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"long local text\"}}]}\n\ndata: [DONE]\n\n", confirm: false},
		{name: "stream无效usage", stream: true, wire: "data: {\"id\":\"db-invalid\",\"model\":\"databricks-actual\",\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":999}}\n\ndata: [DONE]\n\n", confirm: false},
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
			provider := (azuredatabricks.AzureDatabricksProviderFactory{}).Create(&model.Channel{Key: "test-token", Proxy: &proxy, BaseURL: &baseURL}).(*azuredatabricks.AzureDatabricksProvider)
			provider.SetUsage(&types.Usage{})
			request := &types.ChatCompletionRequest{Model: modelName, Stream: test.stream, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}}}
			if test.stream {
				stream, apiErr := provider.CreateChatCompletionStream(request)
				if apiErr != nil {
					t.Fatalf("Databricks stream 失败：%+v", apiErr)
				}
				output := drainUsageFixtureStream(t, stream)
				if test.confirm && !strings.Contains(output, "\"total_tokens\"") {
					t.Fatalf("stream usage 未透传：%s", output)
				}
			} else {
				response, apiErr := provider.CreateChatCompletion(request)
				if apiErr != nil || response == nil {
					t.Fatalf("Databricks unary 失败：response=%+v err=%+v", response, apiErr)
				}
			}

			if test.confirm && !provider.GetUsage().HasProviderUsage() {
				t.Fatalf("真实 usage 未形成 provider evidence：%+v", provider.GetUsage())
			}
			if !test.confirm && provider.GetUsage().HasProviderUsage() {
				t.Fatalf("缺失或无效 usage 被本地估算授权：%+v", provider.GetUsage())
			}
			settleI021ProviderFixtureUsage(t, provider.GetUsage(), modelName, test.stream, test.confirm, test.charge)
		})
	}
}

func settleI021ProviderFixtureUsage(t *testing.T, usage *types.Usage, modelName string, isStream, confirmed bool, charge int64) {
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
	if confirmed {
		if len(logs) != 1 {
			t.Fatalf("真实 usage 应写入一条消费日志：%+v", logs)
		}
		log := logs[0]
		if log.Quota != int(charge) || log.PromptTokens != 100 || log.CompletionTokens != 20 || log.ModelName != modelName || log.IsStream != isStream {
			t.Fatalf("消费日志字段错误：%+v", log)
		}
	} else if len(logs) != 0 {
		t.Fatalf("缺失或无效 usage 不应写消费日志：%+v", logs)
	}
}
