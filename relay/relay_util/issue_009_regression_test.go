package relay_util

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/requester"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	"one-api/providers/ali"
	providersBase "one-api/providers/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestI009AliEmbeddingWireUsageSettlesThroughReducerAndSQL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/services/embeddings/text-embedding/text-embedding" {
			t.Fatalf("unexpected Ali embedding request: %s %s", r.Method, r.URL.Path)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode Ali embedding request: %v", err)
		}
		if request["model"] != "text-embedding-v1" {
			t.Fatalf("unexpected Ali embedding model: %#v", request["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":{"embeddings":[{"embedding":[0.1,0.2],"text_index":0}]},"usage":{"total_tokens":8}}`))
	}))
	defer server.Close()
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	baseURL, proxy := server.URL, ""
	channel := &model.Channel{Type: config.ChannelTypeAli, Key: "test-key", BaseURL: &baseURL, Proxy: &proxy}
	provider, ok := (ali.AliProviderFactory{}).Create(channel).(providersBase.EmbeddingsInterface)
	if !ok {
		t.Fatal("Ali provider does not implement embeddings")
	}
	provider.SetUsage(&types.Usage{PromptTokens: 99})
	request := &types.EmbeddingRequest{Model: "text-embedding-v1", Input: "hello"}
	response, apiErr := provider.CreateEmbeddings(request)
	if apiErr != nil || response == nil || response.Usage == nil {
		t.Fatalf("Ali embedding response usage missing: response=%+v err=%+v", response, apiErr)
	}
	if response.Usage.PromptTokens != 8 || response.Usage.CompletionTokens != 0 || response.Usage.TotalTokens != 8 || !response.Usage.HasProviderUsage() {
		t.Fatalf("Ali embedding wire usage was not converted: %+v", response.Usage)
	}
	internal := provider.GetUsage()
	if internal.PromptTokens != 8 || internal.CompletionTokens != 0 || internal.TotalTokens != 8 || !internal.HasProviderUsage() {
		t.Fatalf("provider usage was not updated with the public snapshot: %+v", internal)
	}

	// 通过真实 reducer 及 SQL 预扣/结算链；输入单价为 1，
	// 因此最终扣款应等于 total_tokens。
	settleI009UsageThroughSQL(t, internal, request.Model, 8)
}

func settleI009UsageThroughSQL(t *testing.T, usage *types.Usage, modelName string, wantCharge int64) {
	t.Helper()

	originalDB := model.DB
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("创建 I009 SQL 测试库失败：%v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserGroup{}); err != nil {
		t.Fatalf("迁移 I009 SQL 测试表失败：%v", err)
	}
	model.DB = db
	t.Cleanup(func() {
		model.DB = originalDB
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	if err := db.Create(&model.User{
		Id:          1,
		Username:    "i009-user",
		Password:    "password123",
		AccessToken: "i009-access-token",
		Quota:       100000,
		Group:       "i009-group",
		Status:      config.UserStatusEnabled,
		Role:        config.RoleCommonUser,
		DisplayName: "I009 User",
		CreatedTime: 1,
	}).Error; err != nil {
		t.Fatalf("写入 I009 用户夹具失败：%v", err)
	}
	if err := db.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id:          1,
		UserId:      1,
		Key:         "i009-token",
		Name:        "i009-token",
		RemainQuota: 100000,
		Group:       "i009-group",
	}).Error; err != nil {
		t.Fatalf("写入 I009 token 夹具失败：%v", err)
	}

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalRedis := config.RedisEnabled
	originalPreConsumed := config.PreConsumedQuota
	originalLogConsume := config.LogConsumeEnabled
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		modelName: {Model: modelName, Type: model.TokensPriceType, Input: 1, Output: 1},
	}}
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	config.PreConsumedQuota = 50
	config.LogConsumeEnabled = false
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.RedisEnabled = originalRedis
		config.PreConsumedQuota = originalPreConsumed
		config.LogConsumeEnabled = originalLogConsume
	})

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/embeddings", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	const group = "i009-group"
	originalGroups := model.GlobalUserGroupRatio.UserGroup
	groups := make(map[string]*model.UserGroup, len(originalGroups)+1)
	for key, value := range originalGroups {
		groups[key] = value
	}
	groups[group] = &model.UserGroup{Symbol: group, Ratio: 1}
	model.GlobalUserGroupRatio.Lock()
	model.GlobalUserGroupRatio.UserGroup = groups
	model.GlobalUserGroupRatio.Unlock()
	groupctx.SetRoutingGroup(ctx, group, groupctx.RoutingGroupSourceUserGroup)
	ctx.Set("group_ratio", 1.0)
	t.Cleanup(func() {
		model.GlobalUserGroupRatio.Lock()
		model.GlobalUserGroupRatio.UserGroup = originalGroups
		model.GlobalUserGroupRatio.Unlock()
	})

	attempt, err := NewAttemptQuota(ctx, modelName, 10, BillingAttemptSpec{})
	if err != nil {
		t.Fatalf("创建 I009 结算 attempt 失败：%v", err)
	}
	if err := attempt.ApplyReserve(context.Background()); err != nil {
		t.Fatalf("I009 预扣失败：%v", err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatalf("I009 提交 claim 失败：%v", err)
	}
	result, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
	if err != nil || result.Unsettled || !result.Confirmed || result.ChargedQuota != wantCharge {
		t.Fatalf("I009 provider usage 未通过 reducer 结算：result=%+v err=%v usage=%+v", result, err, usage)
	}

	var user model.User
	if err := db.First(&user, 1).Error; err != nil {
		t.Fatalf("读取 I009 用户余额失败：%v", err)
	}
	var token model.Token
	if err := db.First(&token, 1).Error; err != nil {
		t.Fatalf("读取 I009 token 余额失败：%v", err)
	}
	if user.Quota != 100000-int(wantCharge) || token.RemainQuota != 100000-int(wantCharge) || int64(token.UsedQuota) != wantCharge {
		t.Fatalf("I009 SQL 扣款不是 %d：user=%+v token=%+v", wantCharge, user, token)
	}
}
