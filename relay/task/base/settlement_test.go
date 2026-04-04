package base

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
	"one-api/common/logger"
	commonredis "one-api/common/redis"
	"one-api/internal/testutil/fakeredis"
	"one-api/model"
	"one-api/relay/relay_util"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useTaskSettlementTestDB(t *testing.T) {
	t.Helper()

	logger.Logger = zap.NewNop()

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Log{}, &model.Task{}); err != nil {
		t.Fatalf("expected task settlement schema migration to succeed, got %v", err)
	}

	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
	})
}

func insertTaskSettlementFixtures(t *testing.T) {
	t.Helper()

	if err := model.DB.Create(&model.User{
		Id:          1,
		Username:    "alice",
		Password:    "password123",
		AccessToken: "access-token-1",
		Quota:       5000,
		Group:       "default",
		Status:      config.UserStatusEnabled,
		Role:        config.RoleCommonUser,
		DisplayName: "Alice",
		CreatedTime: 1,
	}).Error; err != nil {
		t.Fatalf("expected user fixture to persist, got %v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id:          1,
		UserId:      1,
		Key:         "token-key-1",
		Name:        "token-alpha",
		RemainQuota: 5000,
		Group:       "default",
	}).Error; err != nil {
		t.Fatalf("expected token fixture to persist, got %v", err)
	}
	if err := model.DB.Create(&model.Channel{
		Id:     1,
		Name:   "channel-alpha",
		Key:    "sk-test",
		Group:  "default",
		Models: "task-model",
	}).Error; err != nil {
		t.Fatalf("expected channel fixture to persist, got %v", err)
	}
}

func newTaskSettlementQuota(t *testing.T) *relay_util.Quota {
	return newTaskSettlementQuotaWithChannel(t, 1)
}

func newTaskSettlementQuotaWithChannel(t *testing.T, channelID int) *relay_util.Quota {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/tasks", nil)
	ctx.Request.RemoteAddr = "203.0.113.10:1234"
	ctx.Set("id", 1)
	ctx.Set("channel_id", channelID)
	ctx.Set("token_id", 1)
	ctx.Set("token_unlimited_quota", false)
	ctx.Set("token_name", "token-alpha")
	ctx.Set("group_ratio", 1.5)

	quota := relay_util.NewQuota(ctx, "task-model", 1000)
	if errWithCode := quota.PreQuotaConsumption(); errWithCode != nil {
		t.Fatalf("expected task reserve to succeed, got %+v", errWithCode)
	}
	return quota
}

func TestTaskSettlementSnapshotUsesStoredFinalQuotaOnSuccess(t *testing.T) {
	useTaskSettlementTestDB(t)
	insertTaskSettlementFixtures(t)

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalLogConsume := config.LogConsumeEnabled
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"task-model": {
				Model: "task-model",
				Type:  model.TimesPriceType,
				Input: 1,
			},
		},
	}
	config.BatchUpdateEnabled = false
	config.LogConsumeEnabled = true
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.LogConsumeEnabled = originalLogConsume
	})

	quota := newTaskSettlementQuota(t)
	task := &model.Task{
		Platform:  model.TaskPlatformSuno,
		UserId:    1,
		ChannelId: 1,
		TokenID:   1,
		TaskID:    "task-success-1",
	}

	properties, finalQuota, err := BuildTaskSettlementSnapshotProperties(quota, task)
	if err != nil {
		t.Fatalf("expected task settlement snapshot build to succeed, got %v", err)
	}
	if finalQuota != 1500 {
		t.Fatalf("expected stored final quota 1500, got %d", finalQuota)
	}
	task.Properties = properties
	task.Quota = finalQuota

	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"task-model": {
				Model: "task-model",
				Type:  model.TimesPriceType,
				Input: 9,
			},
		},
	}

	result, err := FinalizeTaskSettlement(context.Background(), task, true)
	if err != nil || !result.Handled || !result.PersistTask {
		t.Fatalf("expected task settlement finalize to succeed, got result=%+v err=%v", result, err)
	}
	if task.Quota != 1500 {
		t.Fatalf("expected success finalize to keep stored final quota, got %d", task.Quota)
	}

	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup to succeed, got %v", err)
	}
	if user.Quota != 3500 || user.UsedQuota != 1500 || user.RequestCount != 1 {
		t.Fatalf("expected success finalize to project stored final quota once, got quota=%d used=%d requests=%d", user.Quota, user.UsedQuota, user.RequestCount)
	}

	var channel model.Channel
	if err := model.DB.First(&channel, 1).Error; err != nil {
		t.Fatalf("expected channel lookup to succeed, got %v", err)
	}
	if channel.UsedQuota != 1500 {
		t.Fatalf("expected success finalize to project channel used quota 1500, got %d", channel.UsedQuota)
	}

	var log model.Log
	if err := model.DB.Where("user_id = ?", 1).First(&log).Error; err != nil {
		t.Fatalf("expected task consume log lookup to succeed, got %v", err)
	}
	if log.Quota != 1500 || log.PromptTokens != 0 || log.CompletionTokens != 0 {
		t.Fatalf("expected task consume log to keep fixed-price quota without synthetic tokens, got %+v", log)
	}

	result, err = FinalizeTaskSettlement(context.Background(), task, true)
	if err != nil || !result.Handled || result.PersistTask {
		t.Fatalf("expected repeated finalize to be a no-op success, got result=%+v err=%v", result, err)
	}
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected second user lookup to succeed, got %v", err)
	}
	if user.RequestCount != 1 {
		t.Fatalf("expected repeated finalize not to project twice, got request_count=%d", user.RequestCount)
	}
}

func TestTaskSettlementSnapshotRollsBackReserveOnFailure(t *testing.T) {
	useTaskSettlementTestDB(t)
	insertTaskSettlementFixtures(t)

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalLogConsume := config.LogConsumeEnabled
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"task-model": {
				Model: "task-model",
				Type:  model.TimesPriceType,
				Input: 1,
			},
		},
	}
	config.BatchUpdateEnabled = false
	config.LogConsumeEnabled = true
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.LogConsumeEnabled = originalLogConsume
	})

	quota := newTaskSettlementQuota(t)
	task := &model.Task{
		Platform:  model.TaskPlatformSuno,
		UserId:    1,
		ChannelId: 1,
		TokenID:   1,
		TaskID:    "task-failure-1",
	}

	properties, finalQuota, err := BuildTaskSettlementSnapshotProperties(quota, task)
	if err != nil {
		t.Fatalf("expected task settlement snapshot build to succeed, got %v", err)
	}
	task.Properties = properties
	task.Quota = finalQuota

	result, err := FinalizeTaskSettlement(context.Background(), task, false)
	if err != nil || !result.Handled || !result.PersistTask {
		t.Fatalf("expected failure finalize to succeed, got result=%+v err=%v", result, err)
	}
	if task.Quota != 0 {
		t.Fatalf("expected failure finalize to zero out task quota, got %d", task.Quota)
	}

	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup to succeed, got %v", err)
	}
	if user.Quota != 5000 || user.UsedQuota != 0 || user.RequestCount != 1 {
		t.Fatalf("expected failure finalize to refund reserve and keep single request projection, got quota=%d used=%d requests=%d", user.Quota, user.UsedQuota, user.RequestCount)
	}

	var token model.Token
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup to succeed, got %v", err)
	}
	if token.RemainQuota != 5000 || token.UsedQuota != 0 {
		t.Fatalf("expected failure finalize to restore token reserve, got remain=%d used=%d", token.RemainQuota, token.UsedQuota)
	}
}

func TestTaskSettlementSnapshotRequiresStableLocalIdentity(t *testing.T) {
	useTaskSettlementTestDB(t)
	insertTaskSettlementFixtures(t)

	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"task-model": {
				Model: "task-model",
				Type:  model.TimesPriceType,
				Input: 1,
			},
		},
	}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
	})

	task := &model.Task{
		Platform:  model.TaskPlatformSuno,
		UserId:    1,
		ChannelId: 1,
		TokenID:   1,
	}
	if _, _, err := BuildTaskSettlementSnapshotProperties(newTaskSettlementQuotaWithChannel(t, 1), task); err == nil {
		t.Fatal("expected task settlement snapshot build without local id to fail")
	}
	if err := task.Insert(); err != nil {
		t.Fatalf("expected task insert to succeed, got %v", err)
	}

	properties, _, err := BuildTaskSettlementSnapshotProperties(newTaskSettlementQuotaWithChannel(t, 1), task)
	if err != nil {
		t.Fatalf("expected task settlement snapshot build with local id to succeed, got %v", err)
	}
	snapshot, err := parseTaskSettlementSnapshot(&model.Task{Properties: properties})
	if err != nil || snapshot == nil {
		t.Fatalf("expected task settlement snapshot parse to succeed, got snapshot=%+v err=%v", snapshot, err)
	}
	if got := snapshot.Envelope.Command.Identity; got != fmt.Sprintf("task:%d:finalize", task.ID) {
		t.Fatalf("expected task settlement identity to use local task id, got %q", got)
	}
}

func TestFailTaskWithSettlementSkipsLocalRewriteAfterDeduplicatedMismatch(t *testing.T) {
	useTaskSettlementTestDB(t)
	insertTaskSettlementFixtures(t)

	server, err := fakeredis.Start()
	if err != nil {
		t.Fatalf("expected fake redis server to start, got %v", err)
	}
	defer server.Close()
	server.RegisterLuaScript(`
local key = KEYS[1]
local fingerprint = ARGV[1]
local ttl_ms = tonumber(ARGV[2])

local current = redis.call('GET', key)
if not current then
	redis.call('SET', key, fingerprint, 'PX', ttl_ms)
	return 1
end
if current == fingerprint then
	return 0
end
return -1
`, func(keys, args []string) int64 {
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

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalLogConsume := config.LogConsumeEnabled
	originalRedisEnabled := config.RedisEnabled
	originalRedisClient := commonredis.RDB
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"task-model": {
				Model: "task-model",
				Type:  model.TimesPriceType,
				Input: 1,
			},
		},
	}
	config.BatchUpdateEnabled = false
	config.LogConsumeEnabled = false
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.LogConsumeEnabled = originalLogConsume
		config.RedisEnabled = originalRedisEnabled
		commonredis.RDB = originalRedisClient
	})

	quota := newTaskSettlementQuota(t)
	config.RedisEnabled = true
	commonredis.RDB = server.Client()
	originalTask := &model.Task{
		Platform:   model.TaskPlatformSuno,
		UserId:     1,
		ChannelId:  1,
		TokenID:    1,
		TaskID:     "task-dedupe-1",
		Status:     model.TaskStatusNotStart,
		SubmitTime: 1,
	}

	properties, finalQuota, err := BuildTaskSettlementSnapshotProperties(quota, originalTask)
	if err != nil {
		t.Fatalf("expected task settlement snapshot build to succeed, got %v", err)
	}
	originalTask.Properties = properties
	originalTask.Quota = finalQuota
	if err := originalTask.Insert(); err != nil {
		t.Fatalf("expected task insert to succeed, got %v", err)
	}

	winner := *originalTask
	result, err := FinalizeTaskSettlement(context.Background(), &winner, true)
	if err != nil || !result.PersistTask {
		t.Fatalf("expected winner finalize to persist, got result=%+v err=%v", result, err)
	}
	winner.Status = model.TaskStatusSuccess
	winner.Progress = 100
	if err := winner.Update(); err != nil {
		t.Fatalf("expected winner update to persist, got %v", err)
	}

	loser := *originalTask
	if err := FailTaskWithSettlement(context.Background(), &loser, "stale failure"); err != nil {
		t.Fatalf("expected stale loser finalize to skip rewrite, got %v", err)
	}

	var stored model.Task
	if err := model.DB.First(&stored, originalTask.ID).Error; err != nil {
		t.Fatalf("expected stored task lookup to succeed, got %v", err)
	}
	if stored.Status != model.TaskStatusSuccess || stored.Progress != 100 {
		t.Fatalf("expected stale loser not to overwrite winner task state, got status=%s progress=%d", stored.Status, stored.Progress)
	}

	snapshot, err := parseTaskSettlementSnapshot(&stored)
	if err != nil {
		t.Fatalf("expected stored snapshot parse to succeed, got %v", err)
	}
	if snapshot == nil || snapshot.Status != taskSettlementStatusCommitted {
		t.Fatalf("expected stale loser not to overwrite committed snapshot, got %+v", snapshot)
	}
	if stored.Quota != finalQuota {
		t.Fatalf("expected stale loser not to overwrite stored quota, got %d", stored.Quota)
	}
}

func TestFinalizeTaskSettlementPersistsSameFingerprintDuplicateToHealTaskRow(t *testing.T) {
	useTaskSettlementTestDB(t)
	insertTaskSettlementFixtures(t)

	server, err := fakeredis.Start()
	if err != nil {
		t.Fatalf("expected fake redis server to start, got %v", err)
	}
	defer server.Close()
	server.RegisterLuaScript(`
local key = KEYS[1]
local fingerprint = ARGV[1]
local ttl_ms = tonumber(ARGV[2])

local current = redis.call('GET', key)
if not current then
	redis.call('SET', key, fingerprint, 'PX', ttl_ms)
	return 1
end
if current == fingerprint then
	return 0
end
return -1
`, func(keys, args []string) int64 {
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

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalLogConsume := config.LogConsumeEnabled
	originalRedisEnabled := config.RedisEnabled
	originalRedisClient := commonredis.RDB
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"task-model": {
				Model: "task-model",
				Type:  model.TimesPriceType,
				Input: 1,
			},
		},
	}
	config.BatchUpdateEnabled = false
	config.LogConsumeEnabled = false
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.LogConsumeEnabled = originalLogConsume
		config.RedisEnabled = originalRedisEnabled
		commonredis.RDB = originalRedisClient
	})

	quota := newTaskSettlementQuota(t)
	config.RedisEnabled = true
	commonredis.RDB = server.Client()
	originalTask := &model.Task{
		Platform:   model.TaskPlatformSuno,
		UserId:     1,
		ChannelId:  1,
		TokenID:    1,
		TaskID:     "task-heal-1",
		Status:     model.TaskStatusNotStart,
		SubmitTime: 1,
	}

	properties, finalQuota, err := BuildTaskSettlementSnapshotProperties(quota, originalTask)
	if err != nil {
		t.Fatalf("expected task settlement snapshot build to succeed, got %v", err)
	}
	originalTask.Properties = properties
	originalTask.Quota = finalQuota
	if err := originalTask.Insert(); err != nil {
		t.Fatalf("expected task insert to succeed, got %v", err)
	}

	winner := *originalTask
	result, err := FinalizeTaskSettlement(context.Background(), &winner, true)
	if err != nil || !result.Handled || !result.PersistTask {
		t.Fatalf("expected initial finalize to succeed, got result=%+v err=%v", result, err)
	}

	healer := *originalTask
	result, err = FinalizeTaskSettlement(context.Background(), &healer, true)
	if err != nil || !result.Handled || !result.PersistTask || !result.Deduplicated {
		t.Fatalf("expected duplicate same-fingerprint finalize to heal task row, got result=%+v err=%v", result, err)
	}
	healer.Status = model.TaskStatusSuccess
	healer.Progress = 100
	if err := healer.Update(); err != nil {
		t.Fatalf("expected healer update to persist, got %v", err)
	}

	var stored model.Task
	if err := model.DB.First(&stored, originalTask.ID).Error; err != nil {
		t.Fatalf("expected stored task lookup to succeed, got %v", err)
	}
	snapshot, err := parseTaskSettlementSnapshot(&stored)
	if err != nil {
		t.Fatalf("expected stored snapshot parse to succeed, got %v", err)
	}
	if snapshot == nil || snapshot.Status != taskSettlementStatusCommitted {
		t.Fatalf("expected healer to persist committed snapshot, got %+v", snapshot)
	}
	if stored.Status != model.TaskStatusSuccess || stored.Progress != 100 || stored.Quota != finalQuota {
		t.Fatalf("expected healer to persist committed task row, got status=%s progress=%d quota=%d", stored.Status, stored.Progress, stored.Quota)
	}
}

func TestFailTaskWithSettlementLegacyFallbackRestoresTokenTruth(t *testing.T) {
	useTaskSettlementTestDB(t)
	insertTaskSettlementFixtures(t)

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	model.PricingInstance = &model.Pricing{
		Prices: map[string]*model.Price{
			"task-model": {
				Model: "task-model",
				Type:  model.TimesPriceType,
				Input: 1,
			},
		},
	}
	config.BatchUpdateEnabled = false
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
	})

	quota := newTaskSettlementQuota(t)
	finalQuota := quota.BuildSettlementEnvelope(nil, false, "", "", false).Command.PreConsumedQuota

	legacyTask := &model.Task{
		Platform:   model.TaskPlatformSuno,
		UserId:     1,
		ChannelId:  1,
		TokenID:    1,
		TaskID:     "legacy-fail-1",
		Quota:      finalQuota,
		Status:     model.TaskStatusSubmitted,
		SubmitTime: 1,
	}
	if err := legacyTask.Insert(); err != nil {
		t.Fatalf("expected legacy task insert to succeed, got %v", err)
	}

	if err := FailTaskWithSettlement(context.Background(), legacyTask, "legacy failure"); err != nil {
		t.Fatalf("expected legacy task failure settlement to succeed, got %v", err)
	}

	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup to succeed, got %v", err)
	}
	if user.Quota != 5000 {
		t.Fatalf("expected legacy failure fallback to restore user quota, got %d", user.Quota)
	}

	var token model.Token
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup to succeed, got %v", err)
	}
	if token.RemainQuota != 5000 || token.UsedQuota != 0 {
		t.Fatalf("expected legacy failure fallback to restore token truth, got remain=%d used=%d", token.RemainQuota, token.UsedQuota)
	}

	var stored model.Task
	if err := model.DB.First(&stored, legacyTask.ID).Error; err != nil {
		t.Fatalf("expected stored legacy task lookup to succeed, got %v", err)
	}
	if stored.Status != model.TaskStatusFailure || stored.Progress != 100 {
		t.Fatalf("expected legacy failure task row to persist terminal state, got status=%s progress=%d", stored.Status, stored.Progress)
	}
}
