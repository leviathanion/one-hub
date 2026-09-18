package relay_util

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/controller"
	"one-api/middleware"
	"one-api/model"
	"one-api/providers/claude"
	"one-api/types"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

func TestI030ClaudeTTLProviderToSQLUsesConfiguredFallbackOnce(t *testing.T) {
	for _, test := range []struct {
		name        string
		ratios      map[string]float64
		fiveMinutes int
		oneHour     int
		wantCharge  int64
	}{
		{name: "通用零值", ratios: map[string]float64{config.UsageExtraCacheCreationInputTokens: 0}, fiveMinutes: 2000, wantCharge: 30},
		{name: "通用半价", ratios: map[string]float64{config.UsageExtraCacheCreationInputTokens: 0.5}, fiveMinutes: 2000, wantCharge: 1030},
		{name: "通用双倍", ratios: map[string]float64{config.UsageExtraCacheCreationInputTokens: 2}, fiveMinutes: 2000, wantCharge: 4030},
		{name: "显式五分钟覆盖", ratios: map[string]float64{
			config.UsageExtraCacheCreationInputTokens:        0.5,
			config.UsageExtraEphemeral5mInputTokens: 0.25,
		}, fiveMinutes: 2000, wantCharge: 530},
		{name: "仅显式一小时不改变五分钟", ratios: map[string]float64{
			config.UsageExtraCacheCreationInputTokens:        0.5,
			config.UsageExtraEphemeral1hInputTokens: 0.25,
		}, fiveMinutes: 2000, wantCharge: 1030},
		{name: "五分钟与一小时混合", ratios: map[string]float64{
			config.UsageExtraCacheCreationInputTokens:        0.5,
			config.UsageExtraEphemeral5mInputTokens: 0.25,
			config.UsageExtraEphemeral1hInputTokens: 2,
		}, fiveMinutes: 2000, oneHour: 1000, wantCharge: 2530},
	} {
		t.Run(test.name, func(t *testing.T) {
			useQuotaReserveTestDB(t)
			insertQuotaReserveFixtures(t, 100000)
			if err := model.DB.AutoMigrate(&model.Price{}, &model.ModelInfo{}, &model.PublicationVersion{}); err != nil {
				t.Fatal(err)
			}
			if err := model.EnsurePublicationVersionRows(model.DB); err != nil {
				t.Fatal(err)
			}

			originalPricing := model.PricingInstance
			originalBatch := config.BatchUpdateEnabled
			originalRedis := config.RedisEnabled
			originalReserve := config.PreConsumedQuota
			originalLog := config.LogConsumeEnabled
			extra := datatypes.NewJSONType(test.ratios)
			pricing := &model.Pricing{Prices: make(map[string]*model.Price)}
			model.PricingInstance = pricing
			if err := pricing.Init(); err != nil {
				t.Fatal(err)
			}
			if err := pricing.AddPrice(&model.Price{
				Model:       "claude-i030",
				Type:        model.TokensPriceType,
				Input:       1,
				Output:      1,
				ExtraRatios: &extra,
			}); err != nil {
				t.Fatal(err)
			}
			if pricing.PublishedVersion() != 2 {
				t.Fatalf("后台保存价格后本地 publication version=%d want=2", pricing.PublishedVersion())
			}
			config.BatchUpdateEnabled = false
			config.RedisEnabled = false
			config.PreConsumedQuota = 50
			config.LogConsumeEnabled = false
			t.Cleanup(func() {
				model.PricingInstance = originalPricing
				config.BatchUpdateEnabled = originalBatch
				config.RedisEnabled = originalRedis
				config.PreConsumedQuota = originalReserve
				config.LogConsumeEnabled = originalLog
			})

			wire := fmt.Sprintf(`{"id":"msg-i030","type":"message","role":"assistant","model":"claude-i030","content":[],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":%d,"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":%d}}}`, test.fiveMinutes+test.oneHour, test.fiveMinutes, test.oneHour)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, wire)
			}))
			t.Cleanup(server.Close)
			originalClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = originalClient })

			proxy := ""
			provider := claude.CreateClaudeProvider(&model.Channel{Key: "test-token", Proxy: &proxy}, server.URL)
			provider.SetUsage(&types.Usage{})
			providerContext, _ := gin.CreateTestContext(httptest.NewRecorder())
			providerContext.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-i030","max_tokens":64,"messages":[]}`))
			if _, err := common.CacheRequestBody(providerContext); err != nil {
				t.Fatal(err)
			}
			provider.SetContext(providerContext)
			response, apiErr := provider.CreateClaudeChat(&claude.ClaudeRequest{Model: "claude-i030", MaxTokens: 64})
			if apiErr != nil || response == nil {
				t.Fatalf("Claude provider request failed: response=%+v error=%+v", response, apiErr)
			}
			usage := provider.GetUsage()
			if !usage.ProviderReported || usage.PromptTokens != 10+test.fiveMinutes+test.oneHour || usage.CompletionTokens != 20 || usage.TotalTokens != 30+test.fiveMinutes+test.oneHour {
				t.Fatalf("Claude provider changed TTL usage evidence: %+v", usage)
			}
			if usage.GetExtraTokens()[config.UsageExtraEphemeral5mInputTokens] != test.fiveMinutes || usage.GetExtraTokens()[config.UsageExtraEphemeral1hInputTokens] != test.oneHour {
				t.Fatalf("Claude TTL evidence was not preserved: %+v", usage.GetExtraTokens())
			}
			if _, present := usage.GetExtraTokens()[config.UsageExtraCacheCreationInputTokens]; present {
				t.Fatalf("generic cache-write evidence was synthesized alongside TTL evidence: %+v", usage.GetExtraTokens())
			}

			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			ctx.Set("id", 1)
			ctx.Set("token_id", 1)
			setQuotaTestRoutingGroup(t, ctx, 1)
			attempt, err := NewAttemptQuota(ctx, "claude-i030", 10, BillingAttemptSpec{})
			if err != nil {
				t.Fatal(err)
			}
			if err := attempt.ApplyReserve(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := attempt.ClaimSubmission(); err != nil {
				t.Fatal(err)
			}
			result, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
			if err != nil || !result.Confirmed || result.ChargedQuota != test.wantCharge {
				t.Fatalf("Claude TTL price did not settle through SQL: result=%+v error=%v", result, err)
			}
			var user model.User
			var token model.Token
			if err := model.DB.First(&user, 1).Error; err != nil {
				t.Fatal(err)
			}
			if err := model.DB.First(&token, 1).Error; err != nil {
				t.Fatal(err)
			}
			var savedPrice model.Price
			if err := model.DB.Where("model = ?", "claude-i030").First(&savedPrice).Error; err != nil {
				t.Fatal(err)
			}
			if savedPrice.ExtraRatios == nil {
				t.Fatal("后台保存的 extra_ratios 缺失")
			}
			savedRatios := savedPrice.ExtraRatios.Data()
			if len(savedRatios) != len(test.ratios) {
				t.Fatalf("后台保存的 extra_ratios 被复制或丢失：got=%+v want=%+v", savedRatios, test.ratios)
			}
			for key, want := range test.ratios {
				got, present := savedRatios[key]
				if !present || got != want {
					t.Fatalf("后台保存的 extra_ratio[%s] 不一致：got=%v present=%v want=%v", key, got, present, want)
				}
			}
			if user.Quota != 100000-int(test.wantCharge) || token.RemainQuota != 100000-int(test.wantCharge) || user.UsedQuota != int(test.wantCharge) || token.UsedQuota != int(test.wantCharge) {
				t.Fatalf("SQL balances do not match Claude TTL charge: user=%+v token=%+v want=%d", user, token, test.wantCharge)
			}
		})
	}
}

func issue030ManagementRequest(t *testing.T, router http.Handler, method, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func issue030AssertManagementResponse(t *testing.T, response *httptest.ResponseRecorder, wantStatus int, wantSuccess bool) {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf("价格管理 HTTP status=%d want=%d body=%s", response.Code, wantStatus, response.Body.String())
	}
	var envelope struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Success != wantSuccess {
		t.Fatalf("价格管理 HTTP success=%t want=%t message=%q", envelope.Success, wantSuccess, envelope.Message)
	}
}

func issue030PriceManagementPayload(expectedVersion int64, ratios map[string]float64) map[string]any {
	return map[string]any{
		"expected_version": expectedVersion,
		"model":            "claude-i030",
		"type":             model.TokensPriceType,
		"channel_type":     1,
		"input":            1,
		"output":           1,
		"locked":           false,
		"extra_ratios":     ratios,
	}
}

func issue030AssertStoredRatios(t *testing.T, want map[string]float64) {
	t.Helper()
	var saved model.Price
	if err := model.DB.Where("model = ?", "claude-i030").First(&saved).Error; err != nil {
		t.Fatal(err)
	}
	if saved.ExtraRatios == nil {
		if len(want) == 0 {
			return
		}
		t.Fatalf("stored extra_ratios missing: want=%+v", want)
	}
	got := saved.ExtraRatios.Data()
	if len(got) != len(want) {
		t.Fatalf("stored extra_ratios changed shape: got=%+v want=%+v", got, want)
	}
	for key, expected := range want {
		actual, present := got[key]
		if !present || actual != expected {
			t.Fatalf("stored extra_ratio[%s]=%v present=%t want=%v", key, actual, present, expected)
		}
	}
}

func issue030SettleManagementClaude(t *testing.T, fiveMinutes, oneHour int, wantCharge int64) {
	t.Helper()
	wire := fmt.Sprintf(`{"id":"msg-i030-management","type":"message","role":"assistant","model":"claude-i030","content":[],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":%d,"cache_creation":{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":%d}}}`, fiveMinutes+oneHour, fiveMinutes, oneHour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, wire)
	}))
	t.Cleanup(server.Close)
	originalClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalClient })

	proxy := ""
	provider := claude.CreateClaudeProvider(&model.Channel{Key: "test-token", Proxy: &proxy}, server.URL)
	provider.SetUsage(&types.Usage{})
	providerContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	providerContext.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-i030","max_tokens":64,"messages":[]}`))
	if _, err := common.CacheRequestBody(providerContext); err != nil {
		t.Fatal(err)
	}
	provider.SetContext(providerContext)
	response, apiErr := provider.CreateClaudeChat(&claude.ClaudeRequest{Model: "claude-i030", MaxTokens: 64})
	if apiErr != nil || response == nil {
		t.Fatalf("Claude provider request failed: response=%+v error=%+v", response, apiErr)
	}
	usage := provider.GetUsage()
	if !usage.ProviderReported || usage.PromptTokens != 10+fiveMinutes+oneHour || usage.CompletionTokens != 20 || usage.TotalTokens != 30+fiveMinutes+oneHour {
		t.Fatalf("Claude provider usage changed: %+v", usage)
	}
	if usage.GetExtraTokens()[config.UsageExtraEphemeral5mInputTokens] != fiveMinutes || usage.GetExtraTokens()[config.UsageExtraEphemeral1hInputTokens] != oneHour {
		t.Fatalf("Claude TTL evidence changed: %+v", usage.GetExtraTokens())
	}
	if _, present := usage.GetExtraTokens()[config.UsageExtraCacheCreationInputTokens]; present {
		t.Fatalf("generic cache-write evidence was synthesized: %+v", usage.GetExtraTokens())
	}

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	setQuotaTestRoutingGroup(t, ctx, 1)
	attempt, err := NewAttemptQuota(ctx, "claude-i030", 10, BillingAttemptSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.ApplyReserve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatal(err)
	}
	result, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
	if err != nil || !result.Confirmed || result.ChargedQuota != wantCharge {
		t.Fatalf("Claude management price did not settle through SQL: result=%+v error=%v", result, err)
	}
}

func TestI030ClaudeManagementHTTPToSQLPreservesFallbackDeletion(t *testing.T) {
	useQuotaReserveTestDB(t)
	insertQuotaReserveFixtures(t, 100000)
	if err := model.DB.Model(&model.User{}).Where("id = ?", 1).Update("role", config.RoleAdminUser).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.AutoMigrate(&model.Price{}, &model.ModelInfo{}, &model.PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := model.EnsurePublicationVersionRows(model.DB); err != nil {
		t.Fatal(err)
	}

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalRedis := config.RedisEnabled
	originalReserve := config.PreConsumedQuota
	originalLog := config.LogConsumeEnabled
	pricing := &model.Pricing{Prices: make(map[string]*model.Price)}
	model.PricingInstance = pricing
	if err := pricing.Init(); err != nil {
		t.Fatal(err)
	}
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	config.PreConsumedQuota = 50
	config.LogConsumeEnabled = false
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.RedisEnabled = originalRedis
		config.PreConsumedQuota = originalReserve
		config.LogConsumeEnabled = originalLog
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(sessions.Sessions("session", cookie.NewStore([]byte("i030-management-test-secret"))))
	router.Use(func(c *gin.Context) {
		session := sessions.Default(c)
		session.Set("username", "i030-admin")
		session.Set("id", 1)
		session.Set("status", config.UserStatusEnabled)
		session.Set("role", config.RoleAdminUser)
		c.Next()
	})
	pricesRoute := router.Group("/api/prices")
	pricesRoute.Use(middleware.AdminAuth())
	pricesRoute.POST("/single", controller.AddPrice)
	pricesRoute.PUT("/single/*model", controller.UpdatePrice)
	pricesRoute.POST("/sync/preview", controller.PreviewPriceChange)
	pricesRoute.POST("/sync/apply", controller.ApplyPriceChange)

	genericZero := map[string]float64{config.UsageExtraCacheCreationInputTokens: 0}
	response := issue030ManagementRequest(t, router, http.MethodPost, "/api/prices/single", issue030PriceManagementPayload(1, genericZero))
	issue030AssertManagementResponse(t, response, http.StatusOK, true)
	if got := pricing.PublishedVersion(); got != 2 {
		t.Fatalf("AddPrice publication version=%d want=2", got)
	}
	issue030AssertStoredRatios(t, genericZero)

	stale := issue030ManagementRequest(t, router, http.MethodPut, "/api/prices/single/claude-i030", issue030PriceManagementPayload(1, map[string]float64{
		config.UsageExtraCacheCreationInputTokens:        0,
		config.UsageExtraEphemeral5mInputTokens: 0,
	}))
	issue030AssertManagementResponse(t, stale, http.StatusConflict, false)
	if got := pricing.PublishedVersion(); got != 2 {
		t.Fatalf("stale CAS changed publication version to %d", got)
	}

	// The import payload changes only rate_rules and carries no TTL keys. It
	// exercises the same source shape used by CheckUpdates without materializing
	// absent Claude partitions.
	importSource := []map[string]any{{
		"model":        "claude-i030",
		"type":         model.TokensPriceType,
		"channel_type": 1,
		"input":        1,
		"output":       1,
		"locked":       false,
		"extra_ratios": genericZero,
		"rate_rules":   map[string]any{},
	}}
	preview := issue030ManagementRequest(t, router, http.MethodPost, "/api/prices/sync/preview", map[string]any{
		"mode":   string(model.PriceUpdateModeUpdate),
		"source": importSource,
	})
	issue030AssertManagementResponse(t, preview, http.StatusOK, true)
	var previewEnvelope struct {
		Data struct {
			BaseVersion int64  `json:"base_version"`
			Digest      string `json:"digest"`
		} `json:"data"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &previewEnvelope); err != nil {
		t.Fatal(err)
	}
	if previewEnvelope.Data.BaseVersion != 2 || previewEnvelope.Data.Digest == "" {
		t.Fatalf("unexpected import preview: %+v", previewEnvelope.Data)
	}
	apply := issue030ManagementRequest(t, router, http.MethodPost, "/api/prices/sync/apply", map[string]any{
		"mode":         string(model.PriceUpdateModeUpdate),
		"source":       importSource,
		"base_version": previewEnvelope.Data.BaseVersion,
		"digest":       previewEnvelope.Data.Digest,
	})
	issue030AssertManagementResponse(t, apply, http.StatusOK, true)
	var applyEnvelope struct {
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal(apply.Body.Bytes(), &applyEnvelope); err != nil {
		t.Fatal(err)
	}
	if applyEnvelope.Version != 3 || pricing.PublishedVersion() != 3 {
		t.Fatalf("import publication version=%d local=%d want=3", applyEnvelope.Version, pricing.PublishedVersion())
	}
	issue030AssertStoredRatios(t, genericZero)

	explicitTTLZero := map[string]float64{
		config.UsageExtraCacheCreationInputTokens:        0,
		config.UsageExtraEphemeral5mInputTokens: 0,
	}
	response = issue030ManagementRequest(t, router, http.MethodPut, "/api/prices/single/claude-i030", issue030PriceManagementPayload(3, explicitTTLZero))
	issue030AssertManagementResponse(t, response, http.StatusOK, true)
	if got := pricing.PublishedVersion(); got != 4 {
		t.Fatalf("TTL zero update publication version=%d want=4", got)
	}
	issue030AssertStoredRatios(t, explicitTTLZero)
	issue030SettleManagementClaude(t, 2000, 0, 30)

	response = issue030ManagementRequest(t, router, http.MethodPut, "/api/prices/single/claude-i030", issue030PriceManagementPayload(4, genericZero))
	issue030AssertManagementResponse(t, response, http.StatusOK, true)
	if got := pricing.PublishedVersion(); got != 5 {
		t.Fatalf("TTL deletion publication version=%d want=5", got)
	}
	issue030AssertStoredRatios(t, genericZero)
	issue030SettleManagementClaude(t, 2000, 0, 30)

	emptyRatios := map[string]float64{}
	response = issue030ManagementRequest(t, router, http.MethodPut, "/api/prices/single/claude-i030", issue030PriceManagementPayload(5, emptyRatios))
	issue030AssertManagementResponse(t, response, http.StatusOK, true)
	if got := pricing.PublishedVersion(); got != 6 {
		t.Fatalf("generic deletion publication version=%d want=6", got)
	}
	issue030AssertStoredRatios(t, emptyRatios)
	issue030SettleManagementClaude(t, 2000, 0, 2530)

	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 100000-2590 || token.RemainQuota != 100000-2590 || user.UsedQuota != 2590 || token.UsedQuota != 2590 {
		t.Fatalf("SQL balances do not match management fallback charges: user=%+v token=%+v", user, token)
	}
}
