package model

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"one-api/common/config"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func newAsyncReplayTask(t *testing.T, platform string, change func(*Task)) *Task {
	t.Helper()
	task := &Task{Platform: platform, UserId: 1, TokenID: 1, ChannelId: 1, ReservedQuota: 10, ProviderNamespace: "openai", ProviderTaskScopeIncarnation: "provider-wide", RequestFingerprint: "same-request", Data: datatypes.JSON(`{}`)}
	if change != nil {
		change(task)
	}
	if result, err := CreateTaskBillingOwner(context.Background(), task); err != nil || result.Outcome != BillingBalanceCommitted {
		t.Fatalf("reserve: %+v %v", result, err)
	}
	claimDurableTaskOwner(t, task)
	return task
}

func acceptAsyncReplayTask(t *testing.T, task *Task, id string) (TaskMutationResult, error) {
	t.Helper()
	if task.Platform == TaskPlatformOpenAIBatch {
		slots := batchTestSlots(t, task)
		return AcceptOpenAIBatchSubmission(context.Background(), task, id, slots[0].ID, []uint64{slots[1].ID, slots[2].ID})
	}
	owner, err := NewBackgroundResponseOwner(id, task.UserId, task.TokenID, task.ChannelId, time.Now(), false, task.ProviderNamespace, task.ProviderTaskScopeIncarnation)
	if err != nil {
		t.Fatal(err)
	}
	return AcceptBackgroundResponseSubmission(context.Background(), task, id, owner)
}

func TestAsyncTaskAcceptanceReplayPreservesOwnerAndReleasesReservation(t *testing.T) {
	for _, platform := range []string{TaskPlatformOpenAIBatch, TaskPlatformOpenAIResponsesBackground} {
		for _, state := range []string{"accepted", "closed"} {
			t.Run(platform+"/"+state, func(t *testing.T) {
				db := batchTestDB(t)
				ctx := context.Background()
				existing := newAsyncReplayTask(t, platform, nil)
				const id = " async-replayed "
				if result, err := acceptAsyncReplayTask(t, existing, id); err != nil || result.Outcome != TaskMutationApplied {
					t.Fatalf("first accept: %+v %v", result, err)
				}
				wantQuota, wantUsed := 990, 0
				if state == "closed" {
					existing.Status = TaskStatusSuccess
					if result, err := FinalizeTaskBillingOwner(ctx, existing, 25, "confirm"); err != nil || result.Outcome != BillingBalanceCommitted {
						t.Fatalf("settle original: %+v %v", result, err)
					}
					wantQuota, wantUsed = 975, 25
				}
				before, err := loadTaskOwnerForRecovery(ctx, existing.OwnerID)
				if err != nil {
					t.Fatal(err)
				}
				var resources []ResourceOwner
				var responses []ResponseOwner
				if err := db.Find(&resources).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Find(&responses).Error; err != nil {
					t.Fatal(err)
				}
				// 同一用户换 token 重试仍保留原计费 token，重复预留退回新 token。
				if err := db.Model(&Token{}).Where("id = 2").Update("user_id", 1).Error; err != nil {
					t.Fatal(err)
				}
				for attempt := 0; attempt < 2; attempt++ {
					replay := newAsyncReplayTask(t, platform, func(task *Task) { task.TokenID = 2 })
					provisionalID := replay.OwnerID
					if result, err := acceptAsyncReplayTask(t, replay, id); err != nil || result.Outcome != TaskMutationApplied || !reflect.DeepEqual(replay, before) {
						t.Fatalf("replay replaced original: task=%+v result=%+v err=%v", replay, result, err)
					}
					closed, err := loadTaskOwnerForRecovery(ctx, provisionalID)
					if err != nil || closed.ProviderState != TaskProviderStateClosed || closed.AcceptanceRecordedAt != nil || closed.ChargedQuota == nil || *closed.ChargedQuota != 0 || closed.SettlementDecision != "cancel" {
						t.Fatalf("duplicate reservation not cancelled: %+v %v", closed, err)
					}
				}
				var afterResources []ResourceOwner
				var afterResponses []ResponseOwner
				if err := db.Find(&afterResources).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Find(&afterResponses).Error; err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(resources, afterResources) || !reflect.DeepEqual(responses, afterResponses) {
					t.Fatalf("replay changed resource ownership or leaked slots: resources=%+v responses=%+v", afterResources, afterResponses)
				}
				var user User
				var originalToken, retryToken Token
				for _, read := range []*gorm.DB{db.First(&user, 1), db.First(&originalToken, 1), db.First(&retryToken, 2)} {
					if read.Error != nil {
						t.Fatal(read.Error)
					}
				}
				if user.Quota != wantQuota || user.UsedQuota != wantUsed || originalToken.RemainQuota != wantQuota || retryToken.RemainQuota != 1000 || retryToken.UsedQuota != 0 {
					t.Fatalf("replay changed balances: user=%+v original=%+v retry=%+v", user, originalToken, retryToken)
				}
			})
		}
	}
}

func TestAsyncTaskAcceptanceReplayRejectsDifferentIdentity(t *testing.T) {
	for _, platform := range []string{TaskPlatformOpenAIBatch, TaskPlatformOpenAIResponsesBackground} {
		for _, field := range []string{"user", "channel", "fingerprint", "namespace", "scope", "action", "case alias", "space alias"} {
			t.Run(platform+"/"+field, func(t *testing.T) {
				db := batchTestDB(t)
				if err := db.Create(&Channel{Id: 2, Type: config.ChannelTypeOpenAI, Name: "other", Key: "key", Status: config.ChannelStatusEnabled}).Error; err != nil {
					t.Fatal(err)
				}
				if field == "case alias" || field == "space alias" {
					collation := "NOCASE"
					if field == "space alias" {
						collation = "RTRIM"
					}
					var schema string
					if err := db.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'tasks'").Scan(&schema).Error; err != nil {
						t.Fatal(err)
					}
					collated := strings.Replace(schema, "`task_id` varchar(191)", "`task_id` varchar(191) COLLATE "+collation, 1)
					if collated == schema {
						t.Fatal("task_id column missing from fixture")
					}
					if err := db.Migrator().DropTable(&Task{}); err != nil {
						t.Fatal(err)
					}
					if err := db.Exec(collated).Error; err != nil {
						t.Fatal(err)
					}
					if err := db.AutoMigrate(&Task{}); err != nil {
						t.Fatal(err)
					}
				}
				existing := newAsyncReplayTask(t, platform, nil)
				if result, err := acceptAsyncReplayTask(t, existing, "async-id"); err != nil || result.Outcome != TaskMutationApplied {
					t.Fatalf("first accept: %+v %v", result, err)
				}
				replay := newAsyncReplayTask(t, platform, func(task *Task) {
					switch field {
					case "user":
						task.UserId, task.TokenID = 2, 2
					case "channel":
						task.ChannelId = 2
					case "fingerprint":
						task.RequestFingerprint = "another-request"
					case "namespace":
						task.ProviderNamespace = "another-provider"
					case "scope":
						task.ProviderTaskScopeIncarnation = "another-account"
					case "action":
						task.Action = "another-action"
					}
				})
				id := "async-id"
				if field == "case alias" {
					id = "ASYNC-ID"
				} else if field == "space alias" {
					id += " "
				}
				if result, err := acceptAsyncReplayTask(t, replay, id); !errors.Is(err, ErrTaskIdentity) || result.Outcome != TaskMutationDefinitelyNotApplied || replay.OwnerID == existing.OwnerID || replay.ProviderState != TaskProviderStateClosed {
					t.Fatalf("different %s reused owner: %+v %v", field, result, err)
				}
				var accepted int64
				if err := db.Model(&Task{}).Where("acceptance_recorded_at IS NOT NULL").Count(&accepted).Error; err != nil || accepted != 1 {
					t.Fatalf("conflict changed accepted tasks: %d %v", accepted, err)
				}
			})
		}
	}
}
