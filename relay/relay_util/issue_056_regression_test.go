package relay_util

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/gemini"
	"one-api/providers/vertexai"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

const (
	i056BillingRequestModel = "gemini-2.5-pro"
	i056BillingActualModel  = "gemini-i056-actual"
	i056VertexProjectID     = "i056-gemini-usage-project"
)

type i056ProviderEntry struct {
	name   string
	vertex bool
	native bool
	stream bool
}

var i056ProviderEntries = []i056ProviderEntry{
	{name: "Gemini_Chat_unary"},
	{name: "Gemini_Chat_stream", stream: true},
	{name: "Gemini_native_unary", native: true},
	{name: "Gemini_native_stream", native: true, stream: true},
	{name: "Vertex_Chat_unary", vertex: true},
	{name: "Vertex_Chat_stream", vertex: true, stream: true},
	{name: "Vertex_native_unary", vertex: true, native: true},
	{name: "Vertex_native_stream", vertex: true, native: true, stream: true},
}

// 八个真实 factory 入口共用同一 provider response，断言直接落在
// provider → Attempt → SQL 边界，不用手工构造 Usage 冒充某一个入口。
func TestI056GeminiVertexProviderEntriesSettleThroughAttemptSQLAndLog(t *testing.T) {
	for _, entry := range i056ProviderEntries {
		for _, variant := range []struct {
			name                 string
			candidatesTokenCount string
		}{
			{name: "candidates_omitted", candidatesTokenCount: ""},
			{name: "candidates_explicit_zero", candidatesTokenCount: `,"candidatesTokenCount":0`},
		} {
			t.Run(entry.name+"/"+variant.name, func(t *testing.T) {
				runI056ProviderSettlement(t, entry, variant.candidatesTokenCount)
			})
		}
	}
}

func runI056ProviderSettlement(t *testing.T, entry i056ProviderEntry, candidatesTokenCount string) {
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
		i056BillingRequestModel: {Model: i056BillingRequestModel, Type: model.TokensPriceType, Input: 1, Output: 1},
		i056BillingActualModel:  {Model: i056BillingActualModel, Type: model.TokensPriceType, Input: 1, Output: 1},
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

	usageWire := i056UsageWire(candidatesTokenCount)
	responseWire := i056UnaryWire(usageWire)
	streamWire := i056StreamWire(usageWire)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if entry.stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, streamWire)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(responseWire)
	}))
	t.Cleanup(server.Close)

	requestBody := []byte(`{"contents":[]}`)
	ctx := newI056BillingContext(t, requestBody)
	protocol := LogProtocolHTTP
	if entry.stream {
		protocol = LogProtocolHTTPStream
	}
	attempt, err := NewAttemptQuota(ctx, i056BillingRequestModel, 10, BillingAttemptSpec{LogProtocol: protocol})
	if err != nil {
		t.Fatalf("创建 billing attempt 失败：%v", err)
	}
	if err := attempt.ApplyReserve(ctx.Request.Context()); err != nil {
		t.Fatalf("预扣失败：%v", err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatalf("记录提交失败：%v", err)
	}

	previousClient := requester.HTTPClient
	actualWire := []byte(nil)
	recordingClient := *server.Client()
	recordingClient.Transport = &i056ResponseBodyRecorder{
		base:    recordingClient.Transport,
		capture: &actualWire,
	}
	requester.HTTPClient = &recordingClient
	t.Cleanup(func() { requester.HTTPClient = previousClient })
	usage, rawResponse := invokeI056Provider(t, entry, server, ctx)
	if calls != 1 {
		t.Fatalf("provider calls=%d, want 1", calls)
	}
	wantWire := responseWire
	if entry.stream {
		wantWire = []byte(streamWire)
	}
	if !bytes.Equal(actualWire, wantWire) {
		t.Fatalf("provider HTTP response wire changed before adapter: got %q, want %q", actualWire, wantWire)
	}
	assertI056SettlementUsage(t, usage)
	if entry.native && entry.stream && rawResponse != streamWire {
		t.Fatalf("native stream wire changed: got %q, want %q", rawResponse, streamWire)
	}
	if entry.native && !entry.stream && !entry.vertex && rawResponse != string(responseWire) {
		t.Fatalf("native unary wire changed: got %q, want %q", rawResponse, responseWire)
	}

	result, err := attempt.CloseFromProviderResult(ctx.Request.Context(), usage, entry.stream)
	if err != nil || result.Unsettled || !result.Confirmed || result.ChargedQuota != 109 {
		t.Fatalf("provider→attempt→SQL settlement mismatch: result=%+v err=%v usage=%+v", result, err, usage)
	}
	assertI056SettlementRows(t, i056BillingActualModel, entry.stream)
	if !entry.stream {
		assertI056RawUsage(t, actualWire)
	}
}

type i056ResponseBodyRecorder struct {
	base    http.RoundTripper
	capture *[]byte
}

func (r *i056ResponseBodyRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	base := r.base
	if base == nil {
		base = http.DefaultTransport
	}
	response, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return nil, err
	}
	if r.capture != nil {
		*r.capture = append((*r.capture)[:0], body...)
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}

func i056UsageWire(candidatesTokenCount string) string {
	return `{"promptTokenCount":10` + candidatesTokenCount + `,"thoughtsTokenCount":99,"totalTokenCount":109}`
}

func i056UnaryWire(usageWire string) []byte {
	return []byte(`{"modelVersion":"` + i056BillingActualModel + `","candidates":[{"index":0,"content":{"role":"model","parts":[]},"finishReason":"MAX_TOKENS"}],"usageMetadata":` + usageWire + `,"future":{"kept":true}}`)
}

func i056StreamWire(usageWire string) string {
	first := `{"modelVersion":"` + i056BillingActualModel + `","candidates":[{"index":0,"content":{"role":"model","parts":[]},"finishReason":"MAX_TOKENS"}]}`
	second := `{"modelVersion":"` + i056BillingActualModel + `","usageMetadata":` + usageWire + `}`
	return "data: " + first + "\n\n" + "data: " + second + "\n\n"
}

func newI056BillingContext(t *testing.T, requestBody []byte) *gin.Context {
	t.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(requestBody))
	if _, err := common.CacheRequestBody(ctx); err != nil {
		t.Fatalf("缓存 provider 请求失败：%v", err)
	}
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	setQuotaTestRoutingGroup(t, ctx, 1)
	return ctx
}

func invokeI056Provider(t *testing.T, entry i056ProviderEntry, server *httptest.Server, ctx *gin.Context) (*types.Usage, string) {
	t.Helper()
	if entry.vertex {
		cache.InitCacheManager()
		cacheKey := vertexai.TokenCacheKey + ":" + i056VertexProjectID
		if err := cache.SetCache(cacheKey, "i056-test-token", time.Minute); err != nil {
			t.Fatalf("seed Vertex token cache: %v", err)
		}
		t.Cleanup(func() { _ = cache.DeleteCache(cacheKey) })
	}

	proxy, baseURL := "", server.URL
	if entry.vertex {
		provider := (vertexai.VertexAIProviderFactory{}).Create(&model.Channel{
			Type:  config.ChannelTypeVertexAI,
			Key:   `{}`,
			Proxy: &proxy,
			Other: `{"region":"global","project_id":"` + i056VertexProjectID + `"}`,
		}).(*vertexai.VertexAIProvider)
		provider.Config.BaseURL = baseURL + "/%s/%s/%s/%s:%s"
		provider.SetContext(ctx)
		provider.SetUsage(&types.Usage{})
		return invokeI056VertexProvider(t, provider, entry)
	}

	provider := (gemini.GeminiProviderFactory{}).Create(&model.Channel{
		Type:    config.ChannelTypeGemini,
		Key:     "i056-billing-key",
		Proxy:   &proxy,
		BaseURL: &baseURL,
	}).(*gemini.GeminiProvider)
	provider.SetContext(ctx)
	provider.SetUsage(&types.Usage{})
	if entry.native {
		request := &gemini.GeminiChatRequest{Model: i056BillingRequestModel, Stream: entry.stream}
		if entry.stream {
			stream, apiErr := provider.CreateGeminiChatStream(request)
			if apiErr != nil || stream == nil {
				t.Fatalf("Gemini native stream failed to open: stream=%v err=%+v", stream, apiErr)
			}
			return provider.GetUsage(), drainUsageFixtureStream(t, stream)
		}
		response, apiErr := provider.CreateGeminiChat(request)
		if apiErr != nil || response == nil {
			t.Fatalf("Gemini native unary failed: response=%+v err=%+v", response, apiErr)
		}
		return provider.GetUsage(), string(response.ReplayProviderRawJSON())
	}

	request := &types.ChatCompletionRequest{
		Model:  i056BillingRequestModel,
		Stream: entry.stream,
		Messages: []types.ChatCompletionMessage{{
			Role:    types.ChatMessageRoleUser,
			Content: "hello",
		}},
	}
	if entry.stream {
		stream, apiErr := provider.CreateChatCompletionStream(request)
		if apiErr != nil || stream == nil {
			t.Fatalf("Gemini Chat stream failed to open: stream=%v err=%+v", stream, apiErr)
		}
		return provider.GetUsage(), drainUsageFixtureStream(t, stream)
	}
	response, apiErr := provider.CreateChatCompletion(request)
	if apiErr != nil || response == nil {
		t.Fatalf("Gemini Chat unary failed: response=%+v err=%+v", response, apiErr)
	}
	return provider.GetUsage(), ""
}

func invokeI056VertexProvider(t *testing.T, provider *vertexai.VertexAIProvider, entry i056ProviderEntry) (*types.Usage, string) {
	t.Helper()
	if entry.native {
		request := &gemini.GeminiChatRequest{Model: i056BillingRequestModel, Stream: entry.stream}
		if entry.stream {
			stream, apiErr := provider.CreateGeminiChatStream(request)
			if apiErr != nil || stream == nil {
				t.Fatalf("Vertex native stream failed to open: stream=%v err=%+v", stream, apiErr)
			}
			return provider.GetUsage(), drainUsageFixtureStream(t, stream)
		}
		response, apiErr := provider.CreateGeminiChat(request)
		if apiErr != nil || response == nil {
			t.Fatalf("Vertex native unary failed: response=%+v err=%+v", response, apiErr)
		}
		// Vertex native unary 当前只暴露 typed response；stream 路径在上面额外
		// 校验原生 wire 原样交付。
		return provider.GetUsage(), ""
	}

	request := &types.ChatCompletionRequest{
		Model:  i056BillingRequestModel,
		Stream: entry.stream,
		Messages: []types.ChatCompletionMessage{{
			Role:    types.ChatMessageRoleUser,
			Content: "hello",
		}},
	}
	if entry.stream {
		stream, apiErr := provider.CreateChatCompletionStream(request)
		if apiErr != nil || stream == nil {
			t.Fatalf("Vertex Chat stream failed to open: stream=%v err=%+v", stream, apiErr)
		}
		return provider.GetUsage(), drainUsageFixtureStream(t, stream)
	}
	response, apiErr := provider.CreateChatCompletion(request)
	if apiErr != nil || response == nil {
		t.Fatalf("Vertex Chat unary failed: response=%+v err=%+v", response, apiErr)
	}
	return provider.GetUsage(), ""
}

func assertI056SettlementUsage(t *testing.T, usage *types.Usage) {
	t.Helper()
	if usage == nil || !usage.HasProviderUsage() {
		t.Fatalf("provider usage evidence missing: %+v", usage)
	}
	if usage.ResponseModel != i056BillingActualModel || usage.PromptTokens != 10 || usage.CompletionTokens != 99 || usage.TotalTokens != 109 {
		t.Fatalf("provider usage/model mismatch: %+v", usage)
	}
	if amount := int64(usage.PromptTokens) + int64(usage.CompletionTokens); amount != 109 {
		t.Fatalf("fixed input/output unit price 1 produced amount=%d, want 109", amount)
	}
}

func assertI056SettlementRows(t *testing.T, actualModel string, isStream bool) {
	t.Helper()
	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("读取 user 失败：%v", err)
	}
	var token model.Token
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("读取 token 失败：%v", err)
	}
	if user.Quota != 100000-109 || token.RemainQuota != 100000-109 || user.UsedQuota != 109 || token.UsedQuota != 109 || user.RequestCount != 1 {
		t.Fatalf("SQL balances mismatch: user=%+v token=%+v", user, token)
	}

	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatalf("读取消费日志失败：%v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("应写一条消费日志，得到 %+v", logs)
	}
	log := logs[0]
	if log.Quota != 109 || log.PromptTokens != 10 || log.CompletionTokens != 99 || log.ModelName != i056BillingRequestModel || log.IsStream != isStream {
		t.Fatalf("消费日志 token/金额/model 错误：%+v", log)
	}
	metadata := log.Metadata.Data()
	if metadata["billing_model"] != actualModel {
		t.Fatalf("消费日志未保留实际 model：%#v", metadata)
	}
	details := loggedTokenBilling(t, metadata)
	if details.Status != PriceComponentPriceable || details.Charge != 109 || details.InputUnits != 10 || details.OutputUnits != 99 {
		t.Fatalf("消费日志 token billing facts mismatch: %+v", details)
	}
}

func assertI056RawUsage(t *testing.T, responseWire []byte) {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(responseWire, &decoded); err != nil {
		t.Fatalf("解析原始 provider response 失败：%v", err)
	}
	rawUsage, ok := decoded["usageMetadata"].(map[string]any)
	if !ok || rawUsage["promptTokenCount"] != float64(10) || rawUsage["thoughtsTokenCount"] != float64(99) || rawUsage["totalTokenCount"] != float64(109) {
		t.Fatalf("原始 usage 与结算用量不一致：%#v", decoded["usageMetadata"])
	}
}

// 矛盾的 cache/detail 声明不能形成可计费 evidence，并必须走真实 Attempt
// 退款路径。fixture 本身是成功的 native response，退款原因只能来自 usage 合同。
func TestI056GeminiInvalidUsageRefundsAttemptSQL(t *testing.T) {
	for _, test := range []struct {
		name        string
		usageFields string
	}{
		{name: "cache_exceeds_prompt", usageFields: `,"cachedContentTokenCount":11`},
		{name: "image_detail_exceeds_prompt", usageFields: `,"promptTokensDetails":[{"modality":"IMAGE","tokenCount":11}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			runI056InvalidUsageRefund(t, test.usageFields)
		})
	}
}

func runI056InvalidUsageRefund(t *testing.T, usageFields string) {
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
		i056BillingRequestModel: {Model: i056BillingRequestModel, Type: model.TokensPriceType, Input: 1, Output: 1},
		i056BillingActualModel:  {Model: i056BillingActualModel, Type: model.TokensPriceType, Input: 1, Output: 1},
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

	responseWire := []byte(`{"modelVersion":"` + i056BillingActualModel + `","candidates":[{"index":0,"content":{"role":"model","parts":[]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":10,"thoughtsTokenCount":99,"totalTokenCount":109` + usageFields + `}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(responseWire)
	}))
	t.Cleanup(server.Close)

	ctx := newI056BillingContext(t, []byte(`{"contents":[]}`))
	attempt, err := NewAttemptQuota(ctx, i056BillingRequestModel, 10, BillingAttemptSpec{LogProtocol: LogProtocolHTTP})
	if err != nil {
		t.Fatalf("创建 billing attempt 失败：%v", err)
	}
	if err := attempt.ApplyReserve(ctx.Request.Context()); err != nil {
		t.Fatalf("预扣失败：%v", err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatalf("记录提交失败：%v", err)
	}

	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })
	proxy, baseURL := "", server.URL
	provider := (gemini.GeminiProviderFactory{}).Create(&model.Channel{
		Type:    config.ChannelTypeGemini,
		Key:     "i056-billing-key",
		Proxy:   &proxy,
		BaseURL: &baseURL,
	}).(*gemini.GeminiProvider)
	provider.SetContext(ctx)
	provider.SetUsage(&types.Usage{})
	response, apiErr := provider.CreateGeminiChat(&gemini.GeminiChatRequest{Model: i056BillingRequestModel})
	if apiErr != nil || response == nil {
		t.Fatalf("Gemini invalid-usage response failed: response=%+v err=%+v", response, apiErr)
	}
	if got := response.ReplayProviderRawJSON(); !bytes.Equal(got, responseWire) {
		t.Fatalf("invalid-usage native response wire changed: got %s, want %s", got, responseWire)
	}
	usage := provider.GetUsage()
	if usage.HasProviderUsage() || usage.ProviderReported {
		t.Fatalf("contradictory provider usage became chargeable: %+v", usage)
	}

	result, err := attempt.CloseFromProviderResult(ctx.Request.Context(), usage, false)
	if err != nil || result.Unsettled || result.Confirmed || result.ChargedQuota != 0 {
		t.Fatalf("invalid usage did not refund Attempt: result=%+v err=%v usage=%+v", result, err, usage)
	}
	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("读取退款 user 失败：%v", err)
	}
	var token model.Token
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("读取退款 token 失败：%v", err)
	}
	if user.Quota != 100000 || token.RemainQuota != 100000 || user.UsedQuota != 0 || token.UsedQuota != 0 || user.RequestCount != 0 {
		t.Fatalf("invalid usage refund mismatch: user=%+v token=%+v", user, token)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatalf("读取退款消费日志失败：%v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("invalid usage must not write consume log: %+v", logs)
	}
}
