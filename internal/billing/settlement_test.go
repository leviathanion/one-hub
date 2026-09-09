package billing

import (
	"context"
	"testing"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	"one-api/types"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestUsageSummaryPreservesNonIntegralUsageUnits(t *testing.T) {
	usage := &types.Usage{ExtraUsageUnits: map[string]float64{config.UsageExtraInputAudioTranscription: 2.5}}
	summary := NewUsageSummary(usage)
	restored := summary.ToUsage()
	if restored.ExtraUsageUnits[config.UsageExtraInputAudioTranscription] != 2.5 {
		t.Fatalf("settlement usage units were lost: %+v", restored.ExtraUsageUnits)
	}
	restored.ExtraUsageUnits[config.UsageExtraInputAudioTranscription] = 7
	if summary.ExtraUsageUnits[config.UsageExtraInputAudioTranscription] != 2.5 {
		t.Fatalf("settlement usage units share mutable state: %+v", summary.ExtraUsageUnits)
	}
}

func TestUsageSummaryPreservesDistinctProviderCacheEvidence(t *testing.T) {
	usage := &types.Usage{PromptTokensDetails: types.PromptTokensDetails{
		CachedTokens:      2,
		CachedReadTokens:  3,
		CacheWriteTokens:  5,
		CachedWriteTokens: 7,
	}}
	restored := NewUsageSummary(usage).ToUsage()
	extraTokens := restored.GetExtraTokens()
	if extraTokens[config.UsageExtraCache] != 2 || extraTokens[config.UsageExtraCachedRead] != 3 || extraTokens[config.UsageExtraCacheWrite] != 5 || extraTokens[config.UsageExtraCachedWrite] != 7 {
		t.Fatalf("settlement summary merged or lost provider cache evidence: %+v", extraTokens)
	}
}

func useSettlementTestDB(t *testing.T) {
	t.Helper()

	logger.Logger = zap.NewNop()

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Log{}, &model.UserGroup{}); err != nil {
		t.Fatalf("expected settlement schema migration to succeed, got %v", err)
	}

	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
	})
}

func insertSettlementFixtures(t *testing.T) {
	t.Helper()

	if err := model.DB.Create(&model.User{
		Id:            1,
		Username:      "alice",
		Password:      "password123",
		AccessToken:   "access-token-1",
		Quota:         1000,
		Group:         "default",
		Status:        config.UserStatusEnabled,
		Role:          config.RoleCommonUser,
		DisplayName:   "Alice",
		CreatedTime:   1,
		LastLoginIp:   "127.0.0.1",
		LastLoginTime: 1,
	}).Error; err != nil {
		t.Fatalf("expected user fixture to persist, got %v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id:          1,
		UserId:      1,
		Key:         "token-key-1",
		Name:        "token-alpha",
		RemainQuota: 1000,
		Group:       "default",
	}).Error; err != nil {
		t.Fatalf("expected token fixture to persist, got %v", err)
	}
	if err := model.DB.Create(&model.Channel{
		Id:     1,
		Name:   "channel-alpha",
		Key:    "sk-test",
		Group:  "default",
		Models: "gpt-5",
	}).Error; err != nil {
		t.Fatalf("expected channel fixture to persist, got %v", err)
	}
}

func TestApplySettlementTruthBypassesBatchUpdate(t *testing.T) {
	useSettlementTestDB(t)
	insertSettlementFixtures(t)

	originalBatch := config.BatchUpdateEnabled
	originalLogConsume := config.LogConsumeEnabled
	config.BatchUpdateEnabled = true
	config.LogConsumeEnabled = false
	t.Cleanup(func() {
		config.BatchUpdateEnabled = originalBatch
		config.LogConsumeEnabled = originalLogConsume
	})
	reserve, err := model.ApplyBillingReserve(context.Background(), 1, 1, 100)
	if err != nil {
		t.Fatal(err)
	}

	cmd := SettlementCommand{
		RequestKind:            SettlementRequestKindUnary,
		UserID:                 1,
		TokenID:                1,
		ChannelID:              1,
		ModelName:              "gpt-5",
		PreConsumedQuota:       100,
		PreconsumeTokenApplied: reserve.TokenQuotaApplied,
		FinalQuota:             250,
		UsageSummary: UsageSummary{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		},
	}

	result, err := ApplySettlement(context.Background(), cmd, &SettlementOptions{})
	if err != nil {
		t.Fatalf("expected settlement to succeed, got %v", err)
	}
	if !result.TruthApplied || result.Delta != 150 {
		t.Fatalf("expected truth apply with delta 150, got %+v", result)
	}

	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup to succeed, got %v", err)
	}
	if user.Quota != 750 {
		t.Fatalf("expected direct truth path to decrease user quota immediately, got %d", user.Quota)
	}

	var token model.Token
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup to succeed, got %v", err)
	}
	if token.RemainQuota != 750 || token.UsedQuota != 250 {
		t.Fatalf("expected direct truth path to update token quota immediately, got remain=%d used=%d", token.RemainQuota, token.UsedQuota)
	}
}

func TestApplySettlementProjectionUsesFinalQuota(t *testing.T) {
	useSettlementTestDB(t)
	insertSettlementFixtures(t)

	originalBatch := config.BatchUpdateEnabled
	originalLogConsume := config.LogConsumeEnabled
	config.BatchUpdateEnabled = false
	config.LogConsumeEnabled = true
	t.Cleanup(func() {
		config.BatchUpdateEnabled = originalBatch
		config.LogConsumeEnabled = originalLogConsume
	})
	reserve, err := model.ApplyBillingReserve(context.Background(), 1, 1, 100)
	if err != nil {
		t.Fatal(err)
	}

	cmd := SettlementCommand{
		RequestKind:            SettlementRequestKindUnary,
		UserID:                 1,
		TokenID:                1,
		ChannelID:              1,
		ModelName:              "gpt-5",
		PreConsumedQuota:       100,
		PreconsumeTokenApplied: reserve.TokenQuotaApplied,
		FinalQuota:             250,
		UsageSummary: UsageSummary{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
			PromptTokensDetails: types.PromptTokensDetails{
				CachedTokens: 3,
			},
			ExtraTokens: map[string]int{
				config.UsageExtraCachedRead:  4,
				config.UsageExtraCacheWrite:  2,
				config.UsageExtraCachedWrite: 5,
			},
		},
	}
	opts := SettlementOptions{
		Projection: SettlementProjection{
			TokenName:   "token-alpha",
			RequestTime: 321,
			SourceIP:    "203.0.113.9",
			Metadata: map[string]any{
				"user_agent": "Codex/1.2",
			},
		},
	}

	if _, err := ApplySettlement(context.Background(), cmd, &opts); err != nil {
		t.Fatalf("expected settlement to succeed, got %v", err)
	}

	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup to succeed, got %v", err)
	}
	if user.Quota != 750 || user.UsedQuota != 250 || user.RequestCount != 1 {
		t.Fatalf("expected final quota projection to update user counters, got quota=%d used=%d requests=%d", user.Quota, user.UsedQuota, user.RequestCount)
	}

	var channel model.Channel
	if err := model.DB.First(&channel, 1).Error; err != nil {
		t.Fatalf("expected channel lookup to succeed, got %v", err)
	}
	if channel.UsedQuota != 250 {
		t.Fatalf("expected channel used quota to project final quota 250, got %d", channel.UsedQuota)
	}

	var log model.Log
	if err := model.DB.Where("user_id = ?", 1).First(&log).Error; err != nil {
		t.Fatalf("expected consume log lookup to succeed, got %v", err)
	}
	if log.Quota != 250 || log.PromptTokens != 10 || log.CompletionTokens != 20 {
		t.Fatalf("expected consume log to record final quota and usage, got %+v", log)
	}
	if log.CacheTokens != 3 || log.CacheReadTokens != 4 || log.CacheWriteTokens != 7 {
		t.Fatalf("expected consume log to persist cache token breakdown, got %+v", log)
	}
	if log.Metadata.Data()["user_agent"] != "Codex/1.2" {
		t.Fatalf("expected consume log to persist metadata user-agent, got %#v", log.Metadata.Data())
	}
}
