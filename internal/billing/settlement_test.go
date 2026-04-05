package billing

import (
	"context"
	"fmt"
	"testing"

	"one-api/common/config"
	"one-api/common/logger"
	commonredis "one-api/common/redis"
	"one-api/internal/testutil/fakeredis"
	"one-api/model"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useSettlementTestDB(t *testing.T) {
	t.Helper()

	logger.Logger = zap.NewNop()

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Log{}); err != nil {
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

	cmd := SettlementCommand{
		RequestKind:      SettlementRequestKindUnary,
		UserID:           1,
		TokenID:          1,
		ChannelID:        1,
		ModelName:        "gpt-5",
		PreConsumedQuota: 100,
		FinalQuota:       250,
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
	if user.Quota != 850 {
		t.Fatalf("expected direct truth path to decrease user quota immediately, got %d", user.Quota)
	}

	var token model.Token
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup to succeed, got %v", err)
	}
	if token.RemainQuota != 850 || token.UsedQuota != 150 {
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

	cmd := SettlementCommand{
		RequestKind:      SettlementRequestKindUnary,
		UserID:           1,
		TokenID:          1,
		ChannelID:        1,
		ModelName:        "gpt-5",
		PreConsumedQuota: 100,
		FinalQuota:       250,
		UsageSummary: UsageSummary{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
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
	if user.Quota != 850 || user.UsedQuota != 250 || user.RequestCount != 1 {
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
	if log.Metadata.Data()["user_agent"] != "Codex/1.2" {
		t.Fatalf("expected consume log to persist metadata user-agent, got %#v", log.Metadata.Data())
	}
}

func TestApplySettlementDeduplicatesDetachedFinalize(t *testing.T) {
	useSettlementTestDB(t)
	insertSettlementFixtures(t)

	server, err := fakeredis.Start()
	if err != nil {
		t.Fatalf("expected fake redis server to start, got %v", err)
	}
	defer server.Close()
	server.RegisterLuaScript(settlementAcquireGateScriptSource, func(keys, args []string) int64 {
		current, ok := server.GetRaw(keys[0])
		if !ok {
			server.SetRaw(keys[0], args[0])
			return 1
		}
		if current == args[0] {
			return 0
		}
		return -1
	})

	originalRedisEnabled := config.RedisEnabled
	originalRedisClient := commonredis.RDB
	originalBatch := config.BatchUpdateEnabled
	originalLogConsume := config.LogConsumeEnabled
	config.RedisEnabled = true
	config.BatchUpdateEnabled = false
	config.LogConsumeEnabled = false
	commonredis.RDB = server.Client()
	t.Cleanup(func() {
		config.RedisEnabled = originalRedisEnabled
		config.BatchUpdateEnabled = originalBatch
		config.LogConsumeEnabled = originalLogConsume
		commonredis.RDB = originalRedisClient
	})

	cmd := SettlementCommand{
		Identity:         "session-1:1:finalize",
		RequestKind:      SettlementRequestKindRealtimeTurn,
		UserID:           1,
		TokenID:          1,
		ChannelID:        1,
		ModelName:        "gpt-5",
		PreConsumedQuota: 100,
		FinalQuota:       250,
		UsageSummary: UsageSummary{
			PromptTokens:     3,
			CompletionTokens: 5,
			TotalTokens:      8,
		},
	}
	opts := SettlementOptions{Deduplicate: true}

	first, err := ApplySettlement(context.Background(), cmd, &opts)
	if err != nil {
		t.Fatalf("expected first settlement to succeed, got %v", err)
	}
	second, err := ApplySettlement(context.Background(), cmd, &opts)
	if err != nil {
		t.Fatalf("expected duplicate settlement to be skipped without error, got %v", err)
	}
	if !first.TruthApplied || second.TruthApplied || !second.Deduplicated || second.FingerprintConflict {
		t.Fatalf("expected second settlement to be deduplicated, got first=%+v second=%+v", first, second)
	}

	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup to succeed, got %v", err)
	}
	if user.Quota != 850 || user.RequestCount != 1 {
		t.Fatalf("expected detached finalize dedupe to avoid double projection, got quota=%d requests=%d", user.Quota, user.RequestCount)
	}
}

func TestApplySettlementReportsFingerprintConflictOnDeduplicatedMismatch(t *testing.T) {
	useSettlementTestDB(t)
	insertSettlementFixtures(t)

	server, err := fakeredis.Start()
	if err != nil {
		t.Fatalf("expected fake redis server to start, got %v", err)
	}
	defer server.Close()
	server.RegisterLuaScript(settlementAcquireGateScriptSource, func(keys, args []string) int64 {
		current, ok := server.GetRaw(keys[0])
		if !ok {
			server.SetRaw(keys[0], args[0])
			return 1
		}
		if current == args[0] {
			return 0
		}
		return -1
	})

	originalRedisEnabled := config.RedisEnabled
	originalRedisClient := commonredis.RDB
	originalBatch := config.BatchUpdateEnabled
	originalLogConsume := config.LogConsumeEnabled
	config.RedisEnabled = true
	config.BatchUpdateEnabled = false
	config.LogConsumeEnabled = false
	commonredis.RDB = server.Client()
	t.Cleanup(func() {
		config.RedisEnabled = originalRedisEnabled
		config.BatchUpdateEnabled = originalBatch
		config.LogConsumeEnabled = originalLogConsume
		commonredis.RDB = originalRedisClient
	})

	successCmd := SettlementCommand{
		Identity:         "task-1:finalize",
		RequestKind:      SettlementRequestKindAsyncTask,
		UserID:           1,
		TokenID:          1,
		ChannelID:        1,
		ModelName:        "gpt-5",
		PreConsumedQuota: 100,
		FinalQuota:       250,
		UsageSummary: UsageSummary{
			PromptTokens:     3,
			CompletionTokens: 5,
			TotalTokens:      8,
		},
	}
	failureCmd := successCmd
	failureCmd.FinalQuota = 0
	failureCmd.Fingerprint = ""

	first, err := ApplySettlement(context.Background(), successCmd, &SettlementOptions{Deduplicate: true})
	if err != nil {
		t.Fatalf("expected first settlement to succeed, got %v", err)
	}
	second, err := ApplySettlement(context.Background(), failureCmd, &SettlementOptions{Deduplicate: true})
	if err != nil {
		t.Fatalf("expected conflicting settlement to be deduplicated without error, got %v", err)
	}
	if !first.TruthApplied || !second.Deduplicated || !second.FingerprintConflict || second.TruthApplied {
		t.Fatalf("expected conflicting duplicate settlement to report fingerprint conflict, got first=%+v second=%+v", first, second)
	}
}

func TestApplySettlementFallsBackWhenGateBackendErrors(t *testing.T) {
	useSettlementTestDB(t)
	insertSettlementFixtures(t)

	server, err := fakeredis.Start()
	if err != nil {
		t.Fatalf("expected fake redis server to start, got %v", err)
	}
	defer server.Close()
	server.FailNext("EVALSHA", "ERR settlement gate unavailable")

	originalRedisEnabled := config.RedisEnabled
	originalRedisClient := commonredis.RDB
	originalBatch := config.BatchUpdateEnabled
	originalLogConsume := config.LogConsumeEnabled
	config.RedisEnabled = true
	config.BatchUpdateEnabled = false
	config.LogConsumeEnabled = false
	commonredis.RDB = server.Client()
	t.Cleanup(func() {
		config.RedisEnabled = originalRedisEnabled
		config.BatchUpdateEnabled = originalBatch
		config.LogConsumeEnabled = originalLogConsume
		commonredis.RDB = originalRedisClient
	})

	cmd := SettlementCommand{
		Identity:         "session-1:1:finalize",
		RequestKind:      SettlementRequestKindRealtimeTurn,
		UserID:           1,
		TokenID:          1,
		ChannelID:        1,
		ModelName:        "gpt-5",
		PreConsumedQuota: 100,
		FinalQuota:       250,
		UsageSummary: UsageSummary{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		},
	}

	result, err := ApplySettlement(context.Background(), cmd, &SettlementOptions{Deduplicate: true})
	if err != nil {
		t.Fatalf("expected settlement to degrade gracefully when redis gate fails, got %v", err)
	}
	if !result.TruthApplied || result.Deduplicated {
		t.Fatalf("expected truth write without dedupe after gate failure, got %+v", result)
	}

	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup to succeed, got %v", err)
	}
	if user.Quota != 850 {
		t.Fatalf("expected settlement fallback to charge user quota, got %d", user.Quota)
	}
}
