package model

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestOpenAIBatchTaskPreservesExactProviderHandle(t *testing.T) {
	batchTestDB(t)
	task := newBatchTestTask(t)
	slots := batchTestSlots(t, task)
	const id = " batch-raw "
	if result, err := PreserveTaskSubmissionHandle(context.Background(), task, id); err != nil || result.Outcome != TaskMutationApplied {
		t.Fatalf("preserve: %+v %v", result, err)
	}
	if TaskProviderID(task) != id {
		t.Fatalf("rewritten diagnostic handle: %q", TaskProviderID(task))
	}
	if result, err := AcceptOpenAIBatchSubmission(context.Background(), task, "batch-raw", slots[0].ID, []uint64{slots[1].ID, slots[2].ID}); !errors.Is(err, ErrTaskIdentity) || result.Outcome != TaskMutationDefinitelyNotApplied {
		t.Fatalf("trimmed alias accepted: %+v %v", result, err)
	}
	if result, err := AcceptOpenAIBatchSubmission(context.Background(), task, id, slots[0].ID, []uint64{slots[1].ID, slots[2].ID}); err != nil || result.Outcome != TaskMutationApplied {
		t.Fatalf("accept raw: %+v %v", result, err)
	}
	owner, err := GetResourceOwner(context.Background(), "batch", id, task.UserId)
	if err != nil || owner.UpstreamID == nil || *owner.UpstreamID != id {
		t.Fatalf("resource identity differs: %+v %v", owner, err)
	}
	durable, err := GetOpenAIBatchTask(context.Background(), task.OwnerID)
	if err != nil || TaskProviderID(durable) != id {
		t.Fatalf("durable identity differs: %+v %v", durable, err)
	}
	if _, err := GetResourceOwner(context.Background(), "batch", "batch-raw", task.UserId); !errors.Is(err, ErrResourceOwnerNotFound) {
		t.Fatalf("trim alias authorized: %v", err)
	}
	task.Status = TaskStatusSuccess
	if _, err := FinalizeOpenAIBatchBillingOwner(context.Background(), task, 0, "cancel", task.Data); err != nil {
		t.Fatal(err)
	}
	if TaskProviderID(task) != id {
		t.Fatalf("finalizer rewrote ID: %q", TaskProviderID(task))
	}
}

func TestOpenAIBatchTaskRejectsDatabaseCollationAliases(t *testing.T) {
	batchTestDB(t)
	responseIdentityCasefoldTable(t, DB, "tasks", "task_id", "varchar(191)", &Task{})
	responseIdentityCasefoldTable(t, DB, "tasks", "owner_id", "varchar(36)", &Task{})
	task := newBatchTestTask(t)
	slots := batchTestSlots(t, task)
	if result, err := PreserveTaskSubmissionHandle(context.Background(), task, "batch-case"); err != nil || result.Outcome != TaskMutationApplied {
		t.Fatalf("preserve: %+v %v", result, err)
	}
	if result, err := PreserveTaskSubmissionHandle(context.Background(), task, "BATCH-CASE"); !errors.Is(err, ErrTaskIdentity) || result.Outcome != TaskMutationDefinitelyNotApplied {
		t.Fatalf("SQL alias overwrote diagnostic handle: %+v %v", result, err)
	}
	if _, err := AcceptOpenAIBatchSubmission(context.Background(), task, "batch-case", slots[0].ID, []uint64{slots[1].ID, slots[2].ID}); err != nil {
		t.Fatal(err)
	}
	var matches int64
	if err := DB.Model(&Task{}).Where("task_id = ?", "BATCH-CASE").Count(&matches).Error; err != nil || matches != 1 {
		t.Fatalf("fixture did not exercise collation: %d %v", matches, err)
	}
	if _, err := GetTaskByTaskId(task.Platform, task.UserId, "BATCH-CASE"); !errors.Is(err, ErrTaskLookupConflict) {
		t.Fatalf("SQL alias resolved Task: %v", err)
	}
	if _, err := GetTaskByTaskIds(task.Platform, task.UserId, []string{"BATCH-CASE"}); !errors.Is(err, ErrTaskLookupConflict) {
		t.Fatalf("SQL alias resolved Task list: %v", err)
	}
	if _, err := GetOpenAIBatchTask(context.Background(), strings.ToUpper(task.OwnerID)); !errors.Is(err, ErrTaskIdentity) {
		t.Fatalf("SQL owner alias resolved Task: %v", err)
	}
	alias := *task
	SetTaskProviderID(&alias, "BATCH-CASE")
	alias.Status = TaskStatusSuccess
	if _, err := FinalizeOpenAIBatchBillingOwner(context.Background(), &alias, 0, "cancel", alias.Data); !errors.Is(err, ErrTaskIdentity) {
		t.Fatalf("SQL alias settled Task: %v", err)
	}
}
