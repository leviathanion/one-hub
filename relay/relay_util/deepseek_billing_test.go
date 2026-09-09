package relay_util

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gorm.io/datatypes"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/deepseek"
	"one-api/types"
)

func TestDeepSeekCanonicalCacheSettlesAndLogs(t *testing.T) {
	const modelName = "deepseek-v4.1-flash-expires-on-0910"
	for _, streaming := range []bool{false, true} {
		for _, tc := range []struct {
			name, fields   string
			cached, charge int
		}{
			{"双字段高命中", `"prompt_tokens_details":{"cached_tokens":896},"prompt_cache_hit_tokens":896,"prompt_cache_miss_tokens":238`, 896, 68},
			{"仅标准字段", `"prompt_tokens_details":{"cached_tokens":896}`, 896, 68},
			{"原生命中补入", `"prompt_cache_hit_tokens":896,"prompt_cache_miss_tokens":238`, 896, 68},
			{"不一致采用标准", `"prompt_tokens_details":{"cached_tokens":896},"prompt_cache_hit_tokens":1000,"prompt_cache_miss_tokens":134`, 896, 68},
			{"标准零值优先", `"prompt_tokens_details":{"cached_tokens":0},"prompt_cache_hit_tokens":896,"prompt_cache_miss_tokens":238`, 0, 254},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, streaming), func(t *testing.T) {
				previousLogger := logger.Logger
				t.Cleanup(func() { logger.Logger = previousLogger })
				useQuotaReserveTestDB(t)
				pool, err := model.DB.DB()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = pool.Close() })
				insertQuotaReserveFixtures(t, 10000000)
				if err := model.DB.AutoMigrate(&model.Log{}, &model.Statistics{}); err != nil {
					t.Fatal(err)
				}
				oldPricing, oldOptions := model.PricingInstance, config.GlobalOption
				oldBatch, oldLog, oldRedis := config.BatchUpdateEnabled, config.LogConsumeEnabled, config.RedisEnabled
				t.Cleanup(func() {
					model.PricingInstance, config.GlobalOption = oldPricing, oldOptions
					config.BatchUpdateEnabled, config.LogConsumeEnabled, config.RedisEnabled = oldBatch, oldLog, oldRedis
				})
				config.BatchUpdateEnabled, config.LogConsumeEnabled, config.RedisEnabled = false, true, false
				config.GlobalOption = config.NewOptionManager()
				extra := datatypes.NewJSONType(map[string]float64{config.UsageExtraCache: 0.033})
				model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
					modelName: {Model: modelName, Type: model.TokensPriceType, Input: 0.2143, Output: 0.6429, ExtraRatios: &extra},
				}}
				usageJSON := `{"prompt_tokens":1134,"completion_tokens":16,"total_tokens":1150,` + tc.fields + `}`
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: {\"id\":\"repro\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":%s}\n\ndata: [DONE]\n\n", modelName, usageJSON)
					} else {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprintf(w, `{"id":"repro","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":%s,"future_response":{"retained":true}}`, modelName, usageJSON)
					}
				}))
				defer server.Close()
				oldClient := requester.HTTPClient
				requester.HTTPClient = server.Client()
				t.Cleanup(func() { requester.HTTPClient = oldClient })
				proxy, baseURL := "", server.URL
				provider := (deepseek.DeepseekProviderFactory{}).Create(&model.Channel{Type: config.ChannelTypeDeepseek, Key: "fixture", Proxy: &proxy, BaseURL: &baseURL}).(*deepseek.DeepseekProvider)
				usage := &types.Usage{}
				provider.SetUsage(usage)
				attempt := &AttemptQuota{quota: &Quota{modelName: modelName, groupName: useComponentTestGroup(t), userId: 1, tokenId: 1}}
				if err := attempt.ApplyReserve(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := attempt.ClaimSubmission(); err != nil {
					t.Fatal(err)
				}
				request := &types.ChatCompletionRequest{Model: modelName, Stream: streaming, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "OK"}}}
				if streaming {
					reader, apiErr := provider.CreateChatCompletionStream(request)
					if apiErr != nil {
						t.Fatal(apiErr)
					}
					defer requester.CloseAndDrainStream(reader)
					data, errs := reader.Recv()
					timer := time.NewTimer(5 * time.Second)
					defer timer.Stop()
					for data != nil || errs != nil {
						select {
						case _, ok := <-data:
							if !ok {
								data = nil
							}
						case err, ok := <-errs:
							if !ok {
								errs = nil
							} else if err != nil && !errors.Is(err, io.EOF) {
								t.Fatal(err)
							}
						case <-timer.C:
							t.Fatal("流式响应未结束")
						}
					}
				} else {
					response, apiErr := provider.CreateChatCompletion(request)
					if apiErr != nil {
						t.Fatal(apiErr)
					}
					if response.Usage.PromptTokensDetails.CachedTokens != tc.cached {
						t.Fatalf("响应缓存归一化错误：%+v", response.Usage)
					}
				}
				result, err := attempt.CloseFromProviderResult(context.Background(), usage, streaming)
				if err != nil || !result.Confirmed || result.ChargedQuota != int64(tc.charge) {
					t.Fatalf("结算错误：%+v，%v", result, err)
				}
				var logs []model.Log
				if err := model.DB.Find(&logs).Error; err != nil {
					t.Fatal(err)
				}
				if len(logs) != 1 || logs[0].Quota != tc.charge || logs[0].CacheTokens != tc.cached {
					t.Fatalf("日志与标准缓存计价不一致：%+v", logs)
				}
				var user model.User
				if err := model.DB.First(&user, 1).Error; err != nil || user.Quota != 10000000-tc.charge {
					t.Fatalf("余额与结算不一致：%d，%v", user.Quota, err)
				}
				if calls != 1 {
					t.Fatalf("上游被调用 %d 次", calls)
				}
				for _, key := range []string{"deepseek_cache_hit_tokens", "deepseek_cache_miss_tokens"} {
					if _, ok := usage.GetExtraTokens()[key]; ok {
						t.Fatalf("旧计价维度仍存在：%s", key)
					}
				}
			})
		}
	}
}
