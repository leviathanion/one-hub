package midjourney

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"one-api/model"
	taskbase "one-api/relay/task/base"
)

func TestIssue028MidjourneyTerminalSnapshotsRefundAndStopPolling(t *testing.T) {
	for _, test := range []struct {
		status   model.TaskStatus
		progress string
		reason   string
	}{
		{status: model.TaskStatusCancel, progress: "42%", reason: "provider canceled"},
		{status: model.TaskStatusSuccess, progress: "100%", reason: ""},
		{status: model.TaskStatusFailure, progress: "42%", reason: "provider failed"},
	} {
		t.Run(string(test.status), func(t *testing.T) {
			var providerCalls atomic.Int32
			db, channel, newOwner := midjourneyPollFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				providerCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `[{"id":"issue-028-task","status":%q,"progress":%q,"failReason":%q}]`, test.status, test.progress, test.reason)
			}))
			owner := newOwner("issue-028-task")
			stale := *owner

			// This is the same durable view used by the public MJ fetch path.
			before := model.MidjourneyFromTask(owner)
			if before.Status != string(model.TaskStatusSubmitted) {
				t.Fatalf("public view before provider observation=%+v", before)
			}
			if err := updateTaskBatch(context.Background(), channel.Id, []string{"issue-028-task"}, map[string]*model.Task{"issue-028-task": owner}); err != nil {
				t.Fatal(err)
			}

			var durable model.Task
			if err := db.First(&durable, owner.ID).Error; err != nil {
				t.Fatal(err)
			}
			if durable.ProviderState != model.TaskProviderStateClosed || durable.Status != test.status || durable.Progress != 100 || durable.NextActionAt != 0 || durable.ChargedQuota == nil || *durable.ChargedQuota != 0 || durable.SettlementDecision != "cancel" {
				t.Fatalf("terminal owner was not refunded and closed: %+v", durable)
			}
			if durable.FailReason != test.reason {
				t.Fatalf("provider failure reason was not preserved: got=%q want=%q", durable.FailReason, test.reason)
			}
			after := model.MidjourneyFromTask(&durable)
			if after.Status != string(test.status) || after.Progress != "100%" || after.FailReason != test.reason {
				t.Fatalf("public terminal view changed status/evidence: %+v", after)
			}
			assertMidjourneyPollBalances(t, db, 1000)

			// A duplicate finalizer is a no-op, and a late non-terminal snapshot
			// cannot pass the accepted-owner/version fence.
			if _, err := taskbase.FinalizeTaskSettlement(context.Background(), &durable); err != nil {
				t.Fatalf("duplicate terminal finalization: %v", err)
			}
			assertMidjourneyPollBalances(t, db, 1000)
			stale.Status = model.TaskStatusInProgress
			if saved, err := model.SaveTaskPollSnapshot(context.Background(), &stale, time.Now().Add(model.TaskPollInterval)); saved.Outcome != model.TaskMutationDefinitelyNotApplied || !errors.Is(err, model.ErrTaskBillingState) {
				t.Fatalf("late non-terminal snapshot unexpectedly saved: result=%+v err=%v", saved, err)
			}

			due, err := model.ListDueTaskOwners(context.Background(), time.Now(), 0, 0, model.TaskProgressPageSize)
			if err != nil {
				t.Fatal(err)
			}
			if len(due) != 0 || providerCalls.Load() != 1 {
				t.Fatalf("closed terminal task remained pollable: due=%d provider_calls=%d", len(due), providerCalls.Load())
			}
		})
	}
}
