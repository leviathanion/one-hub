package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func backgroundTaskDB(t *testing.T) {
	t.Helper()
	db := useTaskBillingRepositoryTestDB(t)
	if err := db.AutoMigrate(&ResponseOwner{}); err != nil {
		t.Fatal(err)
	}
}

func createBackgroundTask(t *testing.T) *Task {
	t.Helper()
	task := &Task{Platform: TaskPlatformOpenAIResponsesBackground, UserId: 1, TokenID: 1, ChannelId: 1, ReservedQuota: 10, ProviderNamespace: "openai", ProviderTaskScopeIncarnation: "provider-wide", RequestFingerprint: "background-fixture", Data: datatypes.JSON(`{"model":"fixture"}`)}
	if result, err := CreateTaskBillingOwner(context.Background(), task); err != nil || result.Outcome != BillingBalanceCommitted {
		t.Fatalf("create: %+v %v", result, err)
	}
	return task
}

func backgroundOwner(t *testing.T, task *Task, responseID string) *ResponseOwner {
	t.Helper()
	owner, err := NewBackgroundResponseOwner(responseID, task.UserId, task.TokenID, task.ChannelId, time.Now(), false, "openai", "provider-wide")
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func acceptBackground(t *testing.T, task *Task, responseID string) *ResponseOwner {
	t.Helper()
	claimDurableTaskOwner(t, task)
	owner := backgroundOwner(t, task, responseID)
	if result, err := AcceptBackgroundResponseSubmission(context.Background(), task, responseID, owner); err != nil || result.Outcome != TaskMutationApplied {
		t.Fatalf("accept: %+v %v", result, err)
	}
	return owner
}

func TestBackgroundResponseCapacityReserveAtomic(t *testing.T) {
	backgroundTaskDB(t)
	var first *Task
	for i := 0; i < BackgroundResponseTaskLimit; i++ {
		task := createBackgroundTask(t)
		if i == 0 {
			first = task
		}
	}
	extra := &Task{Platform: TaskPlatformOpenAIResponsesBackground, UserId: 1, TokenID: 1, ChannelId: 1, ReservedQuota: 10, ProviderNamespace: "openai", ProviderTaskScopeIncarnation: "provider-wide", RequestFingerprint: "extra", Data: datatypes.JSON(`{}`)}
	result, err := CreateTaskBillingOwner(context.Background(), extra)
	if !errors.Is(err, ErrBackgroundResponseCapacity) || result.Outcome != BillingBalanceDefinitelyRolledBack {
		t.Fatalf("capacity: %+v %v", result, err)
	}
	var user User
	if err := DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 1000-BackgroundResponseTaskLimit*10 {
		t.Fatalf("failed reserve changed balance: %d", user.Quota)
	}
	first.Status = TaskStatusUnknown
	if _, err := FinalizeBackgroundResponseBillingOwner(context.Background(), first, 0, "cancel", []byte(`{"reason":"unsubmitted"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateTaskBillingOwner(context.Background(), extra); err != nil {
		t.Fatalf("released slot: %v", err)
	}
}

func TestBackgroundResponseAcceptanceOwnerAtomic(t *testing.T) {
	backgroundTaskDB(t)
	task := createBackgroundTask(t)
	claimDurableTaskOwner(t, task)
	owner := backgroundOwner(t, task, "resp_atomic")
	// A pre-existing different principal must roll back the Task acceptance too.
	conflict := *owner
	conflict.UserID = 2
	if err := CreateResponseOwner(context.Background(), &conflict); err != nil {
		t.Fatal(err)
	}
	result, err := AcceptBackgroundResponseSubmission(context.Background(), task, owner.ResponseID, owner)
	if err == nil || result.Outcome != TaskMutationDefinitelyNotApplied {
		t.Fatalf("conflict: %+v %v", result, err)
	}
	durable, err := GetBackgroundResponseTask(context.Background(), task.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if durable.ProviderState != TaskProviderStateSubmitStarted || durable.AcceptanceRecordedAt != nil {
		t.Fatalf("partial acceptance: %+v", durable)
	}
	owner = backgroundOwner(t, task, "resp_good")
	if result, err := AcceptBackgroundResponseSubmission(context.Background(), task, owner.ResponseID, owner); err != nil || result.Outcome != TaskMutationApplied {
		t.Fatalf("accept: %+v %v", result, err)
	}
	got, err := GetResponseOwner(context.Background(), owner.ResponseID, 1)
	if err != nil || got.TaskOwnerID == nil || *got.TaskOwnerID != task.OwnerID {
		t.Fatalf("owner: %+v %v", got, err)
	}
	if result, err := AcceptBackgroundResponseSubmission(context.Background(), task, owner.ResponseID, owner); err != nil || result.Outcome != TaskMutationApplied {
		t.Fatalf("repeat: %+v %v", result, err)
	}
}

func TestBackgroundResponseFinalizeEvidenceFenceAndRetention(t *testing.T) {
	backgroundTaskDB(t)
	task := createBackgroundTask(t)
	owner := acceptBackground(t, task, "resp_final")
	// An old local retention deadline gets extended by the same settlement.
	if err := DB.Model(&ResponseOwner{}).Where("id = ?", owner.ID).Update("expires_at", time.Now().Add(time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	stale := *task
	task.Status = TaskStatusSuccess
	data := []byte(`{"model":"fixture","usage":{"input_tokens":3},"evidence_id":"resp_final"}`)
	if result, err := FinalizeBackgroundResponseBillingOwner(context.Background(), task, 25, "confirm", data); err != nil || result.Outcome != BillingBalanceCommitted {
		t.Fatalf("finalize: %+v %v", result, err)
	}
	stale.Status = TaskStatusUnknown
	if result, err := FinalizeBackgroundResponseBillingOwner(context.Background(), &stale, 0, "cancel", []byte(`{"wrong":true}`)); err != nil || result.Outcome != BillingBalanceCommitted {
		t.Fatalf("late: %+v %v", result, err)
	}
	durable, err := GetBackgroundResponseTask(context.Background(), task.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if string(durable.Data) != string(data) || durable.ChargedQuota == nil || *durable.ChargedQuota != 25 {
		t.Fatalf("late evidence overwrote decision: %+v", durable)
	}
	got, err := GetResponseOwner(context.Background(), owner.ResponseID, 1)
	if err != nil || got.ExpiresAt.Before(time.Now().Add(24*time.Hour-time.Minute)) {
		t.Fatalf("retention: %+v %v", got, err)
	}
	var user User
	if err := DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 975 || user.UsedQuota != 25 {
		t.Fatalf("double settlement: %+v", user)
	}
}

func TestBackgroundResponseStaleEvidenceRollsBack(t *testing.T) {
	backgroundTaskDB(t)
	task := createBackgroundTask(t)
	acceptBackground(t, task, "resp_stale")
	task.Status = TaskStatusSuccess
	if err := DB.Model(&Task{}).Where("id = ?", task.ID).Update("version", gorm.Expr("version + 1")).Error; err != nil {
		t.Fatal(err)
	}
	if result, err := FinalizeBackgroundResponseBillingOwner(context.Background(), task, 25, "confirm", []byte(`{"new":true}`)); !errors.Is(err, ErrTaskBillingState) || result.Outcome != BillingBalanceDefinitelyRolledBack {
		t.Fatalf("stale: %+v %v", result, err)
	}
	got, err := GetBackgroundResponseTask(context.Background(), task.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data) != `{"model":"fixture"}` || got.ChargedQuota != nil {
		t.Fatalf("stale evidence persisted: %+v", got)
	}
}

func TestBackgroundResponseCleanupOnlyOldClosedTasks(t *testing.T) {
	backgroundTaskDB(t)
	now := time.Now()
	for i := 0; i < 3; i++ {
		task := createBackgroundTask(t)
		acceptBackground(t, task, fmt.Sprintf("resp_cleanup_%d", i))
		if i == 2 {
			continue
		}
		task.Status = TaskStatusSuccess
		if _, err := FinalizeBackgroundResponseBillingOwner(context.Background(), task, 0, "cancel", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := DB.Model(&Task{}).Where("id = ?", task.ID).Update("owner_closed_at", now.Add(-91*24*time.Hour).Unix()).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	deleted, err := DeleteExpiredBackgroundResponseTasks(context.Background(), now)
	if err != nil || deleted != 1 {
		t.Fatalf("cleanup: %d %v", deleted, err)
	}
	var owners int64
	if err := DB.Model(&ResponseOwner{}).Count(&owners).Error; err != nil {
		t.Fatal(err)
	}
	if owners != 3 {
		t.Fatalf("task cleanup cascaded owners: %d", owners)
	}
}

func TestBackgroundResponseAcceptanceRejectsChangedPrincipal(t *testing.T) {
	backgroundTaskDB(t)
	task := createBackgroundTask(t)
	claimDurableTaskOwner(t, task)
	forged := *task
	forged.UserId = 2
	forged.TokenID = 2
	owner := backgroundOwner(t, &forged, "resp_wrong_user")
	if result, err := AcceptBackgroundResponseSubmission(context.Background(), &forged, owner.ResponseID, owner); err == nil || result.Outcome != TaskMutationDefinitelyNotApplied {
		t.Fatalf("accepted mismatched durable principal: %+v %v", result, err)
	}
	var count int64
	if err := DB.Model(&ResponseOwner{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("mismatched principal produced owner")
	}
}

func TestBackgroundResponseEvidenceCASAndReplay(t *testing.T) {
	backgroundTaskDB(t)
	task := createBackgroundTask(t)
	acceptBackground(t, task, "resp_evidence")
	stale := *task
	task.Data = datatypes.JSON(`{"usage":{"input_tokens":7}}`)
	task.Status = TaskStatusInProgress
	replay := *task
	next := time.Now().Add(TaskPollInterval)
	version := task.Version
	if result, err := SaveBackgroundResponseEvidence(context.Background(), task, next); err != nil || result.Outcome != TaskMutationApplied {
		t.Fatalf("save: %+v %v", result, err)
	}
	if task.Version != version+1 {
		t.Fatalf("version not advanced: %d", task.Version)
	}
	if result, err := SaveBackgroundResponseEvidence(context.Background(), &replay, next); err != nil || result.Outcome != TaskMutationApplied || replay.Version != task.Version {
		t.Fatalf("replay: %+v %v", result, err)
	}
	stale.Data = datatypes.JSON(`{"usage":{"input_tokens":1}}`)
	if result, err := SaveBackgroundResponseEvidence(context.Background(), &stale, next); !errors.Is(err, ErrTaskBillingState) || result.Outcome != TaskMutationDefinitelyNotApplied {
		t.Fatalf("stale: %+v %v", result, err)
	}
	got, err := GetBackgroundResponseTask(context.Background(), task.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data) != string(task.Data) || got.Version != task.Version {
		t.Fatalf("stale overwrote evidence: %+v", got)
	}
	task.Status = TaskStatusSuccess
	if _, err := FinalizeBackgroundResponseBillingOwner(context.Background(), task, 7, "confirm", task.Data); err != nil {
		t.Fatal(err)
	}
	if result, err := SaveBackgroundResponseEvidence(context.Background(), &replay, next); !errors.Is(err, ErrTaskBillingState) || result.Outcome != TaskMutationDefinitelyNotApplied {
		t.Fatalf("closed owner changed: %+v %v", result, err)
	}
}

func TestBackgroundResponseEvidenceRecoversCommittedWriteError(t *testing.T) {
	backgroundTaskDB(t)
	task := createBackgroundTask(t)
	acceptBackground(t, task, "resp_recovery")
	fired := false
	const hook = "test:background_evidence_commit_error"
	if err := DB.Callback().Update().After("gorm:commit_or_rollback_transaction").Register(hook, func(tx *gorm.DB) {
		if !fired && tx.Statement.Table == "tasks" && tx.Error == nil {
			fired = true
			tx.AddError(errors.New("commit acknowledgment lost"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { DB.Callback().Update().Remove(hook) })
	task.Data = datatypes.JSON(`{"usage":{"input_tokens":11}}`)
	oldVersion := task.Version
	if result, err := SaveBackgroundResponseEvidence(context.Background(), task, time.Now().Add(TaskPollInterval)); err != nil || result.Outcome != TaskMutationApplied {
		t.Fatalf("read recovery: %+v %v", result, err)
	}
	if !fired || task.Version != oldVersion+1 {
		t.Fatalf("missing recovery: fired=%t version=%d", fired, task.Version)
	}
}

func TestBackgroundResponseEvidenceRecoveryJSONNormalization(t *testing.T) {
	expected := &Task{ProviderState: TaskProviderStateAccepted, Version: 3, Data: datatypes.JSON(`{"tokens":9007199254740993,"model":"fixture"}`), NextActionAt: 5}
	durable := *expected
	durable.Data = datatypes.JSON(`{"model": "fixture", "tokens": 9007199254740993}`)
	if !sameBackgroundResponseEvidence(&durable, expected, 5) {
		t.Fatal("database JSON formatting prevented recovery")
	}
	durable.Data = datatypes.JSON(`{"model":"fixture","tokens":9007199254740992}`)
	if sameBackgroundResponseEvidence(&durable, expected, 5) {
		t.Fatal("numeric precision loss treated distinct evidence as equal")
	}
}

func TestBackgroundResponseTaskPreservesRawHandle(t *testing.T) {
	backgroundTaskDB(t)
	task := createBackgroundTask(t)
	const id = " resp-background "
	claimDurableTaskOwner(t, task)
	if result, err := PreserveTaskSubmissionHandle(context.Background(), task, id); err != nil || result.Outcome != TaskMutationApplied {
		t.Fatalf("preserve raw: %+v %v", result, err)
	}
	if TaskProviderID(task) != id {
		t.Fatalf("preserved rewritten ID: %q", TaskProviderID(task))
	}
	wrong := backgroundOwner(t, task, "resp-background")
	if result, err := AcceptBackgroundResponseSubmission(context.Background(), task, wrong.ResponseID, wrong); err == nil || result.Outcome != TaskMutationDefinitelyNotApplied {
		t.Fatalf("accepted trimmed handle alias: %+v %v", result, err)
	}
	owner := backgroundOwner(t, task, id)
	if result, err := AcceptBackgroundResponseSubmission(context.Background(), task, id, owner); err != nil || result.Outcome != TaskMutationApplied {
		t.Fatalf("accept raw: %+v %v", result, err)
	}
	if result, err := PreserveTaskSubmissionHandle(context.Background(), task, id); err != nil || result.Outcome != TaskMutationApplied {
		t.Fatalf("accepted handle read recovery: %+v %v", result, err)
	}
	durable, err := GetBackgroundResponseTask(context.Background(), task.OwnerID)
	if err != nil || TaskProviderID(durable) != id {
		t.Fatalf("durable raw: %+v %v", durable, err)
	}
	if result, err := ClaimTaskPoll(context.Background(), durable, time.Now().Add(time.Second), TaskPollFenceWindow); err != nil || result.Outcome != TaskMutationApplied {
		t.Fatalf("poll raw: %+v %v", result, err)
	}
	if TaskProviderID(durable) != id {
		t.Fatalf("poll rewrote ID: %q", TaskProviderID(durable))
	}
	durable.Status = TaskStatusSuccess
	if _, err := FinalizeBackgroundResponseBillingOwner(context.Background(), durable, 0, "cancel", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if TaskProviderID(durable) != id {
		t.Fatalf("close rewrote ID: %q", TaskProviderID(durable))
	}
	if result, err := classifyTaskAcceptance(context.Background(), durable, "resp-background", nil); err == nil || result.Outcome != TaskMutationDefinitelyNotApplied {
		t.Fatalf("recovery accepted alias: %+v %v", result, err)
	}
}

func TestBackgroundResponseTaskRejectsCollationAlias(t *testing.T) {
	backgroundTaskDB(t)
	responseIdentityCasefoldTable(t, DB, "tasks", "task_id", "varchar(191)", &Task{})
	task := createBackgroundTask(t)
	claimDurableTaskOwner(t, task)
	if result, err := PreserveTaskSubmissionHandle(context.Background(), task, "resp_case"); err != nil || result.Outcome != TaskMutationApplied {
		t.Fatalf("preserve: %+v %v", result, err)
	}
	if result, err := PreserveTaskSubmissionHandle(context.Background(), task, "RESP_CASE"); err == nil || result.Outcome != TaskMutationDefinitelyNotApplied {
		t.Fatalf("SQL alias replaced diagnostic handle: %+v %v", result, err)
	}
	owner := backgroundOwner(t, task, "resp_case")
	if _, err := AcceptBackgroundResponseSubmission(context.Background(), task, owner.ResponseID, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := GetTaskByTaskId(task.Platform, task.UserId, "RESP_CASE"); !errors.Is(err, ErrTaskLookupConflict) {
		t.Fatalf("SQL alias resolved task: %v", err)
	}
	if _, err := GetTaskByTaskIds(task.Platform, task.UserId, []string{"RESP_CASE"}); !errors.Is(err, ErrTaskLookupConflict) {
		t.Fatalf("SQL alias resolved task list: %v", err)
	}
	altered := *task
	SetTaskProviderID(&altered, "RESP_CASE")
	altered.Status = TaskStatusSuccess
	if _, err := FinalizeBackgroundResponseBillingOwner(context.Background(), &altered, 0, "cancel", []byte(`{}`)); !errors.Is(err, ErrTaskIdentity) {
		t.Fatalf("finalize accepted alias: %v", err)
	}
}
