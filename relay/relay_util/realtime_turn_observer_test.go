package relay_util

import (
	"context"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/model"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type realtimeTurnObserverContextKey string

func init() {
	logger.Logger = zap.NewNop()
	if model.DB != nil {
		return
	}
	testDB, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		panic(err)
	}
	if err := testDB.AutoMigrate(&model.User{}); err != nil {
		panic(err)
	}
	model.DB = testDB
}

func TestRealtimeTurnObserverUsageTrackingAndFactoryClone(t *testing.T) {
	logger.Logger = zap.NewNop()

	if (*RealtimeTurnObserver)(nil).Quota() != nil {
		t.Fatal("expected nil realtime turn observer quota lookup to return nil")
	}
	if err := (*RealtimeTurnObserver)(nil).ObserveTurnUsage(&types.UsageEvent{TotalTokens: 1}); err != nil {
		t.Fatalf("expected nil realtime turn observer usage tracking to be ignored, got %v", err)
	}

	observer := &RealtimeTurnObserver{quota: &Quota{}}
	if err := observer.ObserveTurnUsage(&types.UsageEvent{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}); err != nil {
		t.Fatalf("expected usage observation to succeed without redis, got %v", err)
	}
	if observer.observedUsage.InputTokens != 2 || observer.observedUsage.TotalTokens != 5 {
		t.Fatalf("expected observed usage snapshot to accumulate, got %+v", observer.observedUsage)
	}

	templateQuota := &Quota{
		modelName:      "gpt-5",
		groupName:      "team-a",
		requestContext: context.WithValue(context.Background(), realtimeTurnObserverContextKey("trace"), "trace-1"),
	}
	factory := NewRealtimeTurnObserverFactory(templateQuota)
	produced, ok := factory().(*RealtimeTurnObserver)
	if !ok || produced == nil || produced.quota == nil {
		t.Fatalf("expected realtime turn observer factory to return a cloned quota observer, got %#v", produced)
	}
	if produced.quota == templateQuota || produced.quota.groupName != "team-a" {
		t.Fatalf("expected realtime turn observer factory to clone quota state, got produced=%+v template=%+v", produced.quota, templateQuota)
	}
	if got := produced.quota.requestContext.Value(realtimeTurnObserverContextKey("trace")); got != "trace-1" {
		t.Fatalf("expected cloned quota context to preserve values, got %#v", got)
	}

	if nilFactoryObserver := NewRealtimeTurnObserverFactory(nil)(); nilFactoryObserver == nil {
		t.Fatal("expected realtime turn observer factory to still return an observer when quota template is nil")
	}
}

func TestRealtimeTurnObserverFinalizeSeedsTimingWithoutPanicking(t *testing.T) {
	logger.Logger = zap.NewNop()

	originalBatch := config.BatchUpdateEnabled
	config.BatchUpdateEnabled = true
	t.Cleanup(func() {
		config.BatchUpdateEnabled = originalBatch
	})

	startedAt := time.Unix(1700000000, 0)
	firstResponseAt := startedAt.Add(200 * time.Millisecond)
	completedAt := startedAt.Add(2 * time.Second)

	var nilObserver *RealtimeTurnObserver
	nilObserver.FinalizeTurn(runtimesession.TurnFinalizePayload{})

	observer := &RealtimeTurnObserver{
		quota: &Quota{},
		observedUsage: types.UsageEvent{
			InputTokens:  2,
			OutputTokens: 3,
			TotalTokens:  5,
		},
	}
	observer.FinalizeTurn(runtimesession.TurnFinalizePayload{
		StartedAt:       startedAt,
		FirstResponseAt: firstResponseAt,
		CompletedAt:     completedAt,
	})
	if observer.quota.startTime != startedAt || observer.quota.firstResponseTime != firstResponseAt {
		t.Fatalf("expected finalize to seed managed timing into quota, got %+v", observer.quota)
	}
	if !observer.quota.requestFrozen || observer.quota.requestDuration != completedAt.Sub(startedAt) {
		t.Fatalf("expected finalize to freeze request timing, got duration=%v frozen=%v", observer.quota.requestDuration, observer.quota.requestFrozen)
	}

	observer.FinalizeTurn(runtimesession.TurnFinalizePayload{
		StartedAt:       startedAt,
		FirstResponseAt: firstResponseAt,
		CompletedAt:     completedAt,
		Usage: &types.UsageEvent{
			InputTokens:  4,
			OutputTokens: 6,
			TotalTokens:  10,
		},
	})
	if observer.quota.GetFirstResponseTime() != firstResponseAt.Sub(startedAt).Milliseconds() {
		t.Fatalf("expected finalize to preserve first response timing, got %d", observer.quota.GetFirstResponseTime())
	}
}
