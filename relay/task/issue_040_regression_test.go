package task

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/requester"
	"one-api/model"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

func TestIssue040KlingPollsHealthyFollowerAfterSlowTaskAcrossRounds(t *testing.T) {
	_, _ = taskSubmitFixture(t)
	closeIssue040Database(t)

	var slowCalls, healthyCalls atomic.Int32
	slowStarted := make(chan struct{}, 2)
	slowFinished := make(chan struct{}, 2)
	var orderMu sync.Mutex
	var requestOrder []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := path.Base(r.URL.Path)
		orderMu.Lock()
		requestOrder = append(requestOrder, requestID)
		orderMu.Unlock()
		switch requestID {
		case "slow":
			slowCalls.Add(1)
			slowStarted <- struct{}{}
			<-r.Context().Done()
			slowFinished <- struct{}{}
		case "healthy":
			call := healthyCalls.Add(1)
			status := "processing"
			updatedAt := 2
			if call >= 2 {
				status = "succeed"
				updatedAt = 3
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"task_id":"healthy","task_status":%q,"created_at":1,"updated_at":%d,"task_result":{"videos":[{"id":"video","url":"https://example.invalid/video.mp4"}]}}}`, status, updatedAt)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	oldHTTPClient := requester.HTTPClient
	t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })
	requester.HTTPClient = server.Client()
	// The production poll context remains 30 seconds.  This shorter transport
	// budget only makes the slow test endpoint deterministic; it does not alter
	// the production timeout contract.
	requester.HTTPClient.Timeout = 50 * time.Millisecond
	if err := model.DB.Model(&model.Channel{}).Where("id = ?", 1).Updates(map[string]any{
		"base_url": server.URL,
		"proxy":    "",
	}).Error; err != nil {
		t.Fatal(err)
	}

	slowOwner := createIssue040AcceptedOwner(t, "slow")
	healthyOwner := createIssue040AcceptedOwner(t, "healthy")

	var mutationMu sync.Mutex
	var mutationDeadlines []time.Duration
	if err := model.DB.Callback().Update().Before("gorm:update").Register("issue040:poll-mutation-context", func(tx *gorm.DB) {
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
		} else if deadline, hasDeadline := tx.Statement.Context.Deadline(); hasDeadline {
			mutationDeadlines = append(mutationDeadlines, time.Until(deadline))
		} else {
			mutationDeadlines = append(mutationDeadlines, -1)
		}
		mutationMu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}

	for round := 1; round <= 2; round++ {
		makeIssue040TasksDue(t)
		start := time.Now()
		UpdateTaskBulk()
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("round %d took %v; slow task blocked the healthy follower", round, elapsed)
		}
		waitIssue040Signal(t, slowStarted, "slow request started")
		waitIssue040Signal(t, slowFinished, "slow request observed cancellation")

		var slowSaved, healthySaved model.Task
		if err := model.DB.First(&slowSaved, slowOwner.ID).Error; err != nil {
			t.Fatal(err)
		}
		if err := model.DB.First(&healthySaved, healthyOwner.ID).Error; err != nil {
			t.Fatal(err)
		}
		if slowSaved.ProviderState != model.TaskProviderStateAccepted {
			t.Fatalf("round %d slow owner state=%s, want accepted", round, slowSaved.ProviderState)
		}
		if slowSaved.NextActionAt <= time.Now().Unix()+10 {
			t.Fatalf("round %d slow task was not rescheduled with a live mutation context: next_action_at=%d", round, slowSaved.NextActionAt)
		}
		if healthyCalls.Load() != int32(round) || slowCalls.Load() != int32(round) {
			t.Fatalf("round %d provider calls slow=%d healthy=%d, want %d each", round, slowCalls.Load(), healthyCalls.Load(), round)
		}
		if round == 1 {
			if healthySaved.ProviderState != model.TaskProviderStateAccepted || healthySaved.Status != model.TaskStatusInProgress {
				t.Fatalf("healthy task did not persist its first real poll: %+v", healthySaved)
			}
		} else {
			if healthySaved.ProviderState != model.TaskProviderStateClosed || healthySaved.Status != model.TaskStatusSuccess || healthySaved.NextActionAt != 0 {
				t.Fatalf("healthy task did not terminate after the second real poll: %+v", healthySaved)
			}
			if healthySaved.ChargedQuota == nil || *healthySaved.ChargedQuota != 0 || healthySaved.SettlementDecision != "cancel" {
				t.Fatalf("healthy task settlement=%+v, want one cancel/0 settlement", healthySaved)
			}
		}
	}

	mutationMu.Lock()
	deadlines := append([]time.Duration(nil), mutationDeadlines...)
	mutationMu.Unlock()
	if len(deadlines) < 3 {
		t.Fatalf("poll mutation callback count=%d, want snapshot/reschedule and finalization writes", len(deadlines))
	}
	for _, remaining := range deadlines {
		if remaining <= 0 || remaining > 5*time.Second {
			t.Fatalf("poll mutation deadline remaining=%v, want an independent live <=5s budget", remaining)
		}
	}

	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	var token model.Token
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 900 || token.RemainQuota != 900 {
		t.Fatalf("balances user=%d token=%d, want slow reservation retained and healthy reservation refunded once", user.Quota, token.RemainQuota)
	}

	orderMu.Lock()
	gotOrder := append([]string(nil), requestOrder...)
	orderMu.Unlock()
	wantOrder := []string{"slow", "healthy", "slow", "healthy"}
	if fmt.Sprint(gotOrder) != fmt.Sprint(wantOrder) {
		t.Fatalf("same-channel polling order=%v, want slow then healthy in both rounds", gotOrder)
	}

	UpdateTaskBulk()
	if slowCalls.Load() != 2 || healthyCalls.Load() != 2 {
		t.Fatalf("closed/future owners triggered another poll: slow=%d healthy=%d", slowCalls.Load(), healthyCalls.Load())
	}
}

func TestIssue040KlingChannelLookupUsesBoundedContext(t *testing.T) {
	_, _ = taskSubmitFixture(t)
	closeIssue040Database(t)

	var lookups atomic.Int32
	var remaining atomic.Int64
	if err := model.DB.Callback().Query().Before("gorm:query").Register("issue040:kling-channel-query-context", func(tx *gorm.DB) {
		if tx.Statement.Table != "channels" {
			return
		}
		lookups.Add(1)
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
	UpdateTaskByPlatform(context.Background(), model.TaskPlatformKling, map[int][]string{1: {"bounded"}}, map[string]*model.Task{})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Kling channel lookup exceeded bounded budget: %v", elapsed)
	}
	if lookups.Load() != 1 {
		t.Fatalf("Kling channel lookup calls=%d, want 1", lookups.Load())
	}
	if got := time.Duration(remaining.Load()); got <= 0 || got > 5*time.Second {
		t.Fatalf("Kling channel lookup deadline remaining=%v, want an independent live <=5s deadline", got)
	}
}

func TestIssue040PreCanceledKlingPollSkipsChannelLookup(t *testing.T) {
	_, _ = taskSubmitFixture(t)
	closeIssue040Database(t)

	var channelReads atomic.Int32
	if err := model.DB.Callback().Query().Before("gorm:query").Register("issue040:kling-pre-canceled-channel-read", func(tx *gorm.DB) {
		if tx.Statement.Table == "channels" {
			channelReads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	UpdateTaskByPlatform(ctx, model.TaskPlatformKling, map[int][]string{1: {"pre-canceled"}}, map[string]*model.Task{})
	if channelReads.Load() != 0 {
		t.Fatalf("pre-canceled Kling poll performed %d channel reads", channelReads.Load())
	}
}

func TestIssue040CanceledKlingPollStopsBeforeStartingNextTask(t *testing.T) {
	_, _ = taskSubmitFixture(t)
	closeIssue040Database(t)

	var calls atomic.Int32
	var cancelPoll context.CancelFunc
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if cancelPoll != nil {
			cancelPoll()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	oldHTTPClient := requester.HTTPClient
	t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })
	requester.HTTPClient = server.Client()
	if err := model.DB.Model(&model.Channel{}).Where("id = ?", 1).Updates(map[string]any{
		"base_url": server.URL,
		"proxy":    "",
	}).Error; err != nil {
		t.Fatal(err)
	}

	first := createIssue040AcceptedOwner(t, "cancel-first")
	second := createIssue040AcceptedOwner(t, "cancel-second")
	var channelReads atomic.Int32
	if err := model.DB.Callback().Query().Before("gorm:query").Register("issue040:kling-canceled-channel-read", func(tx *gorm.DB) {
		if tx.Statement.Table == "channels" {
			channelReads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancelPoll = cancel
	UpdateTaskByPlatform(ctx, model.TaskPlatformKling, map[int][]string{1: {model.TaskProviderID(first), model.TaskProviderID(second)}}, map[string]*model.Task{
		model.TaskProviderID(first):  first,
		model.TaskProviderID(second): second,
	})
	if calls.Load() != 1 {
		t.Fatalf("parent cancellation started %d provider queries, want only the first", calls.Load())
	}
	if channelReads.Load() != 1 {
		t.Fatalf("in-flight cancellation performed %d channel reads, want the initial lookup only", channelReads.Load())
	}
}

func createIssue040AcceptedOwner(t *testing.T, providerTaskID string) *model.Task {
	t.Helper()
	task := &model.Task{
		Platform:                     model.TaskPlatformKling,
		UserId:                       1,
		TokenID:                      1,
		ChannelId:                    1,
		Action:                       "text2video",
		ReservedQuota:                100,
		ProviderNamespace:            "task-platform:kling",
		ProviderTaskScopeIncarnation: "provider-wide",
		RequestFingerprint:           uuid.NewString(),
	}
	if result, err := model.CreateTaskBillingOwner(context.Background(), task); err != nil || result.Outcome != model.BillingBalanceCommitted {
		t.Fatalf("create owner result=%+v err=%v", result, err)
	}
	if result, err := model.ClaimTaskSubmission(context.Background(), task, uuid.NewString()); err != nil || result.Outcome != model.TaskMutationApplied {
		t.Fatalf("claim owner result=%+v err=%v", result, err)
	}
	if result, err := model.AcceptTaskSubmission(context.Background(), task, providerTaskID); err != nil || result.Outcome != model.TaskMutationApplied {
		t.Fatalf("accept owner result=%+v err=%v", result, err)
	}
	return task
}

func closeIssue040Database(t *testing.T) {
	t.Helper()
	sqlDB, err := model.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
}

func makeIssue040TasksDue(t *testing.T) {
	t.Helper()
	if err := model.DB.Model(&model.Task{}).
		Where("provider_state = ?", model.TaskProviderStateAccepted).
		Update("next_action_at", time.Now().Unix()-1).Error; err != nil {
		t.Fatal(err)
	}
}

func waitIssue040Signal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}
