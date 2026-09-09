package midjourney

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	"one-api/model"
)

func TestIssue040MidjourneyBatchPollHasIndependentBoundedQueryContext(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	finished := make(chan struct{}, 1)
	db, channel, newOwner := midjourneyPollFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		finished <- struct{}{}
	}))
	owner := newOwner("slow")

	originalDeadline := midjourneyTaskPollDeadline
	midjourneyTaskPollDeadline = 40 * time.Millisecond
	t.Cleanup(func() { midjourneyTaskPollDeadline = originalDeadline })
	start := time.Now()
	err := updateTaskBatch(context.Background(), channel.Id, []string{"slow"}, map[string]*model.Task{"slow": owner})
	if err == nil {
		t.Fatal("slow Midjourney batch poll unexpectedly succeeded")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Midjourney batch poll exceeded bounded query budget: %v", elapsed)
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("slow Midjourney endpoint did not finish after the bounded query returned")
	}
	if calls.Load() != 1 {
		t.Fatalf("Midjourney batch provider calls=%d, want 1", calls.Load())
	}
	var saved model.Task
	if err := db.First(&saved, owner.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.ProviderState != model.TaskProviderStateAccepted || saved.ChargedQuota != nil {
		t.Fatalf("timed out poll changed owner settlement: %+v", saved)
	}
}

func TestIssue040MidjourneyBatchChannelLookupUsesBoundedQueryContext(t *testing.T) {
	var providerCalls atomic.Int32
	db, channel, newOwner := midjourneyPollFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
	}))
	owner := newOwner("channel-lookup")

	originalDeadline := midjourneyTaskPollDeadline
	midjourneyTaskPollDeadline = 40 * time.Millisecond
	t.Cleanup(func() { midjourneyTaskPollDeadline = originalDeadline })
	var lookupCalls atomic.Int32
	var remaining atomic.Int64
	if err := db.Callback().Query().Before("gorm:query").Register("issue040:midjourney-channel-query-context", func(tx *gorm.DB) {
		if tx.Statement.Table != "channels" {
			return
		}
		lookupCalls.Add(1)
		if tx.Statement.Context != nil {
			if deadline, ok := tx.Statement.Context.Deadline(); ok {
				remaining.Store(deadline.UnixNano() - time.Now().UnixNano())
			}
		}
		tx.AddError(errors.New("bounded channel lookup gate"))
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := updateTaskBatch(context.Background(), channel.Id, []string{"channel-lookup"}, map[string]*model.Task{"channel-lookup": owner}); err == nil {
		t.Fatal("channel lookup failure unexpectedly succeeded")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Midjourney channel lookup exceeded bounded budget: %v", elapsed)
	}
	if lookupCalls.Load() != 1 || providerCalls.Load() != 0 {
		t.Fatalf("Midjourney channel lookup calls=%d provider calls=%d, want one SQL lookup and no HTTP", lookupCalls.Load(), providerCalls.Load())
	}
	if got := time.Duration(remaining.Load()); got <= 0 || got > 200*time.Millisecond {
		t.Fatalf("Midjourney channel lookup deadline remaining=%v, want a short bounded deadline", got)
	}
}

func TestIssue040MidjourneyPollMutationUsesIndependentBoundedContext(t *testing.T) {
	db, channel, newOwner := midjourneyPollFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"terminal","status":"SUCCESS","progress":"100%"}]`))
	}))
	terminal, missing := newOwner("terminal"), newOwner("missing")

	var mutationMu sync.Mutex
	var mutationDeadlines []time.Duration
	if err := db.Callback().Update().Before("gorm:update").Register("issue040:midjourney-mutation-context", func(tx *gorm.DB) {
		updates, ok := tx.Statement.Dest.(map[string]any)
		if !ok {
			return
		}
		if _, hasStatus := updates["status"]; !hasStatus {
			return
		}
		if _, hasNextAction := updates["next_action_at"]; !hasNextAction {
			return
		}
		mutationMu.Lock()
		if tx.Statement.Context == nil {
			mutationDeadlines = append(mutationDeadlines, -1)
		} else if deadline, ok := tx.Statement.Context.Deadline(); ok {
			mutationDeadlines = append(mutationDeadlines, time.Until(deadline))
		} else {
			mutationDeadlines = append(mutationDeadlines, -1)
		}
		mutationMu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}

	if err := updateTaskBatch(context.Background(), channel.Id, []string{"terminal", "missing"}, map[string]*model.Task{
		"terminal": terminal,
		"missing":  missing,
	}); err != nil {
		t.Fatal(err)
	}
	var terminalSaved, missingSaved model.Task
	if err := db.First(&terminalSaved, terminal.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&missingSaved, missing.ID).Error; err != nil {
		t.Fatal(err)
	}
	if terminalSaved.ProviderState != model.TaskProviderStateClosed || terminalSaved.Status != model.TaskStatusSuccess || terminalSaved.ChargedQuota == nil || *terminalSaved.ChargedQuota != 0 {
		t.Fatalf("Midjourney terminal owner=%+v", terminalSaved)
	}
	if missingSaved.ProviderState != model.TaskProviderStateAccepted || missingSaved.NextActionAt <= time.Now().Unix() {
		t.Fatalf("Midjourney missing owner=%+v", missingSaved)
	}

	mutationMu.Lock()
	deadlines := append([]time.Duration(nil), mutationDeadlines...)
	mutationMu.Unlock()
	if len(deadlines) != 2 {
		t.Fatalf("Midjourney mutation callback count=%d, want terminal+reschedule", len(deadlines))
	}
	for _, remaining := range deadlines {
		if remaining <= 0 || remaining > 5*time.Second {
			t.Fatalf("Midjourney mutation deadline remaining=%v, want independent live <=5s", remaining)
		}
	}
	var user model.User
	if err := db.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 900 {
		t.Fatalf("Midjourney poll settlement changed user quota=%d, want 900", user.Quota)
	}
}

func TestIssue040CanceledMidjourneyBatchPollDoesNotStartProviderWork(t *testing.T) {
	var calls atomic.Int32
	db, channel, newOwner := midjourneyPollFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	owner := newOwner("canceled")
	var channelReads atomic.Int32
	if err := db.Callback().Query().Before("gorm:query").Register("issue040:midjourney-canceled-channel-read", func(tx *gorm.DB) {
		if tx.Statement.Table == "channels" {
			channelReads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := updateTaskBatch(ctx, channel.Id, []string{"canceled"}, map[string]*model.Task{"canceled": owner}); err == nil {
		t.Fatal("canceled Midjourney poll unexpectedly succeeded")
	}
	if calls.Load() != 0 {
		t.Fatalf("canceled Midjourney poll started %d provider calls", calls.Load())
	}
	if channelReads.Load() != 0 {
		t.Fatalf("canceled Midjourney poll performed %d channel reads", channelReads.Load())
	}
}
