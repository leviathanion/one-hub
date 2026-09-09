package base

import (
	"context"
	"testing"

	"one-api/common/config"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"

	"github.com/google/uuid"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func taskBillingFixture(t *testing.T, state model.TaskProviderState) *model.Task {
	t.Helper()
	originalDB := model.DB
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserGroup{}, &model.Channel{}, &model.Task{}); err != nil {
		t.Fatal(err)
	}
	model.DB = db
	t.Cleanup(func() { model.DB = originalDB })
	if err := db.Create(&model.User{Id: 1, Username: "u", Password: "password123", AccessToken: "access", Quota: 1000, Status: config.UserStatusEnabled}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{Id: 1, UserId: 1, Key: "token", Status: config.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Channel{Id: 1, Name: "channel", Key: "key", Status: config.ChannelStatusEnabled}).Error; err != nil {
		t.Fatal(err)
	}
	task := &model.Task{Platform: model.TaskPlatformKling, UserId: 1, TokenID: 1, ChannelId: 1, Status: model.TaskStatusSubmitted, ReservedQuota: 100, ProviderNamespace: "task-platform:kling", ProviderTaskScopeIncarnation: "provider-wide", RequestFingerprint: "fixture"}
	if result, err := model.CreateTaskBillingOwner(context.Background(), task); err != nil || result.Outcome != model.BillingBalanceCommitted {
		t.Fatalf("create result=%+v err=%v", result, err)
	}
	if state == model.TaskProviderStatePrepared {
		return task
	}
	if _, err := model.ClaimTaskSubmission(context.Background(), task, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if state == model.TaskProviderStateSubmitStarted {
		return task
	}
	if _, err := model.AcceptTaskSubmission(context.Background(), task, "provider-1"); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestTaskTerminalWithoutProviderUsageCancelsReservation(t *testing.T) {
	task := taskBillingFixture(t, model.TaskProviderStateAccepted)
	task.Status = model.TaskStatusSuccess
	result, err := FinalizeTaskSettlement(context.Background(), task)
	if err != nil || !result.Handled || result.PersistTask {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	assertTaskBillingState(t, task.ID, model.TaskProviderStateClosed, model.TaskStatusSuccess, 0, 1000)
	if handle := TaskTrackingHandle(task); handle != "provider-1" {
		t.Fatalf("closed task lost provider handle: %q", handle)
	}
}

func TestPreparedLocalFailureCancelsReservation(t *testing.T) {
	task := taskBillingFixture(t, model.TaskProviderStatePrepared)
	if err := FailTaskWithSettlement(context.Background(), task, "claim failed"); err != nil {
		t.Fatal(err)
	}
	assertTaskBillingState(t, task.ID, model.TaskProviderStateClosed, model.TaskStatusLocalFailure, 0, 1000)
}

func TestAmbiguousSubmissionCancelsReservationWithoutRetry(t *testing.T) {
	task := taskBillingFixture(t, model.TaskProviderStateSubmitStarted)
	if err := FailTaskWithSettlement(context.Background(), task, "ambiguous"); err != nil {
		t.Fatal(err)
	}
	assertTaskBillingState(t, task.ID, model.TaskProviderStateClosed, model.TaskStatusUnknown, 0, 1000)
}

func assertTaskBillingState(t *testing.T, id int64, state model.TaskProviderState, status model.TaskStatus, charged int64, userQuota int) {
	t.Helper()
	var task model.Task
	if err := model.DB.First(&task, id).Error; err != nil {
		t.Fatal(err)
	}
	if task.ProviderState != state || task.Status != status {
		t.Fatalf("task state=%s status=%s, want state=%s status=%s", task.ProviderState, task.Status, state, status)
	}
	if task.ChargedQuota == nil || *task.ChargedQuota != charged {
		t.Fatalf("charged quota=%v, want %d", task.ChargedQuota, charged)
	}
	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != userQuota {
		t.Fatalf("user quota=%d, want %d", user.Quota, userQuota)
	}
}
