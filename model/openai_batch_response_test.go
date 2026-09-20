package model

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOpenAIBatchResponseCapacityTransferAndRecovery(t *testing.T) {
	r, spec, _ := resourceOwnerFixture(t)
	original := DB
	DB = r.DB
	t.Cleanup(func() { DB = original })
	ctx := context.Background()
	spec.Kind = "response"
	spec.TaskOwnerID = "batch-capacity-source"
	spec.Slot = "item-response"
	slot, err := r.Reserve(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.TransferToTask(ctx, 1, spec.TaskOwnerID, slot.ID); err != nil {
		t.Fatal(err)
	}
	// 接受确认丢失后的提交栈清理不能释放已转交的 Task 槽。
	if err := r.ReleaseSubmittingTaskSlots(ctx, 1, spec.TaskOwnerID); err != nil {
		t.Fatal(err)
	}
	if slots, err := r.TaskSlots(ctx, 1, spec.TaskOwnerID); err != nil || len(slots) != 1 {
		t.Fatalf("accepted slot was released: %+v %v", slots, err)
	}
	task := &Task{OwnerID: spec.TaskOwnerID, Platform: TaskPlatformOpenAIBatch, UserId: 1, TokenID: spec.TokenID, ChannelId: spec.ChannelID, ProviderNamespace: spec.ProviderNamespace, ProviderTaskScopeIncarnation: spec.ProviderScope}
	if count, err := resourceOwnerCount(DB, 1, time.Now()); err != nil || count != 1 {
		t.Fatalf("before transfer capacity=%d %v", count, err)
	}
	if err := BindOpenAIBatchResponse(ctx, task, slot.ID, spec.Slot, "resp-capacity"); err != nil {
		t.Fatal(err)
	}
	if count, err := resourceOwnerCount(DB, 1, time.Now()); err != nil || count != 1 {
		t.Fatalf("transfer changed capacity=%d %v", count, err)
	}
	if err := BindOpenAIBatchResponse(ctx, task, slot.ID, spec.Slot, "resp-capacity"); err != nil {
		t.Fatal("reloaded binding is not idempotent", err)
	}
	if err := BindOpenAIBatchResponse(ctx, task, slot.ID, spec.Slot, "resp-replacement"); !errors.Is(err, ErrResponseOwnerConflict) {
		t.Fatalf("reclaimed slot: %v", err)
	}
	owner, err := GetResponseOwner(ctx, "resp-capacity", 1)
	if err != nil || owner.BatchOwnerID == nil || *owner.BatchOwnerID != task.OwnerID {
		t.Fatalf("source owner=%+v %v", owner, err)
	}
	if err := TombstoneResponseOwner(ctx, "resp-capacity", 1); err != nil {
		t.Fatal(err)
	}
	if count, err := resourceOwnerCount(DB, 1, time.Now()); err != nil || count != 0 {
		t.Fatalf("tombstone still consumes capacity=%d %v", count, err)
	}
	if _, err := GetResponseOwner(ctx, "resp-capacity", 1); err != nil {
		t.Fatal("tombstone revoked access", err)
	}
}
