package model

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIssue028CancelIsTerminalAndCannotReviveClosedOwner(t *testing.T) {
	useTaskBillingRepositoryTestDB(t)
	task := createDurableTaskOwner(t, 1, "provider-wide", "issue-028-cancel")
	claimDurableTaskOwner(t, task)
	acceptDurableTaskOwner(t, task, "issue-028-provider-task")

	stale := *task
	task.Status = TaskStatusCancel
	task.FailReason = "provider canceled"
	task.Progress = 42
	if result, err := FinalizeTaskBillingOwner(context.Background(), task, 0, "cancel"); err != nil || result.Outcome != BillingBalanceCommitted {
		t.Fatalf("cancel finalization: result=%+v err=%v", result, err)
	}
	if task.ProviderState != TaskProviderStateClosed || task.Status != TaskStatusCancel || task.Progress != 42 || task.NextActionAt != 0 || task.ChargedQuota == nil || *task.ChargedQuota != 0 || task.SettlementDecision != "cancel" {
		t.Fatalf("unexpected canceled owner: %+v", task)
	}

	if saved, err := SaveTaskPollSnapshot(context.Background(), task, time.Now().Add(TaskPollInterval)); saved.Outcome != TaskMutationDefinitelyNotApplied || !errors.Is(err, ErrTaskBillingState) {
		t.Fatalf("closed CANCEL owner accepted a poll snapshot: result=%+v err=%v", saved, err)
	}
	stale.Status = TaskStatusInProgress
	if saved, err := SaveTaskPollSnapshot(context.Background(), &stale, time.Now().Add(TaskPollInterval)); saved.Outcome != TaskMutationDefinitelyNotApplied || !errors.Is(err, ErrTaskBillingState) {
		t.Fatalf("late non-terminal snapshot revived CANCEL owner: result=%+v err=%v", saved, err)
	}

	var durable Task
	if err := DB.First(&durable, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if durable.ProviderState != TaskProviderStateClosed || durable.Status != TaskStatusCancel || durable.NextActionAt != 0 || durable.ChargedQuota == nil || *durable.ChargedQuota != 0 {
		t.Fatalf("closed owner changed after stale snapshots: %+v", durable)
	}
	var user User
	var token Token
	if err := DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := DB.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 1000 || token.RemainQuota != 1000 {
		t.Fatalf("cancel finalization did not release reservation: user=%d token=%d", user.Quota, token.RemainQuota)
	}
}
