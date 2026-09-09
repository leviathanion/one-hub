package suno

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestIssue040SunoBatchPollHasIndependentBoundedQueryContext(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	finished := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		finished <- struct{}{}
	}))
	t.Cleanup(server.Close)
	db, channel, newOwner := sunoIssue040Fixture(t, server)
	owner := newOwner("slow")

	originalDeadline := sunoTaskPollDeadline
	sunoTaskPollDeadline = 40 * time.Millisecond
	t.Cleanup(func() { sunoTaskPollDeadline = originalDeadline })
	start := time.Now()
	err := updateSunoTaskAll(context.Background(), channel.Id, []string{"slow"}, map[string]*model.Task{"slow": owner})
	if err == nil {
		t.Fatal("slow Suno batch poll unexpectedly succeeded")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Suno batch poll exceeded bounded query budget: %v", elapsed)
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("slow Suno endpoint did not finish after the bounded query returned")
	}
	if calls.Load() != 1 {
		t.Fatalf("Suno batch provider calls=%d, want 1", calls.Load())
	}
	var saved model.Task
	if err := db.First(&saved, owner.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.ProviderState != model.TaskProviderStateAccepted || saved.ChargedQuota != nil {
		t.Fatalf("timed out poll changed owner settlement: %+v", saved)
	}
}

func TestIssue040SunoBatchChannelLookupUsesBoundedQueryContext(t *testing.T) {
	var providerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
	}))
	t.Cleanup(server.Close)
	db, channel, newOwner := sunoIssue040Fixture(t, server)
	owner := newOwner("channel-lookup")

	originalDeadline := sunoTaskPollDeadline
	sunoTaskPollDeadline = 40 * time.Millisecond
	t.Cleanup(func() { sunoTaskPollDeadline = originalDeadline })
	var lookupCalls atomic.Int32
	var remaining atomic.Int64
	if err := db.Callback().Query().Before("gorm:query").Register("issue040:suno-channel-query-context", func(tx *gorm.DB) {
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
	if err := updateSunoTaskAll(context.Background(), channel.Id, []string{"channel-lookup"}, map[string]*model.Task{"channel-lookup": owner}); err == nil {
		t.Fatal("channel lookup failure unexpectedly succeeded")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Suno channel lookup exceeded bounded budget: %v", elapsed)
	}
	if lookupCalls.Load() != 1 || providerCalls.Load() != 0 {
		t.Fatalf("Suno channel lookup calls=%d provider calls=%d, want one SQL lookup and no HTTP", lookupCalls.Load(), providerCalls.Load())
	}
	if got := time.Duration(remaining.Load()); got <= 0 || got > 200*time.Millisecond {
		t.Fatalf("Suno channel lookup deadline remaining=%v, want a short bounded deadline", got)
	}
}

func TestIssue040SunoPollMutationUsesIndependentBoundedContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"code":"success","data":[{"task_id":"terminal","status":"SUCCESS","submit_time":1,"start_time":2,"finish_time":3,"data":{"url":"https://example.invalid/song.mp3"}},{"task_id":"progress","status":"IN_PROGRESS","submit_time":1,"start_time":2,"data":{"progress":50}}]}`)
	}))
	t.Cleanup(server.Close)
	db, channel, newOwner := sunoIssue040Fixture(t, server)
	terminal, progress := newOwner("terminal"), newOwner("progress")

	var mutationMu sync.Mutex
	var mutationDeadlines []time.Duration
	if err := db.Callback().Update().Before("gorm:update").Register("issue040:suno-mutation-context", func(tx *gorm.DB) {
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

	if err := updateSunoTaskAll(context.Background(), channel.Id, []string{"terminal", "progress"}, map[string]*model.Task{
		"terminal": terminal,
		"progress": progress,
	}); err != nil {
		t.Fatal(err)
	}
	var terminalSaved, progressSaved model.Task
	if err := db.First(&terminalSaved, terminal.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&progressSaved, progress.ID).Error; err != nil {
		t.Fatal(err)
	}
	if terminalSaved.ProviderState != model.TaskProviderStateClosed || terminalSaved.Status != model.TaskStatusSuccess || terminalSaved.ChargedQuota == nil || *terminalSaved.ChargedQuota != 0 {
		t.Fatalf("Suno terminal owner=%+v", terminalSaved)
	}
	if progressSaved.ProviderState != model.TaskProviderStateAccepted || progressSaved.Status != model.TaskStatusInProgress || progressSaved.NextActionAt <= time.Now().Unix() {
		t.Fatalf("Suno progress owner=%+v", progressSaved)
	}

	mutationMu.Lock()
	deadlines := append([]time.Duration(nil), mutationDeadlines...)
	mutationMu.Unlock()
	if len(deadlines) != 2 {
		t.Fatalf("Suno mutation callback count=%d, want terminal+snapshot", len(deadlines))
	}
	for _, remaining := range deadlines {
		if remaining <= 0 || remaining > 5*time.Second {
			t.Fatalf("Suno mutation deadline remaining=%v, want independent live <=5s", remaining)
		}
	}
	var user model.User
	if err := db.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 900 {
		t.Fatalf("Suno poll settlement changed user quota=%d, want 900", user.Quota)
	}
}

func TestIssue040CanceledSunoBatchPollDoesNotStartProviderWork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	t.Cleanup(server.Close)
	db, channel, newOwner := sunoIssue040Fixture(t, server)
	owner := newOwner("canceled")
	var channelReads atomic.Int32
	if err := db.Callback().Query().Before("gorm:query").Register("issue040:suno-canceled-channel-read", func(tx *gorm.DB) {
		if tx.Statement.Table == "channels" {
			channelReads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := updateSunoTaskAll(ctx, channel.Id, []string{"canceled"}, map[string]*model.Task{"canceled": owner}); err == nil {
		t.Fatal("canceled Suno poll unexpectedly succeeded")
	}
	if calls.Load() != 0 {
		t.Fatalf("canceled Suno poll started %d provider calls", calls.Load())
	}
	if channelReads.Load() != 0 {
		t.Fatalf("canceled Suno poll performed %d channel reads", channelReads.Load())
	}
}

func sunoIssue040Fixture(t *testing.T, server *httptest.Server) (*gorm.DB, *model.Channel, func(string) *model.Task) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&model.Channel{}, &model.Task{}, &model.User{}, &model.Token{}, &model.UserGroup{}); err != nil {
		t.Fatal(err)
	}
	originalDB, originalClient := model.DB, requester.HTTPClient
	model.DB, requester.HTTPClient = db, server.Client()
	t.Cleanup(func() { model.DB, requester.HTTPClient = originalDB, originalClient })
	proxy := ""
	channel := &model.Channel{Id: 1, Type: config.ChannelTypeSuno, Name: "suno", Key: "owner-secret", Status: config.ChannelStatusEnabled, BaseURL: &server.URL, Proxy: &proxy, Other: `{}`}
	rows := []any{
		channel,
		&model.User{Id: 1, Username: "poll-user", Password: "password123", AccessToken: "access", Quota: 1000, Status: config.UserStatusEnabled},
		&model.Token{Id: 1, UserId: 1, Key: "poll-token", Status: config.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000},
	}
	for _, row := range rows {
		if err := db.Session(&gorm.Session{SkipHooks: true}).Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db, channel, func(providerTaskID string) *model.Task {
		t.Helper()
		owner := &model.Task{Platform: model.TaskPlatformSuno, UserId: 1, TokenID: 1, ChannelId: channel.Id, Action: "MUSIC", Status: model.TaskStatusSubmitted, ReservedQuota: 100, ProviderNamespace: "task-platform:suno", ProviderTaskScopeIncarnation: "provider-wide", RequestFingerprint: uuid.NewString()}
		if _, err := model.CreateTaskBillingOwner(context.Background(), owner); err != nil {
			t.Fatal(err)
		}
		if _, err := model.ClaimTaskSubmission(context.Background(), owner, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		if _, err := model.AcceptTaskSubmission(context.Background(), owner, providerTaskID); err != nil {
			t.Fatal(err)
		}
		return owner
	}
}
