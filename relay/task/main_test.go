package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/model"
	providerbase "one-api/providers/base"
	"one-api/relay/relay_util"
	taskbase "one-api/relay/task/base"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type completedTaskAdaptor struct {
	task *model.Task
}

func (a *completedTaskAdaptor) Init() *taskbase.TaskError           { return nil }
func (a *completedTaskAdaptor) Relay() *taskbase.TaskError          { return nil }
func (a *completedTaskAdaptor) HandleError(err *taskbase.TaskError) {}
func (a *completedTaskAdaptor) ShouldRetry(c *gin.Context, err *taskbase.TaskError) bool {
	return false
}
func (a *completedTaskAdaptor) GetModelName() string                        { return "task-model" }
func (a *completedTaskAdaptor) GetTask() *model.Task                        { return a.task }
func (a *completedTaskAdaptor) SetProvider() *taskbase.TaskError            { return nil }
func (a *completedTaskAdaptor) GetProvider() providerbase.ProviderInterface { return nil }
func (a *completedTaskAdaptor) GinResponse()                                {}
func (a *completedTaskAdaptor) UpdateTaskStatus(ctx context.Context, taskChannelM map[int][]string, taskM map[string]*model.Task) error {
	return nil
}

type relayTaskSubmitAdaptor struct {
	task           *model.Task
	ctx            *gin.Context
	response       any
	relayErr       *taskbase.TaskError
	acceptedTaskID string
}

func (a *relayTaskSubmitAdaptor) Init() *taskbase.TaskError { return nil }

func (a *relayTaskSubmitAdaptor) Relay() *taskbase.TaskError {
	if a.relayErr != nil {
		return a.relayErr
	}
	a.task.TaskID = a.acceptedTaskID
	return nil
}

func (a *relayTaskSubmitAdaptor) HandleError(err *taskbase.TaskError) {
	if a.ctx == nil || err == nil {
		return
	}
	a.ctx.JSON(err.StatusCode, err)
}

func (a *relayTaskSubmitAdaptor) ShouldRetry(c *gin.Context, err *taskbase.TaskError) bool {
	return false
}

func (a *relayTaskSubmitAdaptor) GetModelName() string { return "task-model" }

func (a *relayTaskSubmitAdaptor) GetTask() *model.Task { return a.task }

func (a *relayTaskSubmitAdaptor) SetProvider() *taskbase.TaskError { return nil }

func (a *relayTaskSubmitAdaptor) GetProvider() providerbase.ProviderInterface { return nil }

func (a *relayTaskSubmitAdaptor) GinResponse() {
	if a.ctx == nil {
		return
	}
	a.ctx.JSON(http.StatusOK, a.response)
}

func (a *relayTaskSubmitAdaptor) UpdateTaskStatus(ctx context.Context, taskChannelM map[int][]string, taskM map[string]*model.Task) error {
	return nil
}

func useCompletedTaskTestDB(t *testing.T) {
	t.Helper()

	logger.Logger = zap.NewNop()

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.User{}, &model.Token{}, &model.Task{}); err != nil {
		t.Fatalf("expected completed task schema migration to succeed, got %v", err)
	}

	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
	})
}

func insertCompletedTaskFixtures(t *testing.T) {
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
}

func newCompletedTaskQuota(t *testing.T) *relay_util.Quota {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/tasks", nil)
	ctx.Request.RemoteAddr = "203.0.113.10:1234"
	ctx.Set("id", 1)
	ctx.Set("channel_id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("token_unlimited_quota", false)
	ctx.Set("token_name", "token-alpha")
	ctx.Set("group_ratio", 1.5)

	quota := relay_util.NewQuota(ctx, "task-model", 1000)
	quota.ForcePreConsume()
	if errWithCode := quota.PreQuotaConsumption(); errWithCode != nil {
		t.Fatalf("expected task reserve to succeed, got %+v", errWithCode)
	}
	return quota
}

func TestCompletedTaskUpdatesPreparedPlaceholderWithoutSecondInsert(t *testing.T) {
	useCompletedTaskTestDB(t)
	insertCompletedTaskFixtures(t)

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalRedisEnabled := config.RedisEnabled
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
	config.RedisEnabled = false
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.RedisEnabled = originalRedisEnabled
	})

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/tasks", nil)
	ctx.Request.RemoteAddr = "203.0.113.10:1234"
	ctx.Set("id", 1)
	ctx.Set("channel_id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("token_unlimited_quota", false)
	ctx.Set("token_name", "token-alpha")
	ctx.Set("group_ratio", 1.5)

	adaptor := &completedTaskAdaptor{
		task: &model.Task{
			Platform:  model.TaskPlatformSuno,
			UserId:    1,
			ChannelId: 1,
			TokenID:   1,
			Action:    "generate",
		},
	}

	quota, taskErr := prepareTaskAttemptQuota(ctx, adaptor)
	if taskErr != nil {
		t.Fatalf("expected prepared task attempt to succeed, got %+v", taskErr)
	}
	adaptor.task.TaskID = "accepted-task"

	if adaptor.task.ID == 0 {
		t.Fatal("expected prepared task attempt to allocate a local task id")
	}
	if err := CompletedTask(quota, adaptor); err != nil {
		t.Fatalf("expected completed task update to succeed, got %v", err)
	}

	var stored model.Task
	if err := model.DB.First(&stored, adaptor.task.ID).Error; err != nil {
		t.Fatalf("expected prepared task row to persist, got %v", err)
	}
	if stored.TaskID != "accepted-task" || stored.Quota != 1500 || stored.ChannelId != 1 {
		t.Fatalf("expected accepted task update to reuse prepared row, got task_id=%q quota=%d channel=%d", stored.TaskID, stored.Quota, stored.ChannelId)
	}

	var taskCount int64
	if err := model.DB.Model(&model.Task{}).Count(&taskCount).Error; err != nil {
		t.Fatalf("expected task count lookup to succeed, got %v", err)
	}
	if taskCount != 1 {
		t.Fatalf("expected completed task update not to create a second task row, got %d", taskCount)
	}
}

func TestCompletedTaskFallsBackToSnapshotTrackingHandleWhenAcceptedColumnWriteFails(t *testing.T) {
	useCompletedTaskTestDB(t)
	insertCompletedTaskFixtures(t)

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalRedisEnabled := config.RedisEnabled
	originalPersistAccepted := persistAcceptedTaskFunc
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
	config.RedisEnabled = false
	persistAcceptedTaskFunc = func(task *model.Task) error {
		return errors.New("forced accepted update failure")
	}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.RedisEnabled = originalRedisEnabled
		persistAcceptedTaskFunc = originalPersistAccepted
	})

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/tasks", nil)
	ctx.Request.RemoteAddr = "203.0.113.10:1234"
	ctx.Set("id", 1)
	ctx.Set("channel_id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("token_unlimited_quota", false)
	ctx.Set("token_name", "token-alpha")
	ctx.Set("group_ratio", 1.5)

	adaptor := &completedTaskAdaptor{
		task: &model.Task{
			Platform:  model.TaskPlatformSuno,
			UserId:    1,
			ChannelId: 1,
			TokenID:   1,
			Action:    "generate",
		},
	}

	quota, taskErr := prepareTaskAttemptQuota(ctx, adaptor)
	if taskErr != nil {
		t.Fatalf("expected prepared task attempt to succeed, got %+v", taskErr)
	}
	adaptor.task.TaskID = "accepted-task-fallback"

	if err := CompletedTask(quota, adaptor); err != nil {
		t.Fatalf("expected completed task fallback persistence to succeed, got %v", err)
	}

	var stored model.Task
	if err := model.DB.First(&stored, adaptor.task.ID).Error; err != nil {
		t.Fatalf("expected stored fallback task lookup to succeed, got %v", err)
	}
	if stored.TaskID != "" {
		t.Fatalf("expected fallback persistence to leave task_id column empty, got %q", stored.TaskID)
	}
	if taskbase.TaskTrackingHandle(&stored) != "accepted-task-fallback" {
		t.Fatalf("expected snapshot tracking handle to persist accepted task id, got %q", taskbase.TaskTrackingHandle(&stored))
	}
	if shouldSweepEmptyTaskID(&stored) {
		t.Fatal("expected accepted fallback handle task not to be swept as empty task_id")
	}
}

func TestRelayTaskSubmitAcceptedFallbackStillReturnsSuccessResponse(t *testing.T) {
	useCompletedTaskTestDB(t)
	insertCompletedTaskFixtures(t)

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalRedisEnabled := config.RedisEnabled
	originalGetTaskAdaptor := getTaskAdaptorFunc
	originalPersistAccepted := persistAcceptedTaskFunc
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
	config.RedisEnabled = false
	persistAcceptedTaskFunc = func(task *model.Task) error {
		return errors.New("forced accepted update failure")
	}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.RedisEnabled = originalRedisEnabled
		getTaskAdaptorFunc = originalGetTaskAdaptor
		persistAcceptedTaskFunc = originalPersistAccepted
	})

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/suno/submit/music", nil)
	ctx.Request.RemoteAddr = "203.0.113.10:1234"
	ctx.Set("id", 1)
	ctx.Set("channel_id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("token_unlimited_quota", false)
	ctx.Set("token_name", "token-alpha")
	ctx.Set("group_ratio", 1.5)

	adaptor := &relayTaskSubmitAdaptor{
		task: &model.Task{
			Platform:  model.TaskPlatformSuno,
			UserId:    1,
			ChannelId: 1,
			TokenID:   1,
			Action:    "generate",
		},
		response: map[string]any{
			"task_id": "accepted-submit-task",
		},
		acceptedTaskID: "accepted-submit-task",
	}
	getTaskAdaptorFunc = func(relayType int, c *gin.Context) (taskbase.TaskInterface, error) {
		adaptor.ctx = c
		return adaptor, nil
	}

	RelayTaskSubmit(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected submit to keep provider success response, got status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("expected success response body to parse, got %v", err)
	}
	if payload["task_id"] != "accepted-submit-task" {
		t.Fatalf("expected provider task id to be preserved in success response, got %#v", payload)
	}

	var stored model.Task
	if err := model.DB.First(&stored).Error; err != nil {
		t.Fatalf("expected fallback task row to persist, got %v", err)
	}
	if stored.TaskID != "" {
		t.Fatalf("expected fallback persistence to leave task_id column empty, got %q", stored.TaskID)
	}
	if taskbase.TaskTrackingHandle(&stored) != "accepted-submit-task" {
		t.Fatalf("expected fallback tracking handle to remain available for sweeper, got %q", taskbase.TaskTrackingHandle(&stored))
	}
}

func TestRelayTaskSubmitAcceptedPersistenceFailureReturnsInternalErrorWithoutUndo(t *testing.T) {
	useCompletedTaskTestDB(t)
	insertCompletedTaskFixtures(t)

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalRedisEnabled := config.RedisEnabled
	originalGetTaskAdaptor := getTaskAdaptorFunc
	originalPersistAccepted := persistAcceptedTaskFunc
	originalPersistAcceptedFallback := persistAcceptedTaskFallbackFunc
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
	config.RedisEnabled = false
	persistAcceptedTaskFunc = func(task *model.Task) error {
		return errors.New("forced accepted update failure")
	}
	persistAcceptedTaskFallbackFunc = func(task *model.Task) error {
		return errors.New("forced accepted fallback failure")
	}
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.RedisEnabled = originalRedisEnabled
		getTaskAdaptorFunc = originalGetTaskAdaptor
		persistAcceptedTaskFunc = originalPersistAccepted
		persistAcceptedTaskFallbackFunc = originalPersistAcceptedFallback
	})

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/suno/submit/music", nil)
	ctx.Request.RemoteAddr = "203.0.113.10:1234"
	ctx.Set("id", 1)
	ctx.Set("channel_id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("token_unlimited_quota", false)
	ctx.Set("token_name", "token-alpha")
	ctx.Set("group_ratio", 1.5)

	adaptor := &relayTaskSubmitAdaptor{
		task: &model.Task{
			Platform:  model.TaskPlatformSuno,
			UserId:    1,
			ChannelId: 1,
			TokenID:   1,
			Action:    "generate",
		},
		response: map[string]any{
			"task_id": "accepted-submit-task",
		},
		acceptedTaskID: "accepted-submit-task",
	}
	getTaskAdaptorFunc = func(relayType int, c *gin.Context) (taskbase.TaskInterface, error) {
		adaptor.ctx = c
		return adaptor, nil
	}

	RelayTaskSubmit(ctx)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected accepted persistence failure to surface as 500, got status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	var response taskbase.TaskError
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("expected error response body to parse, got %v", err)
	}
	if response.Code != "task_persist_failed" {
		t.Fatalf("expected task_persist_failed code, got %+v", response)
	}

	var taskCount int64
	if err := model.DB.Model(&model.Task{}).Count(&taskCount).Error; err != nil {
		t.Fatalf("expected task count lookup to succeed, got %v", err)
	}
	if taskCount != 1 {
		t.Fatalf("expected accepted persistence failure to leave placeholder task intact, got %d rows", taskCount)
	}

	var storedTask model.Task
	if err := model.DB.First(&storedTask).Error; err != nil {
		t.Fatalf("expected stored placeholder task lookup to succeed, got %v", err)
	}
	if storedTask.Quota != 1500 || storedTask.TaskID != "" {
		t.Fatalf("expected unresolved placeholder task to keep reserved quota and empty task_id, got quota=%d task_id=%q", storedTask.Quota, storedTask.TaskID)
	}

	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup after submit failure to succeed, got %v", err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup after submit failure to succeed, got %v", err)
	}
	if user.Quota != 3500 || token.RemainQuota != 3500 || token.UsedQuota != 1500 {
		t.Fatalf("expected accepted persistence failure not to undo reserve, got user=%d remain=%d used=%d", user.Quota, token.RemainQuota, token.UsedQuota)
	}
}

func TestPrepareTaskAttemptQuotaRebuildsReserveForRetryContext(t *testing.T) {
	useCompletedTaskTestDB(t)
	insertCompletedTaskFixtures(t)

	originalPricing := model.PricingInstance
	originalBatch := config.BatchUpdateEnabled
	originalRedisEnabled := config.RedisEnabled
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
	config.RedisEnabled = false
	t.Cleanup(func() {
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled = originalBatch
		config.RedisEnabled = originalRedisEnabled
	})

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/tasks", nil)
	ctx.Request.RemoteAddr = "203.0.113.10:1234"
	ctx.Set("id", 1)
	ctx.Set("channel_id", 1)
	ctx.Set("token_id", 1)
	ctx.Set("token_unlimited_quota", false)
	ctx.Set("token_name", "token-alpha")
	ctx.Set("group_ratio", 1.0)

	adaptor := &completedTaskAdaptor{
		task: &model.Task{
			Platform:  model.TaskPlatformSuno,
			UserId:    1,
			ChannelId: 1,
			TokenID:   1,
			Action:    "generate",
		},
	}

	firstAttemptQuota, taskErr := prepareTaskAttemptQuota(ctx, adaptor)
	if taskErr != nil {
		t.Fatalf("expected first task attempt reserve to succeed, got %+v", taskErr)
	}

	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup after first reserve to succeed, got %v", err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup after first reserve to succeed, got %v", err)
	}
	if user.Quota != 4000 || token.RemainQuota != 4000 || token.UsedQuota != 1000 {
		t.Fatalf("expected first attempt reserve to debit 1000 quota, got user=%d remain=%d used=%d", user.Quota, token.RemainQuota, token.UsedQuota)
	}
	firstAttemptPreConsumed := firstAttemptQuota.BuildSettlementEnvelope(nil, false, "", "", false).Command.PreConsumedQuota
	if err := model.PostConsumeTokenQuotaWithInfo(1, 1, false, -firstAttemptPreConsumed); err != nil {
		t.Fatalf("expected synchronous retry refund to succeed, got %v", err)
	}
	discardPreparedTaskAttempt(adaptor.task, "retry")

	ctx.Set("channel_id", 2)
	ctx.Set("group_ratio", 2.0)
	adaptor.task.ChannelId = 2

	secondAttemptQuota, taskErr := prepareTaskAttemptQuota(ctx, adaptor)
	if taskErr != nil {
		t.Fatalf("expected second task attempt reserve to succeed, got %+v", taskErr)
	}
	adaptor.task.TaskID = "accepted-task"
	if err := CompletedTask(secondAttemptQuota, adaptor); err != nil {
		t.Fatalf("expected accepted retry task to persist, got %v", err)
	}

	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatalf("expected user lookup after retry reserve to succeed, got %v", err)
	}
	if user.Quota != 3000 {
		t.Fatalf("expected only the successful retry reserve to remain, got quota=%d", user.Quota)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatalf("expected token lookup after retry reserve to succeed, got %v", err)
	}
	if token.RemainQuota != 3000 || token.UsedQuota != 2000 {
		t.Fatalf("expected successful retry reserve to be held on token, got remain=%d used=%d", token.RemainQuota, token.UsedQuota)
	}

	var task model.Task
	if err := model.DB.First(&task).Error; err != nil {
		t.Fatalf("expected accepted retry task to persist, got %v", err)
	}
	if task.Quota != 2000 || task.ChannelId != 2 {
		t.Fatalf("expected accepted task to store retry attempt channel/quota, got channel=%d quota=%d", task.ChannelId, task.Quota)
	}

	var snapshot struct {
		Envelope struct {
			Command struct {
				ChannelID        int `json:"channel_id"`
				PreConsumedQuota int `json:"pre_consumed_quota"`
				FinalQuota       int `json:"final_quota"`
			} `json:"command"`
		} `json:"envelope"`
	}
	if err := json.Unmarshal(task.Properties, &snapshot); err != nil {
		t.Fatalf("expected stored task snapshot parse to succeed, got %v", err)
	}
	if snapshot.Envelope.Command.ChannelID != 2 || snapshot.Envelope.Command.PreConsumedQuota != 2000 || snapshot.Envelope.Command.FinalQuota != 2000 {
		t.Fatalf("expected stored snapshot to freeze successful retry pricing context, got %+v", snapshot.Envelope.Command)
	}
}

func TestShouldSweepEmptyTaskIDOnlyForAcceptedTasksWithoutTrackingHandle(t *testing.T) {
	useCompletedTaskTestDB(t)
	insertCompletedTaskFixtures(t)

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

	tracked := &model.Task{
		Platform:   model.TaskPlatformSuno,
		UserId:     1,
		ChannelId:  1,
		TokenID:    1,
		Status:     model.TaskStatusSubmitted,
		SubmitTime: time.Now().Add(-2 * emptyTaskIDSweepDelay).Unix(),
	}
	if err := tracked.Insert(); err != nil {
		t.Fatalf("expected tracked task insert to succeed, got %v", err)
	}
	properties, _, err := taskbase.BuildTaskSettlementSnapshotProperties(newCompletedTaskQuota(t), tracked)
	if err != nil {
		t.Fatalf("expected tracked snapshot build to succeed, got %v", err)
	}
	tracked.Properties = properties
	tracked.Properties, err = taskbase.MarkTaskProviderAccepted(tracked, "remote-task-1")
	if err != nil {
		t.Fatalf("expected tracked task accepted marker to succeed, got %v", err)
	}
	if shouldSweepEmptyTaskID(tracked) {
		t.Fatal("expected accepted task with snapshot tracking handle not to be swept")
	}

	untracked := &model.Task{
		Platform:   model.TaskPlatformSuno,
		UserId:     1,
		ChannelId:  1,
		TokenID:    1,
		Status:     model.TaskStatusSubmitted,
		SubmitTime: time.Now().Add(-2 * emptyTaskIDSweepDelay).Unix(),
	}
	if err := untracked.Insert(); err != nil {
		t.Fatalf("expected untracked task insert to succeed, got %v", err)
	}
	properties, _, err = taskbase.BuildTaskSettlementSnapshotProperties(newCompletedTaskQuota(t), untracked)
	if err != nil {
		t.Fatalf("expected untracked snapshot build to succeed, got %v", err)
	}
	untracked.Properties = properties
	untracked.Properties, err = taskbase.MarkTaskProviderAccepted(untracked, "")
	if err != nil {
		t.Fatalf("expected untracked task accepted marker to succeed, got %v", err)
	}
	if !shouldSweepEmptyTaskID(untracked) {
		t.Fatal("expected accepted task without tracking handle to be swept")
	}
}
