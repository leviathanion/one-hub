package relay_util

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"one-api/common/utils"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/claude"
	"one-api/providers/openai"
	xAI "one-api/providers/xAI"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

func usagePartitionXAIUsage(t *testing.T, raw string) *types.Usage {
	t.Helper()
	const modelName = "xai-usage-model"
	body := `{"model":"` + modelName + `","choices":[{"message":{"role":"assistant","content":"OK"}}],"future_response":9007199254740993,"usage":` + raw + `}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	defer func() { requester.HTTPClient = previousClient }()
	proxy, baseURL := "", server.URL
	provider := (xAI.XAIProviderFactory{}).Create(&model.Channel{
		Type: config.ChannelTypeXAI, Key: "test-key", Proxy: &proxy, BaseURL: &baseURL,
	}).(*xAI.XAIProvider)
	provider.SetUsage(&types.Usage{})
	response, apiErr := provider.CreateChatCompletion(&types.ChatCompletionRequest{
		Model: modelName, Messages: []types.ChatCompletionMessage{{Role: "user", Content: "Hi"}},
	})
	if apiErr != nil {
		t.Fatalf("xAI usage extraction: %+v", apiErr)
	}
	public := string(response.ReplayProviderRawJSON())
	for _, field := range []string{`"future_response":9007199254740993`, `"cost_in_usd_ticks":9007199254740993000`, `"future_usage":true`} {
		if !strings.Contains(public, field) {
			t.Fatalf("xAI normalization lost upstream field %s: %s", field, public)
		}
	}
	var original struct {
		Details map[string]json.RawMessage `json:"completion_tokens_details"`
	}
	if err := json.Unmarshal([]byte(raw), &original); err != nil {
		t.Fatal(err)
	}
	if text := original.Details["text_tokens"]; text != nil && !strings.Contains(public, `"text_tokens":`+string(text)) {
		t.Fatalf("xAI 改写了上游显式文本用量：%s", public)
	}
	return provider.GetUsage()
}

func TestXAIOutputPartitionsSettleFromProviderEvidence(t *testing.T) {
	const modelName = "xai-usage-model"
	for _, test := range []struct {
		name, fields string
		priceable    bool
		conflicting  bool
	}{
		{name: "explicit reasoning without total", fields: `"prompt_tokens":3,"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":2}`, priceable: true},
		{name: "total without reasoning detail", fields: `"prompt_tokens":3,"completion_tokens":5,"total_tokens":10`, priceable: true},
		{name: "complete output evidence", fields: `"prompt_tokens":3,"completion_tokens":5,"total_tokens":10,"completion_tokens_details":{"reasoning_tokens":2}`, priceable: true},
		{name: "matching explicit text partition", fields: `"prompt_tokens":3,"completion_tokens":5,"total_tokens":10,"completion_tokens_details":{"reasoning_tokens":2,"text_tokens":5}`, priceable: true},
		{name: "matching text partition without reasoning detail", fields: `"prompt_tokens":3,"completion_tokens":5,"total_tokens":10,"completion_tokens_details":{"text_tokens":5}`, priceable: true},
		{name: "conflicting explicit text partition", fields: `"prompt_tokens":3,"completion_tokens":5,"total_tokens":10,"completion_tokens_details":{"reasoning_tokens":2,"text_tokens":4}`, conflicting: true},
		{name: "conflicting explicit zero text partition", fields: `"prompt_tokens":3,"completion_tokens":5,"total_tokens":10,"completion_tokens_details":{"reasoning_tokens":2,"text_tokens":0}`, conflicting: true},
		{name: "conflicting text partition without reasoning detail", fields: `"prompt_tokens":3,"completion_tokens":5,"total_tokens":10,"completion_tokens_details":{"text_tokens":4}`, conflicting: true},
		{name: "conflicting zero text partition without reasoning detail", fields: `"prompt_tokens":3,"completion_tokens":5,"total_tokens":10,"completion_tokens_details":{"text_tokens":0}`, conflicting: true},
		{name: "visible output without total or reasoning", fields: `"prompt_tokens":3,"completion_tokens":5`},
		{name: "missing visible output", fields: `"prompt_tokens":3,"total_tokens":10,"completion_tokens_details":{"reasoning_tokens":2}`},
		{name: "negative output", fields: `"prompt_tokens":3,"completion_tokens":-1,"total_tokens":10,"completion_tokens_details":{"reasoning_tokens":2}`},
		{name: "conflicting total", fields: `"prompt_tokens":3,"completion_tokens":5,"total_tokens":20,"completion_tokens_details":{"reasoning_tokens":2}`, conflicting: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := `{` + test.fields + `,"cost_in_usd_ticks":9007199254740993000,"future_usage":true}`
			usage := usagePartitionXAIUsage(t, raw)
			if test.name == "total without reasoning detail" && usage.ProviderTokenFields[config.UsageExtraReasoning] {
				t.Fatal("derived reasoning was marked as observed provider evidence")
			}
			for _, price := range []struct {
				name   string
				ratios map[string]float64
				want   int64
			}{
				{name: "uniform price", want: 10},
				{name: "reasoning price", ratios: map[string]float64{config.UsageExtraReasoning: 2}, want: 12},
				{name: "distinct output partition prices", ratios: map[string]float64{config.UsageExtraOutputTextTokens: 2, config.UsageExtraReasoning: 3}, want: 19},
			} {
				t.Run(price.name, func(t *testing.T) {
					quota := usagePartitionQuota(t, usagePartitionPrice(t, modelName, 1, 1, price.ratios), modelName)
					decision := quota.EvaluateProviderUsage(usage)
					want, status := price.want, PriceComponentPriceable
					if !test.priceable {
						want, status = 0, PriceComponentMissingEvidence
						if test.conflicting {
							status = PriceComponentConflictingEvidence
						}
					}
					if decision.Confirm != test.priceable || decision.FinalQuota != want || componentStatuses(decision.Components)["tokens"] != status {
						t.Fatalf("xAI usage must settle with the customer price policy: usage=%+v decision=%+v want=%d status=%s", usage, decision, want, status)
					}
				})
			}
		})
	}
}

func usagePartitionPrice(t *testing.T, modelName string, input, output float64, ratios map[string]float64) *model.Pricing {
	t.Helper()
	price := &model.Price{Model: modelName, Type: model.TokensPriceType, Input: input, Output: output}
	if ratios != nil {
		extra := datatypes.NewJSONType(ratios)
		price.ExtraRatios = &extra
	}
	return &model.Pricing{Prices: map[string]*model.Price{modelName: price}}
}

func usagePartitionQuota(t *testing.T, pricing *model.Pricing, modelName string) *Quota {
	t.Helper()
	original := model.PricingInstance
	model.PricingInstance = pricing
	t.Cleanup(func() { model.PricingInstance = original })
	return &Quota{modelName: modelName, groupName: useComponentTestGroup(t), groupRatio: 1}
}

func usagePartitionTokenUsage(t *testing.T, input, output, total int, present ...string) *types.Usage {
	t.Helper()
	usage := &types.Usage{PromptTokens: input, CompletionTokens: output, TotalTokens: total}
	usage.MarkProviderReported()
	usage.RequireTokenExtraEvidence(config.UsageExtraInputTextTokens, config.UsageExtraInputAudio)
	usage.SetTokenExtraEvidenceGroups([]string{config.UsageExtraInputTextTokens, config.UsageExtraInputAudio})
	for _, key := range present {
		usage.SetExtraTokens(key, map[string]int{
			config.UsageExtraInputAudio:      input,
			config.UsageExtraInputTextTokens: 0,
		}[key])
		usage.ProviderTokenFields[key] = true
	}
	return usage
}

func usagePartitionClaudeUsage(t *testing.T, raw string) *types.Usage {
	t.Helper()
	var providerUsage claude.Usage
	if err := json.Unmarshal([]byte(raw), &providerUsage); err != nil {
		t.Fatalf("decode Claude usage: %v", err)
	}
	usage := &types.Usage{}
	if !claude.ClaudeUsageToOpenaiUsage(&providerUsage, usage) {
		t.Fatalf("Claude usage was not accepted by its producer: %+v", providerUsage)
	}
	usage.ResponseModel = "claude-model"
	return usage
}

func TestImagesOptionalOutputDetailsUseBaseTokensWhenUnpriced(t *testing.T) {
	var response types.ImageResponse
	if err := json.Unmarshal([]byte(`{
		"model":"image-model",
		"data":[{"b64_json":"image"}],
		"usage":{"input_tokens":50,"output_tokens":50,"total_tokens":100,
			"input_tokens_details":{"text_tokens":50,"image_tokens":0}}
	}`), &response); err != nil {
		t.Fatal(err)
	}
	usage := &types.Usage{}
	openai.ApplyImageEvidence(usage, &response)
	if !usage.HasProviderBaseUsage() || usage.HasProviderUsage() {
		t.Fatalf("optional output detail changed base/strict evidence contracts: %+v", usage)
	}
	if len(usage.RequiredTokenExtraKeys) != 4 || len(usage.TokenExtraEvidenceGroups) != 2 {
		t.Fatalf("image partition contract was lost: %+v", usage)
	}
	pricing := usagePartitionPrice(t, "image-model", 1, 1, nil)
	quota := usagePartitionQuota(t, pricing, "image-model")
	decision := quota.EvaluateProviderUsage(usage)
	if !decision.Confirm || decision.FinalQuota != 100 {
		t.Fatalf("image base usage should settle without optional output details: %+v", decision)
	}
	if status := componentStatuses(decision.Components)["tokens"]; status != PriceComponentPriceable {
		t.Fatalf("image token component status=%q, decision=%+v", status, decision)
	}
}

func TestTranscriptionOptionalInputDetailsUseBaseTokens(t *testing.T) {
	for _, test := range []struct {
		name    string
		present []string
	}{
		{name: "details omitted"},
		{name: "audio only", present: []string{config.UsageExtraInputAudio}},
		{name: "audio plus explicit zero text", present: []string{config.UsageExtraInputAudio, config.UsageExtraInputTextTokens}},
	} {
		t.Run(test.name, func(t *testing.T) {
			usage := usagePartitionTokenUsage(t, 14, 45, 59, test.present...)
			pricing := usagePartitionPrice(t, "transcription-model", 1, 1, nil)
			quota := usagePartitionQuota(t, pricing, "transcription-model")
			decision := quota.EvaluateProviderUsage(usage)
			if !decision.Confirm || decision.FinalQuota != 59 {
				t.Fatalf("transcription 14/45/59 should settle at 59: %+v", decision)
			}
		})
	}
}

func TestClaudeCachePartitionSentinelStillBlocksTokenSettlement(t *testing.T) {
	usage := usagePartitionClaudeUsage(t, `{
		"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":5,
		"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":0}
	}`)
	if usage.HasProviderUsage() {
		t.Fatalf("Claude producer sentinel unexpectedly passed strict evidence: %+v", usage)
	}
	quota := usagePartitionQuota(t, usagePartitionPrice(t, "claude-model", 1, 1, nil), "claude-model")
	decision := quota.EvaluateProviderUsage(usage)
	if decision.Confirm || decision.FinalQuota != 0 || componentStatuses(decision.Components)["tokens"] != PriceComponentMissingEvidence {
		t.Fatalf("missing Claude cache partition was charged: usage=%+v decision=%+v", usage, decision)
	}
}

func TestClaudeCachePartitionSentinelDoesNotUseDifferentPriceRatios(t *testing.T) {
	usage := usagePartitionClaudeUsage(t, `{
		"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":5,
		"cache_creation":{"ephemeral_5m_input_tokens":4}
	}`)
	pricing := usagePartitionPrice(t, "claude-model", 1, 1, map[string]float64{
		config.UsageExtraEphemeral5mInputTokens: 4,
		config.UsageExtraEphemeral1hInputTokens: 2,
	})
	quota := usagePartitionQuota(t, pricing, "claude-model")
	decision := quota.EvaluateProviderUsage(usage)
	if decision.Confirm || decision.FinalQuota != 0 || componentStatuses(decision.Components)["tokens"] != PriceComponentMissingEvidence {
		t.Fatalf("Claude cache sentinel was bypassed by price ratios: usage=%+v decision=%+v", usage, decision)
	}
}

func TestClaudeIndependentSearchSettlesWhenCacheTokenEvidenceIsMissing(t *testing.T) {
	usage := usagePartitionClaudeUsage(t, `{
		"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":5,
		"cache_creation":{"ephemeral_5m_input_tokens":4},
		"server_tool_use":{"web_search_requests":2}
	}`)
	quota := usagePartitionQuota(t, usagePartitionPrice(t, "claude-model", 1, 1, nil), "claude-model")
	decision := quota.EvaluateProviderUsage(usage)
	want := int64(saturatingCeilToInt(float64(2) * defaultExtraServicePrices.WebSearchGA * quota.effectiveQuotaPerUnit()))
	if !decision.Confirm || decision.FinalQuota != want {
		t.Fatalf("independent Claude search component was lost: usage=%+v decision=%+v want=%d", usage, decision, want)
	}
	status := componentStatuses(decision.Components)
	if status["tokens"] != PriceComponentMissingEvidence || status["unit:"+types.APIToolTypeWebSearch] != PriceComponentPriceable {
		t.Fatalf("Claude cache sentinel changed independent component statuses: %+v", decision.Components)
	}
}

func TestMissingPartitionDependsOnEffectivePrice(t *testing.T) {
	missingOutput := &types.Usage{}
	missingOutput.PromptTokens, missingOutput.CompletionTokens, missingOutput.TotalTokens = 50, 50, 100
	missingOutput.MarkProviderReported()
	missingOutput.RequireTokenExtraEvidence(config.UsageExtraInputTextTokens, config.UsageExtraInputImageTokens, config.UsageExtraOutputTextTokens, config.UsageExtraOutputImageTokens)
	missingOutput.SetTokenExtraEvidenceGroups(
		[]string{config.UsageExtraInputTextTokens, config.UsageExtraInputImageTokens},
		[]string{config.UsageExtraOutputTextTokens, config.UsageExtraOutputImageTokens},
	)
	missingOutput.SetExtraTokens(config.UsageExtraInputTextTokens, 50)
	missingOutput.ProviderTokenFields[config.UsageExtraInputTextTokens] = true

	t.Run("different output multipliers remain missing", func(t *testing.T) {
		quota := usagePartitionQuota(t, usagePartitionPrice(t, "image-model", 1, 1, map[string]float64{
			config.UsageExtraOutputTextTokens:  2,
			config.UsageExtraOutputImageTokens: 3,
		}), "image-model")
		decision := quota.EvaluateProviderUsage(missingOutput)
		if decision.Confirm || decision.FinalQuota != 0 || componentStatuses(decision.Components)["tokens"] != PriceComponentMissingEvidence {
			t.Fatalf("unknown differently priced output partition was guessed: %+v", decision)
		}
	})

	t.Run("equal non-one output multipliers are derivable", func(t *testing.T) {
		quota := usagePartitionQuota(t, usagePartitionPrice(t, "image-model", 1, 1, map[string]float64{
			config.UsageExtraOutputTextTokens:  2,
			config.UsageExtraOutputImageTokens: 2,
		}), "image-model")
		decision := quota.EvaluateProviderUsage(missingOutput)
		if !decision.Confirm || decision.FinalQuota != 150 {
			t.Fatalf("equal output partition multipliers were not derived: %+v", decision)
		}
	})

	t.Run("zero output base price makes missing output irrelevant", func(t *testing.T) {
		quota := usagePartitionQuota(t, usagePartitionPrice(t, "image-model", 1, 0, map[string]float64{
			config.UsageExtraOutputTextTokens:  2,
			config.UsageExtraOutputImageTokens: 3,
		}), "image-model")
		decision := quota.EvaluateProviderUsage(missingOutput)
		if !decision.Confirm || decision.FinalQuota != 50 {
			t.Fatalf("zero output price should leave input charge priceable: %+v", decision)
		}
	})
}

func TestActualModelAndTierPriceAffectPartitionDecision(t *testing.T) {
	requested := &model.Price{Model: "requested-model", Type: model.TokensPriceType, Input: 1, Output: 1}
	actual := &model.Price{Model: "actual-model", Type: model.TokensPriceType, Input: 2, Output: 3}
	rules := datatypes.NewJSONType(model.PriceRateRules{Version: 2, ServiceTier: []model.PriceRateRule{{ID: "flex", When: model.PriceRuleCondition{ServiceTier: []string{"flex"}}, Multipliers: model.PriceRateMultiplier{Input: utils.GetPointer(float64(2)), Output: utils.GetPointer(float64(3))}}}})
	actual.RateRules = &rules
	pricing := &model.Pricing{Prices: map[string]*model.Price{
		requested.Model: requested,
		actual.Model:    actual,
	}}
	usage := usagePartitionTokenUsage(t, 14, 45, 59)
	usage.ResponseModel = "actual-model"
	usage.ServiceTier = "flex"
	quota := usagePartitionQuota(t, pricing, "requested-model")
	decision := quota.EvaluateProviderUsage(usage)
	if !decision.Confirm || decision.FinalQuota != 461 {
		t.Fatalf("actual model/tier price was not applied (14*2*2 + 45*3*3): %+v", decision)
	}
}

func TestExplicitZeroNegativeConflictAndMissingBaseStayDistinct(t *testing.T) {
	pricing := usagePartitionPrice(t, "transcription-model", 1, 1, map[string]float64{
		config.UsageExtraInputTextTokens: 0,
		config.UsageExtraInputAudio:      1,
	})

	t.Run("explicit zero is a present partition", func(t *testing.T) {
		usage := usagePartitionTokenUsage(t, 14, 45, 59, config.UsageExtraInputAudio, config.UsageExtraInputTextTokens)
		quota := usagePartitionQuota(t, pricing, "transcription-model")
		decision := quota.EvaluateProviderUsage(usage)
		if !decision.Confirm || decision.FinalQuota != 59 {
			t.Fatalf("explicit zero partition was treated as missing: %+v", decision)
		}
	})

	t.Run("negative base", func(t *testing.T) {
		usage := usagePartitionTokenUsage(t, -1, 45, 44)
		quota := usagePartitionQuota(t, pricing, "transcription-model")
		decision := quota.EvaluateProviderUsage(usage)
		if decision.Confirm || decision.FinalQuota != 0 {
			t.Fatalf("negative provider usage was charged: %+v", decision)
		}
	})

	t.Run("conflicting total", func(t *testing.T) {
		usage := usagePartitionTokenUsage(t, 14, 45, 60)
		usage.ProviderTokenConflict = true
		quota := usagePartitionQuota(t, pricing, "transcription-model")
		decision := quota.EvaluateProviderUsage(usage)
		if decision.Confirm || componentStatuses(decision.Components)["tokens"] != PriceComponentConflictingEvidence {
			t.Fatalf("conflicting provider total was charged: %+v", decision)
		}
	})

	t.Run("missing base evidence", func(t *testing.T) {
		usage := usagePartitionTokenUsage(t, 14, 45, 59)
		usage.ProviderReported = false
		quota := usagePartitionQuota(t, pricing, "transcription-model")
		decision := quota.EvaluateProviderUsage(usage)
		if decision.Confirm || decision.FinalQuota != 0 {
			t.Fatalf("unreported base usage was charged: %+v", decision)
		}
	})
}

func TestSuccessfulChargeAndMissingDependentPartitionRefundSQL(t *testing.T) {
	useQuotaReserveTestDB(t)
	if err := model.DB.AutoMigrate(&model.Log{}); err != nil {
		t.Fatal(err)
	}
	insertQuotaReserveFixtures(t, 100000)

	originalPricing := model.PricingInstance
	originalPreConsumed := config.PreConsumedQuota
	originalBatch := config.BatchUpdateEnabled
	originalRedis := config.RedisEnabled
	originalLog := config.LogConsumeEnabled
	config.PreConsumedQuota = 50
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	config.LogConsumeEnabled = true
	pricing := usagePartitionPrice(t, "image-model", 1, 1, nil)
	model.PricingInstance = pricing
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.PreConsumedQuota = originalPreConsumed
		config.BatchUpdateEnabled = originalBatch
		config.RedisEnabled = originalRedis
		config.LogConsumeEnabled = originalLog
	})

	newContext := func() *gin.Context {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
		ctx.Set("id", 1)
		ctx.Set("token_id", 1)
		setQuotaTestRoutingGroup(t, ctx, 1)
		return ctx
	}
	imageUsage := func() *types.Usage {
		var response types.ImageResponse
		if err := json.Unmarshal([]byte(`{"model":"image-model","data":[{"url":"image"}],"usage":{"input_tokens":50,"output_tokens":50,"total_tokens":100,"input_tokens_details":{"text_tokens":50,"image_tokens":0}}}`), &response); err != nil {
			t.Fatal(err)
		}
		usage := &types.Usage{}
		openai.ApplyImageEvidence(usage, &response)
		return usage
	}

	first, err := NewAttemptQuota(newContext(), "image-model", 50, BillingAttemptSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.ApplyReserve(nil); err != nil {
		t.Fatal(err)
	}
	if err := first.ClaimSubmission(); err != nil {
		t.Fatal(err)
	}
	result, err := first.CloseFromProviderResult(nil, imageUsage(), false)
	if err != nil || !result.Confirmed || result.ChargedQuota != 100 {
		t.Fatalf("optional image detail success did not settle: result=%+v err=%v", result, err)
	}
	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 99900 || token.RemainQuota != 99900 || user.UsedQuota != 100 || token.UsedQuota != 100 {
		t.Fatalf("successful charge changed SQL balances incorrectly: user=%+v token=%+v", user, token)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Quota != 100 || logs[0].PromptTokens != 50 || logs[0].CompletionTokens != 50 {
		t.Fatalf("successful charge audit log mismatch: %+v", logs)
	}

	extra := datatypes.NewJSONType(map[string]float64{
		config.UsageExtraOutputTextTokens:  2,
		config.UsageExtraOutputImageTokens: 3,
	})
	pricing.Prices["image-model"].ExtraRatios = &extra
	second, err := NewAttemptQuota(newContext(), "image-model", 50, BillingAttemptSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.ApplyReserve(nil); err != nil {
		t.Fatal(err)
	}
	if err := second.ClaimSubmission(); err != nil {
		t.Fatal(err)
	}
	refund, err := second.CloseFromProviderResult(nil, imageUsage(), false)
	if err != nil || refund.Confirmed || refund.Unsettled {
		t.Fatalf("price-dependent missing partition did not refund cleanly: result=%+v err=%v", refund, err)
	}
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 99900 || token.RemainQuota != 99900 || user.UsedQuota != 100 || token.UsedQuota != 100 {
		t.Fatalf("refunded reservation changed settled SQL balances: user=%+v token=%+v", user, token)
	}
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("refunded missing token component wrote a consume log: %+v", logs)
	}
}

func TestClaudeCacheSentinelRefundsReservationSQL(t *testing.T) {
	useQuotaReserveTestDB(t)
	if err := model.DB.AutoMigrate(&model.Log{}); err != nil {
		t.Fatal(err)
	}
	insertQuotaReserveFixtures(t, 100000)

	originalPricing := model.PricingInstance
	originalPreConsumed := config.PreConsumedQuota
	originalBatch := config.BatchUpdateEnabled
	originalRedis := config.RedisEnabled
	originalLog := config.LogConsumeEnabled
	config.PreConsumedQuota = 50
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	config.LogConsumeEnabled = true
	model.PricingInstance = usagePartitionPrice(t, "claude-model", 1, 1, nil)
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.PreConsumedQuota = originalPreConsumed
		config.BatchUpdateEnabled = originalBatch
		config.RedisEnabled = originalRedis
		config.LogConsumeEnabled = originalLog
	})

	ctxFactory := func() *gin.Context {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		ctx.Set("id", 1)
		ctx.Set("token_id", 1)
		setQuotaTestRoutingGroup(t, ctx, 1)
		return ctx
	}
	usage := usagePartitionClaudeUsage(t, `{
		"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":5,
		"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":0}
	}`)
	attempt, err := NewAttemptQuota(ctxFactory(), "claude-model", 0, BillingAttemptSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.ApplyReserve(nil); err != nil {
		t.Fatal(err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatal(err)
	}
	result, err := attempt.CloseFromProviderResult(nil, usage, false)
	if err != nil || result.Confirmed || result.Unsettled || result.ChargedQuota != 0 {
		t.Fatalf("missing Claude cache sentinel did not refund reservation: result=%+v err=%v", result, err)
	}
	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 100000 || token.RemainQuota != 100000 || user.UsedQuota != 0 || token.UsedQuota != 0 {
		t.Fatalf("Claude cache refund changed SQL balances: user=%+v token=%+v", user, token)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if len(logs) != 0 {
		t.Fatalf("refunded Claude cache component wrote a consume log: %+v", logs)
	}
}

func TestIndependentSearchRemainsChargeableWithMissingTokens(t *testing.T) {
	pricing := usagePartitionPrice(t, "search-model", 1, 1, nil)
	quota := usagePartitionQuota(t, pricing, "search-model")
	usage := &types.Usage{ExtraBilling: map[string]types.ExtraBilling{
		types.APIToolTypeWebSearch: {ServiceType: types.APIToolTypeWebSearch, Type: "medium", CallCount: 1},
	}}
	usage.MarkProviderExtraBilling(types.APIToolTypeWebSearch, usage.ExtraBilling[types.APIToolTypeWebSearch])
	decision := quota.EvaluateProviderUsage(usage)
	want := int64(defaultExtraServicePrices.WebSearchGA * float64(config.QuotaPerUnit))
	if !decision.Confirm || decision.FinalQuota != want {
		t.Fatalf("independent search component was erased by missing tokens: %+v want=%d", decision, want)
	}
	status := componentStatuses(decision.Components)
	if status["tokens"] != PriceComponentMissingEvidence || status["unit:"+types.APIToolTypeWebSearch] != PriceComponentPriceable {
		t.Fatalf("independent component statuses changed: %+v", decision.Components)
	}
}
