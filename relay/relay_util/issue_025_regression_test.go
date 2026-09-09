package relay_util

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"one-api/common/utils"
	"strings"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/claude"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

func TestI025ClaudeProviderTierReachesAttemptAndSQL(t *testing.T) {
	const modelName = "claude-i025"
	for _, test := range []struct {
		name        string
		stream      bool
		actualTier  string
		requestTier string
		charge      int64
	}{
		{name: "native_unary_priority", actualTier: "priority", charge: 240},
		{name: "native_stream_priority", stream: true, actualTier: "priority", charge: 240},
		{name: "native_unary_request_priority_actual_default", actualTier: "default", requestTier: "priority", charge: 120},
		{name: "native_stream_request_priority_actual_default", stream: true, actualTier: "default", requestTier: "priority", charge: 120},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			priorityRules := datatypes.NewJSONType(model.PriceRateRules{Version: 2, ServiceTier: []model.PriceRateRule{{ID: "priority", When: model.PriceRuleCondition{ServiceTier: []string{"fast", "priority"}}, Multipliers: model.PriceRateMultiplier{Input: utils.GetPointer(float64(2)), Output: utils.GetPointer(float64(2))}}}})
			model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
				modelName: {Model: modelName, Type: model.TokensPriceType, Input: 1, Output: 1, RateRules: &priorityRules},
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

			requestBody := fmt.Sprintf(`{"model":%q,"max_tokens":64,"messages":[],"stream":%t`, modelName, test.stream)
			if test.requestTier != "" {
				requestBody += fmt.Sprintf(`,"service_tier":%q`, test.requestTier)
			}
			requestBody += "}"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, issue025ClaudeStreamWire(modelName, test.actualTier))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, issue025ClaudeUnaryWire(modelName, test.actualTier))
			}))
			t.Cleanup(server.Close)
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/claude/v1/messages", strings.NewReader(requestBody))
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Set("id", 1)
			ctx.Set("token_id", 1)
			setQuotaTestRoutingGroup(t, ctx, 1)
			if _, err := common.CacheRequestBody(ctx); err != nil {
				t.Fatalf("缓存 Claude request body 失败：%v", err)
			}

			protocol := LogProtocolHTTP
			if test.stream {
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

			proxy := ""
			provider := claude.CreateClaudeProvider(&model.Channel{Key: "i025-key", Proxy: &proxy}, server.URL)
			provider.SetContext(ctx)
			provider.SetOriginalModel(modelName)
			provider.SetUsage(&types.Usage{})
			if test.stream {
				stream, apiErr := provider.CreateClaudeChatStream(&claude.ClaudeRequest{Model: modelName, Stream: true})
				if apiErr != nil {
					t.Fatalf("Claude stream 失败：%+v", apiErr)
				}
				drainUsageFixtureStream(t, stream)
			} else {
				response, apiErr := provider.CreateClaudeChat(&claude.ClaudeRequest{Model: modelName})
				if apiErr != nil || response == nil {
					t.Fatalf("Claude unary 失败：response=%+v err=%+v", response, apiErr)
				}
			}

			usage := provider.GetUsage()
			if usage.ServiceTier != test.actualTier || usage.PromptTokens != 100 || usage.CompletionTokens != 20 || usage.TotalTokens != 120 {
				t.Fatalf("Claude provider usage 投影错误：want tier=%q tokens=100+20, got %+v", test.actualTier, usage)
			}
			result, err := attempt.CloseFromProviderResult(context.Background(), usage, test.stream)
			if err != nil || result.Unsettled || !result.Confirmed || result.ChargedQuota != test.charge {
				t.Fatalf("Claude usage → Attempt → SQL 结算错误：want charge=%d result=%+v err=%v", test.charge, result, err)
			}
			second, err := attempt.CloseFromProviderResult(context.Background(), usage, test.stream)
			if err != nil || second != result {
				t.Fatalf("重复 close 改变了结算结果：first=%+v second=%+v err=%v", result, second, err)
			}

			var user model.User
			if err := model.DB.First(&user, 1).Error; err != nil {
				t.Fatalf("读取 user 失败：%v", err)
			}
			var token model.Token
			if err := model.DB.First(&token, 1).Error; err != nil {
				t.Fatalf("读取 token 失败：%v", err)
			}
			if int64(user.Quota) != 100000-test.charge || int64(token.RemainQuota) != 100000-test.charge || int64(token.UsedQuota) != test.charge {
				t.Fatalf("SQL 余额错误：user=%d token_remain=%d token_used=%d charge=%d", user.Quota, token.RemainQuota, token.UsedQuota, test.charge)
			}

			var logs []model.Log
			if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
				t.Fatalf("读取消费日志失败：%v", err)
			}
			if len(logs) != 1 {
				t.Fatalf("一次确认结算应只有一条消费日志：%+v", logs)
			}
			log := logs[0]
			if log.Quota != int(test.charge) || log.PromptTokens != 100 || log.CompletionTokens != 20 || log.ModelName != modelName || log.IsStream != test.stream {
				t.Fatalf("消费日志字段错误：%+v", log)
			}
			metadata := log.Metadata.Data()
			if metadata["effective_service_tier"] != test.actualTier {
				t.Fatalf("消费日志未记录上游实际 tier：metadata=%+v", metadata)
			}
			var tokenBilling struct {
				Charge int64 `json:"charge"`
			}
			rawBilling, err := json.Marshal(metadata["token_billing"])
			if err != nil || json.Unmarshal(rawBilling, &tokenBilling) != nil || tokenBilling.Charge != test.charge {
				t.Fatalf("消费日志 token_billing charge 错误：want=%d err=%v raw=%s", test.charge, err, rawBilling)
			}
		})
	}
}

func issue025ClaudeUnaryWire(modelName, serviceTier string) string {
	return fmt.Sprintf(`{"id":"msg_i025","type":"message","role":"assistant","model":%q,"content":[],"usage":{"input_tokens":100,"output_tokens":20,"service_tier":%q}}`, modelName, serviceTier)
}

func issue025ClaudeStreamWire(modelName, serviceTier string) string {
	return fmt.Sprintf("event: message_start\ndata: %s\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", fmt.Sprintf(`{"type":"message_start","message":{"model":%q,"usage":{"input_tokens":100,"output_tokens":20,"service_tier":%q}}}`, modelName, serviceTier))
}
