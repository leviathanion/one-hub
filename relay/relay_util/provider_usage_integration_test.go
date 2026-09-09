package relay_util

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/claude"
	"one-api/providers/cloudflareAI"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func drainUsageFixtureStream(t *testing.T, stream requester.StreamReaderInterface[string]) string {
	t.Helper()
	defer requester.CloseAndDrainStream(stream)
	data, streamErrors := stream.Recv()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var output strings.Builder
	for data != nil || streamErrors != nil {
		select {
		case chunk, ok := <-data:
			if !ok {
				data = nil
				continue
			}
			output.WriteString(chunk)
		case err, ok := <-streamErrors:
			if !ok {
				streamErrors = nil
				continue
			}
			if !errors.Is(err, io.EOF) {
				t.Fatalf("读取供应商 stream：%v", err)
			}
		case <-timer.C:
			t.Fatal("等待 fixture stream 结束超时")
		}
	}
	return output.String()
}

func settleProviderFixtureUsage(t *testing.T, usage *types.Usage, modelName string, confirmed bool, charge int64) {
	t.Helper()
	useQuotaReserveTestDB(t)
	insertQuotaReserveFixtures(t, 100000)
	originalPricing := model.PricingInstance
	originalBatch, originalRedis, originalReserve := config.BatchUpdateEnabled, config.RedisEnabled, config.PreConsumedQuota
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		modelName: {Model: modelName, Type: model.TokensPriceType, Input: 1, Output: 1},
	}}
	config.BatchUpdateEnabled, config.RedisEnabled, config.PreConsumedQuota = false, false, 50
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled, config.RedisEnabled, config.PreConsumedQuota = originalBatch, originalRedis, originalReserve
	})
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	setQuotaTestRoutingGroup(t, ctx, 1)
	attempt, err := NewAttemptQuota(ctx, modelName, 10, BillingAttemptSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.ApplyReserve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !attempt.quota.HasPreConsumedSideEffect() {
		t.Fatal("测试必须先真实预扣")
	}
	result, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
	if err != nil || result.Unsettled || result.Confirmed != confirmed || result.ChargedQuota != charge {
		t.Fatalf("adapter → reducer → SQL 结算错误：result=%+v err=%v usage=%+v", result, err, usage)
	}
	second, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
	if err != nil || second != result {
		t.Fatalf("重复收尾改变结算：%+v %v", second, err)
	}
	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if int64(user.Quota) != 100000-charge || int64(token.RemainQuota) != 100000-charge || int64(token.UsedQuota) != charge {
		t.Fatalf("SQL 余额/预扣释放不一致：user=%d token=%d used=%d charge=%d", user.Quota, token.RemainQuota, token.UsedQuota, charge)
	}
}

func TestCloudflareWireUsageSettlesThroughAdapterAndSQL(t *testing.T) {
	fixture, err := os.ReadFile("../../providers/cloudflareAI/testdata/chat_usage.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		wire      string
		stream    bool
		confirmed bool
		charge    int64
	}{
		{name: "REST_权威计量", wire: string(fixture), confirmed: true, charge: 120},
		{name: "REST_长正文仅按权威量", wire: strings.Replace(string(fixture), `"ok"`, `"`+strings.Repeat("token ", 2000)+`"`, 1), confirmed: true, charge: 120},
		{name: "REST_零值", wire: `{"result":{"response":"很多正文不应估算扣费","usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}},"success":true}`, confirmed: true},
		{name: "REST_缺usage", wire: `{"result":{"response":"很多正文不应估算扣费"},"success":true}`},
		{name: "REST_缺completion", wire: `{"result":{"response":"ok","usage":{"prompt_tokens":100,"total_tokens":120}},"success":true}`},
		{name: "REST_null_prompt", wire: `{"result":{"response":"ok","usage":{"prompt_tokens":null,"completion_tokens":20}},"success":true}`},
		{name: "REST_总数保持原值", wire: `{"result":{"response":"ok","usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":999}},"success":true}`, confirmed: true, charge: 120},
		{name: "SSE_累计帧边界", stream: true, wire: "data: {\"response\":\"hello\",\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":3,\"total_tokens\":103}}\n\ndata: {\"response\":\" world\",\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120}}\n\ndata: [DONE]\n\n", confirmed: true, charge: 120},
		{name: "SSE_无证据", stream: true, wire: "data: {\"response\":\"hello world\"}\n\ndata: [DONE]\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			proxy, baseURL := "", server.URL+"/%s/%s"
			provider := (cloudflareAI.CloudflareAIProviderFactory{}).Create(&model.Channel{Key: "test-account|test-token", Proxy: &proxy, BaseURL: &baseURL}).(*cloudflareAI.CloudflareAIProvider)
			provider.SetUsage(&types.Usage{PromptTokens: 9999})
			request := &types.ChatCompletionRequest{Model: "@cf/test-fixture", Stream: test.stream, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "hello"}}}
			if test.stream {
				stream, apiErr := provider.CreateChatCompletionStream(request)
				if apiErr != nil {
					t.Fatalf("Cloudflare stream：%+v", apiErr)
				}
				drainUsageFixtureStream(t, stream)
			} else {
				response, apiErr := provider.CreateChatCompletion(request)
				if apiErr != nil || response == nil {
					t.Fatalf("Cloudflare REST：response=%+v err=%+v", response, apiErr)
				}
				if test.name == "REST_总数保持原值" && response.Usage.TotalTokens != 999 {
					t.Fatal("adapter 不应按本地规则改写供应商 total")
				}
			}
			settleProviderFixtureUsage(t, provider.GetUsage(), request.Model, test.confirmed, test.charge)
		})
	}
}

func TestClaudeWireUsageSettlesThroughBothAdaptersAndSQL(t *testing.T) {
	fixture, err := os.ReadFile("../../providers/claude/testdata/search_usage.sse")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		native  bool
		missing bool
	}{{native: true}, {native: false}, {native: true, missing: true}, {native: false, missing: true}} {
		native := test.native
		wire := fixture
		if test.missing {
			wire = bytes.ReplaceAll(wire, []byte(`"input_tokens":2679`), []byte(`"input_tokens":null`))
			wire = bytes.ReplaceAll(wire, []byte(`"input_tokens":10682`), []byte(`"input_tokens":null`))
			wire = bytes.ReplaceAll(wire, []byte(`,"server_tool_use":{"web_search_requests":1}`), nil)
		}
		name := "Chat转换"
		if native {
			name = "Messages原生"
		}
		if test.missing {
			name += "_缺完整Token证据"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(wire)
			}))
			defer server.Close()
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })
			proxy := ""
			provider := claude.CreateClaudeProvider(&model.Channel{Key: "test-token", Proxy: &proxy}, server.URL)
			provider.SetUsage(&types.Usage{})
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			provider.SetContext(ctx)
			var stream requester.StreamReaderInterface[string]
			var apiErr *types.OpenAIErrorWithStatusCode
			if native {
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = httptest.NewRequest(http.MethodPost, "/claude/v1/messages", strings.NewReader(`{"model":"claude-opus-5","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"weather"}]}`))
				if _, err := common.CacheRequestBody(ctx); err != nil {
					t.Fatal(err)
				}
				provider.SetContext(ctx)
				stream, apiErr = provider.CreateClaudeChatStream(&claude.ClaudeRequest{Model: "claude-opus-5", Stream: true})
			} else {
				stream, apiErr = provider.CreateChatCompletionStream(&types.ChatCompletionRequest{Model: "claude-opus-5", MaxCompletionTokens: 1024, Stream: true, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "weather"}}})
			}
			if apiErr != nil {
				t.Fatalf("Claude stream：%+v", apiErr)
			}
			output := drainUsageFixtureStream(t, stream)
			if native && !bytes.Equal([]byte(output), wire) {
				t.Fatalf("原生 stream wire 被改写：%s", output)
			}
			usage := provider.GetUsage()
			if test.missing {
				if usage.HasProviderUsage() {
					t.Fatalf("不完整 token 证据被本地估算补齐：%+v", usage)
				}
				settleProviderFixtureUsage(t, usage, "claude-opus-5", false, 0)
				return
			}
			if usage.PromptTokens != 10682 || usage.CompletionTokens != 510 || usage.ExtraBilling[types.APIToolTypeWebSearch].CallCount != 1 {
				t.Fatalf("累计 token 或搜索量错误：%+v", usage)
			}
			wantCharge := int64(10682+510) + int64(defaultExtraServicePrices.WebSearchGA*float64(config.QuotaPerUnit))
			settleProviderFixtureUsage(t, usage, "claude-opus-5", true, wantCharge)
		})
	}
}
