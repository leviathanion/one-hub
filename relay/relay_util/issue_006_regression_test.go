package relay_util

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/openai"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func issue006GroupName() string {
	return "issue006-free-group"
}

func installIssue006PersistedGroup(t *testing.T, ratio float64) string {
	t.Helper()
	if err := model.DB.AutoMigrate(&model.UserGroup{}, &model.PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := model.EnsurePublicationVersionRows(model.DB); err != nil {
		t.Fatal(err)
	}
	originalGroups := model.GlobalUserGroupRatio
	model.GlobalUserGroupRatio = &model.UserGroupRatio{}
	t.Cleanup(func() {
		for _, limiter := range model.GlobalUserGroupRatio.APILimiter {
			if stopper, ok := limiter.(interface{ Stop() }); ok {
				stopper.Stop()
			}
		}
		model.GlobalUserGroupRatio = originalGroups
	})
	enabled := true
	group := &model.UserGroup{
		Symbol: issue006GroupName(), Name: issue006GroupName(), Ratio: ratio,
		APIRate: 600, Public: true, Enable: &enabled,
	}
	if err := group.Create(); err != nil {
		t.Fatalf("persist zero/positive group: %v", err)
	}
	return group.Symbol
}

func TestIssue006CurrentGroupRatioAllowsZeroAndRejectsInvalidValues(t *testing.T) {
	for _, test := range []struct {
		name    string
		ratio   float64
		wantErr bool
	}{
		{name: "zero", ratio: 0},
		{name: "positive", ratio: 1.5},
		{name: "negative", ratio: -1, wantErr: true},
		{name: "positive infinity", ratio: math.Inf(1), wantErr: true},
		{name: "negative infinity", ratio: math.Inf(-1), wantErr: true},
		{name: "nan", ratio: math.NaN(), wantErr: true},
		{name: "missing", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name != "missing" {
				useCurrentQuotaGroup(t, issue006GroupName(), test.ratio)
			}
			quota := &Quota{groupName: issue006GroupName()}
			got, err := quota.currentGroupRatio()
			if test.wantErr {
				if err == nil {
					t.Fatalf("invalid group ratio was accepted: got=%v", got)
				}
				return
			}
			if err != nil || got != test.ratio {
				t.Fatalf("group ratio=%v err=%v, want %v", got, err, test.ratio)
			}
		})
	}
}

func runIssue006HTTPSettlement(t *testing.T, groupRatio float64, initialBalance int, wantQuota int64, wantReservation int) {
	t.Helper()
	useQuotaReserveTestDB(t)
	if err := model.DB.AutoMigrate(&model.Log{}); err != nil {
		t.Fatal(err)
	}
	insertQuotaReserveFixtures(t, initialBalance)

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalRedis := config.RedisEnabled
	originalLog := config.LogConsumeEnabled
	originalPreConsumed := config.PreConsumedQuota
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"issue006-model": {Model: "issue006-model", Type: model.TokensPriceType, Input: 2, Output: 3},
	}}
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	config.LogConsumeEnabled = true
	config.PreConsumedQuota = 500
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.RedisEnabled = originalRedis
		config.LogConsumeEnabled = originalLog
		config.PreConsumedQuota = originalPreConsumed
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	group := installIssue006PersistedGroup(t, groupRatio)
	groupctx.SetRoutingGroup(ctx, group, groupctx.RoutingGroupSourceUserGroup)
	ctx.Set("group_ratio", groupRatio)
	attempt, err := NewAttemptQuota(ctx, "issue006-model", 10, BillingAttemptSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.ApplyReserve(context.Background()); err != nil {
		t.Fatalf("group ratio %v should be admissible: %v", groupRatio, err)
	}
	if got := attempt.ReservedQuota(); got != int64(wantReservation) {
		t.Fatalf("reserved quota=%d want=%d", got, wantReservation)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatal(err)
	}

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"issue006-response","model":"issue006-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })
	proxy, endpoint := "", server.URL
	provider := (openai.OpenAIProviderFactory{}).Create(&model.Channel{
		Type: config.ChannelTypeOpenAI, Key: "issue006-key", Proxy: &proxy, BaseURL: &endpoint,
	}).(*openai.OpenAIProvider)
	provider.SetContext(ctx)
	provider.SetUsage(&types.Usage{})
	response, apiErr := provider.CreateChatCompletion(&types.ChatCompletionRequest{
		Model:    "issue006-model",
		Messages: []types.ChatCompletionMessage{{Role: types.ChatMessageRoleUser, Content: "hello"}},
	})
	if apiErr != nil || response == nil {
		t.Fatalf("local upstream request failed: response=%+v err=%+v", response, apiErr)
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d want=1", calls)
	}
	result, err := attempt.CloseFromProviderResult(ctx.Request.Context(), provider.GetUsage(), false)
	if err != nil || !result.Confirmed || result.Unsettled || result.ChargedQuota != wantQuota {
		t.Fatalf("settlement result=%+v err=%v want quota=%d", result, err, wantQuota)
	}

	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != initialBalance-int(wantQuota) || token.RemainQuota != initialBalance-int(wantQuota) || user.UsedQuota != int(wantQuota) || token.UsedQuota != int(wantQuota) || user.RequestCount != 1 {
		t.Fatalf("SQL settlement mismatch: user=%+v token=%+v", user, token)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Quota != int(wantQuota) || logs[0].PromptTokens != 10 || logs[0].CompletionTokens != 2 {
		t.Fatalf("consume audit mismatch: %+v", logs)
	}
}

func TestIssue006PublishedZeroGroupChargesZeroWithoutSkippingRequest(t *testing.T) {
	runIssue006HTTPSettlement(t, 0, 100, 0, 0)
}

func TestIssue006PositiveGroupRestoresNormalCharge(t *testing.T) {
	runIssue006HTTPSettlement(t, 1, 100000, 26, 520)
}

func TestIssue006ZeroGroupStillChecksTokenValidity(t *testing.T) {
	useQuotaReserveTestDB(t)
	insertQuotaReserveFixtures(t, 100)
	if err := model.DB.Model(&model.Token{}).Where("id = ?", 1).Update("status", config.TokenStatusDisabled).Error; err != nil {
		t.Fatal(err)
	}
	originalPricing := model.PricingInstance
	originalPreConsumed := config.PreConsumedQuota
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"issue006-invalid-token": {Model: "issue006-invalid-token", Type: model.TokensPriceType, Input: 1, Output: 1},
	}}
	config.PreConsumedQuota = 500
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.PreConsumedQuota = originalPreConsumed
	})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	setQuotaTestRoutingGroup(t, ctx, 0)
	attempt, err := NewAttemptQuota(ctx, "issue006-invalid-token", 0, BillingAttemptSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.ApplyReserve(context.Background()); err == nil {
		t.Fatal("disabled token was admitted by a free group")
	}
}

func TestIssue006ZeroGroupScalesTimesAndIndependentUnits(t *testing.T) {
	useCurrentQuotaGroup(t, issue006GroupName(), 0)
	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"issue006-times":  {Model: "issue006-times", Type: model.TimesPriceType, Input: 2},
		"issue006-search": {Model: "issue006-search", Type: model.TokensPriceType, Input: 1, Output: 1},
	}}
	t.Cleanup(func() { model.PricingInstance = originalPricing })

	timesQuota := &Quota{modelName: "issue006-times", groupName: issue006GroupName()}
	operation := &types.Usage{}
	operation.MarkProviderOperationUnits(3)
	operationDecision := timesQuota.EvaluateProviderUsage(operation)
	if !operationDecision.Confirm || operationDecision.FinalQuota != 0 || componentStatuses(operationDecision.Components)["operation_units"] != PriceComponentPriceable {
		t.Fatalf("times component bypassed zero group: %+v", operationDecision)
	}

	searchQuota := &Quota{modelName: "issue006-search", groupName: issue006GroupName()}
	search := &types.Usage{ExtraBilling: map[string]types.ExtraBilling{
		types.APIToolTypeWebSearch: {ServiceType: types.APIToolTypeWebSearch, CallCount: 2},
	}}
	search.MarkProviderExtraBilling(types.APIToolTypeWebSearch, search.ExtraBilling[types.APIToolTypeWebSearch])
	searchDecision := searchQuota.EvaluateProviderUsage(search)
	status := componentStatuses(searchDecision.Components)
	if !searchDecision.Confirm || searchDecision.FinalQuota != 0 || status["unit:"+types.APIToolTypeWebSearch] != PriceComponentPriceable {
		t.Fatalf("independent component bypassed zero group: %+v", searchDecision)
	}
}

func TestIssue006MissingGroupCannotBecomeFreeAdmission(t *testing.T) {
	quota := &Quota{groupName: issue006GroupName() + "-missing"}
	if _, err := quota.currentGroupRatio(); err == nil {
		t.Fatal("missing group became an admissible free group")
	}
}
