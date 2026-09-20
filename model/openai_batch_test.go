package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func newBatchTestTask(t *testing.T) *Task {
	t.Helper()
	task := &Task{Platform: TaskPlatformOpenAIBatch, UserId: 1, TokenID: 1, ChannelId: 1, ReservedQuota: 10, ProviderNamespace: "openai", ProviderTaskScopeIncarnation: "channel:1", RequestFingerprint: "batch-test", Data: datatypes.JSON(`{"evidence":[]}`)}
	result, err := CreateTaskBillingOwner(context.Background(), task)
	if err != nil || result.Outcome != BillingBalanceCommitted {
		t.Fatalf("create: %+v %v", result, err)
	}
	claimDurableTaskOwner(t, task)
	return task
}

func batchTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := useTaskBillingRepositoryTestDB(t)
	if err := db.AutoMigrate(&ResourceOwner{}, &ResponseOwner{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func batchTestSlots(t *testing.T, task *Task) []*ResourceOwner {
	t.Helper()
	specs := make([]ResourceOwnerReservation, 3)
	for i, name := range []string{"batch", "output", "error"} {
		kind := "file"
		if i == 0 {
			kind = "batch"
		}
		specs[i] = ResourceOwnerReservation{Kind: kind, UserID: task.UserId, TokenID: task.TokenID, ChannelID: task.ChannelId, ProviderNamespace: task.ProviderNamespace, ProviderScope: task.ProviderTaskScopeIncarnation, TaskOwnerID: task.OwnerID, Slot: name, SubmitDeadline: time.Now().Add(time.Hour)}
	}
	slots, err := NewResourceOwnerRepository(DB).ReserveMany(context.Background(), specs)
	if err != nil {
		t.Fatal(err)
	}
	return slots
}

func TestOpenAIBatchAcceptanceAtomic(t *testing.T) {
	for _, failure := range []string{"none", "batch collision", "expired derived", "missing derived", "foreign derived"} {
		t.Run(failure, func(t *testing.T) {
			db := batchTestDB(t)
			task := newBatchTestTask(t)
			slots := batchTestSlots(t, task)
			ids := []uint64{slots[1].ID, slots[2].ID}
			switch failure {
			case "batch collision":
				spec := ResourceOwnerReservation{Kind: "batch", UserID: 1, TokenID: 1, ChannelID: 1, ProviderNamespace: "openai", ProviderScope: "channel:1", SubmitDeadline: time.Now().Add(time.Hour)}
				other, err := NewResourceOwnerRepository(db).Reserve(context.Background(), spec)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := NewResourceOwnerRepository(db).Bind(context.Background(), other.ID, 1, "batch_1"); err != nil {
					t.Fatal(err)
				}
			case "expired derived":
				if err := db.Model(&ResourceOwner{}).Where("id = ?", slots[2].ID).Update("reservation_expires_at", time.Now().Add(-time.Hour)).Error; err != nil {
					t.Fatal(err)
				}
			case "missing derived":
				if err := db.Delete(&ResourceOwner{}, slots[2].ID).Error; err != nil {
					t.Fatal(err)
				}
			case "foreign derived":
				if err := db.Model(&ResourceOwner{}).Where("id = ?", slots[2].ID).Update("channel_id", 2).Error; err != nil {
					t.Fatal(err)
				}
			}
			result, err := AcceptOpenAIBatchSubmission(context.Background(), task, "batch_1", slots[0].ID, ids)
			durable, readErr := GetOpenAIBatchTask(context.Background(), task.OwnerID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			var owner ResourceOwner
			if err := db.First(&owner, slots[0].ID).Error; err != nil {
				t.Fatal(err)
			}
			if failure != "none" {
				if err == nil || result.Outcome != TaskMutationDefinitelyNotApplied {
					t.Fatalf("accepted failed owner transaction: %+v %v", result, err)
				}
				if durable.ProviderState != TaskProviderStateSubmitStarted || owner.Phase != ResourceOwnerReserved || owner.UpstreamID != nil {
					t.Fatalf("partial acceptance durable=%+v owner=%+v", durable, owner)
				}
				return
			}
			if err != nil || result.Outcome != TaskMutationApplied || durable.ProviderState != TaskProviderStateAccepted || owner.Phase != ResourceOwnerBound {
				t.Fatalf("accept: %+v %v", result, err)
			}
			for _, id := range ids {
				var slot ResourceOwner
				if err := db.First(&slot, id).Error; err != nil {
					t.Fatal(err)
				}
				if slot.ReservationKind != ResourceReservationTaskDerived || slot.ReservationExpiresAt != nil {
					t.Fatalf("not transferred %+v", slot)
				}
			}
			if _, err := AcceptOpenAIBatchSubmission(context.Background(), task, "batch_1", slots[0].ID, ids); err != nil {
				t.Fatalf("idempotent accept: %v", err)
			}
		})
	}
}

func TestOpenAIBatchFinalizeOnceReleasesOnlyUnusedSlots(t *testing.T) {
	db := batchTestDB(t)
	task := newBatchTestTask(t)
	slots := batchTestSlots(t, task)
	if _, err := AcceptOpenAIBatchSubmission(context.Background(), task, "batch_1", slots[0].ID, []uint64{slots[1].ID, slots[2].ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewResourceOwnerRepository(db).Bind(context.Background(), slots[1].ID, 1, "file_output"); err != nil {
		t.Fatal(err)
	}
	task.Status = TaskStatusSuccess
	for i := 0; i < 2; i++ {
		result, err := FinalizeOpenAIBatchBillingOwner(context.Background(), task, 25, "confirm", []byte(`{"usage":25}`))
		if err != nil || result.Outcome != BillingBalanceCommitted {
			t.Fatalf("finalize: %+v %v", result, err)
		}
	}
	var user User
	if err := db.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 975 || user.UsedQuota != 25 {
		t.Fatalf("double billing %+v", user)
	}
	var remaining []ResourceOwner
	if err := db.Where("task_owner_id = ?", task.OwnerID).Find(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("slots left %+v", remaining)
	}
	for _, slot := range remaining {
		if slot.Phase != ResourceOwnerBound {
			t.Fatalf("unused slot retained %+v", slot)
		}
	}
}

func TestOpenAIBatchSnapshotCASAndRetention(t *testing.T) {
	db := batchTestDB(t)
	task := newBatchTestTask(t)
	slots := batchTestSlots(t, task)
	if _, err := AcceptOpenAIBatchSubmission(context.Background(), task, "batch_1", slots[0].ID, []uint64{slots[1].ID, slots[2].ID}); err != nil {
		t.Fatal(err)
	}
	stale := *task
	next := time.Now().Add(time.Minute)
	task.Data = datatypes.JSON(`{"usage":1}`)
	if _, err := SaveOpenAIBatchSnapshot(context.Background(), task, next); err != nil {
		t.Fatal(err)
	}
	stale.Data = datatypes.JSON(`{"usage":2}`)
	if _, err := SaveOpenAIBatchSnapshot(context.Background(), &stale, next); !errors.Is(err, ErrTaskBillingState) {
		t.Fatalf("stale accepted %v", err)
	}
	replay := *task
	replay.Version--
	if _, err := SaveOpenAIBatchSnapshot(context.Background(), &replay, next); err != nil {
		t.Fatalf("same snapshot recovery: %v", err)
	}
	task.Status = TaskStatusSuccess
	if _, err := FinalizeOpenAIBatchBillingOwner(context.Background(), task, 0, "cancel", task.Data); err != nil {
		t.Fatal(err)
	}
	live := newBatchTestTask(t)
	old := time.Now().Add(-BatchTaskRetention - time.Hour).Unix()
	if err := db.Model(&Task{}).Where("id IN ?", []int64{task.ID, live.ID}).Update("owner_closed_at", old).Error; err != nil {
		t.Fatal(err)
	}
	count, err := DeleteExpiredOpenAIBatchTasks(context.Background(), time.Now())
	if err != nil || count != 1 {
		t.Fatalf("cleanup %d %v", count, err)
	}
	if _, err := GetOpenAIBatchTask(context.Background(), live.OwnerID); err != nil {
		t.Fatalf("live removed %v", err)
	}
}

func TestOpenAIBatchCapacityAndBoundedData(t *testing.T) {
	db := batchTestDB(t)
	for i := 0; i < BatchTaskLimit; i++ {
		newBatchTestTask(t)
	}
	var before User
	if err := db.First(&before, 1).Error; err != nil {
		t.Fatal(err)
	}
	task := &Task{Platform: TaskPlatformOpenAIBatch, UserId: 1, TokenID: 1, ChannelId: 1, ReservedQuota: 10, ProviderNamespace: "openai", ProviderTaskScopeIncarnation: "channel:1", RequestFingerprint: "overflow", Data: datatypes.JSON(`{}`)}
	if _, err := CreateTaskBillingOwner(context.Background(), task); !errors.Is(err, ErrOpenAIBatchCapacity) {
		t.Fatalf("capacity: %v", err)
	}
	var after User
	if err := db.First(&after, 1).Error; err != nil {
		t.Fatal(err)
	}
	if before.Quota != after.Quota {
		t.Fatal("capacity failure retained balance reservation")
	}
	task.Data = datatypes.JSON(`invalid`)
	if _, err := CreateTaskBillingOwner(context.Background(), task); !errors.Is(err, ErrTaskBillingState) {
		t.Fatalf("bad data: %v", err)
	}
	task.Data = make(datatypes.JSON, BatchDataMaxBytes+1)
	if _, err := CreateTaskBillingOwner(context.Background(), task); !errors.Is(err, ErrTaskBillingState) {
		t.Fatalf("oversized data: %v", err)
	}
}

func TestOpenAIBatchCannotBypassResourceAcceptance(t *testing.T) {
	batchTestDB(t)
	task := newBatchTestTask(t)
	if result, err := AcceptTaskSubmission(context.Background(), task, "batch_1"); !errors.Is(err, ErrTaskIdentity) || result.Outcome != TaskMutationDefinitelyNotApplied {
		t.Fatalf("generic acceptance bypassed owner transaction: %+v %v", result, err)
	}
	durable, err := GetOpenAIBatchTask(context.Background(), task.OwnerID)
	if err != nil || durable.ProviderState != TaskProviderStateSubmitStarted {
		t.Fatalf("task unexpectedly accepted: %+v %v", durable, err)
	}
}

func TestOpenAIBatchGenericFinalizeReleasesReservedSlots(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		name := "submission not accepted"
		if accepted {
			name = "accepted task"
		}
		t.Run(name, func(t *testing.T) {
			db := batchTestDB(t)
			task := newBatchTestTask(t)
			slots := batchTestSlots(t, task)
			if accepted {
				if _, err := AcceptOpenAIBatchSubmission(context.Background(), task, "batch_generic", slots[0].ID, []uint64{slots[1].ID, slots[2].ID}); err != nil {
					t.Fatal(err)
				}
			}
			task.Status = TaskStatusUnknown
			for i := 0; i < 2; i++ {
				result, err := FinalizeTaskBillingOwner(context.Background(), task, 0, "cancel")
				if err != nil || result.Outcome != BillingBalanceCommitted {
					t.Fatalf("generic finalize: %+v %v", result, err)
				}
			}
			var reserved int64
			if err := db.Model(&ResourceOwner{}).Where("task_owner_id = ? AND phase = ?", task.OwnerID, ResourceOwnerReserved).Count(&reserved).Error; err != nil {
				t.Fatal(err)
			}
			if reserved != 0 {
				t.Fatalf("generic finalize leaked %d reservations", reserved)
			}
			var user User
			if err := db.First(&user, 1).Error; err != nil {
				t.Fatal(err)
			}
			if user.Quota != 1000 || user.UsedQuota != 0 {
				t.Fatalf("repeated cancel changed balance: %+v", user)
			}
			if accepted {
				var owner ResourceOwner
				if err := db.First(&owner, slots[0].ID).Error; err != nil || owner.Phase != ResourceOwnerBound {
					t.Fatalf("bound owner lost: %+v %v", owner, err)
				}
			}
		})
	}
}
