package model

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"

	"gorm.io/gorm"
)

func TestGeneralDisableRollbackHasNoRoutingOrCacheSideEffects(t *testing.T) {
	useTestChannelDB(t)
	snapshot := snapshotTestChannelGroup(t)
	t.Cleanup(func() { restoreTestChannelGroup(t, snapshot) })

	insertTestChannel(t, &Channel{
		Id: 1, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled,
		Name: "round22-rollback", Key: `{"access_token":"token"}`, Group: "default", Models: "gpt-5",
	})
	requireChannelGroupLoad(t)

	var invalidations atomic.Int32
	originalInvalidate := invalidateChannelCodexDerivedCaches
	invalidateChannelCodexDerivedCaches = func([]int) { invalidations.Add(1) }
	t.Cleanup(func() { invalidateChannelCodexDerivedCaches = originalInvalidate })

	updates := 0
	forcedErr := errors.New("forced zero-value update failure")
	callback := "test:round22_general_disable_rollback"
	if err := DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		updates++
		if updates == 2 {
			tx.AddError(forcedErr)
		}
	}); err != nil {
		t.Fatalf("register update callback: %v", err)
	}
	t.Cleanup(func() { _ = DB.Callback().Update().Remove(callback) })

	err := (&Channel{Id: 1, Status: config.ChannelStatusManuallyDisabled, Other: ""}).UpdateWithOptions(false, ChannelUpdateOptions{OtherSubmitted: true})
	if !errors.Is(err, forcedErr) {
		t.Fatalf("expected transactional update failure, got %v", err)
	}
	var durable Channel
	if err := DB.First(&durable, 1).Error; err != nil || durable.Status != config.ChannelStatusEnabled {
		t.Fatalf("failed update escaped rollback: channel=%+v err=%v", durable, err)
	}
	if invalidations.Load() != 0 {
		t.Fatalf("rollback triggered %d cache invalidations", invalidations.Load())
	}
	if _, err := ChannelGroup.Next("default", "gpt-5"); err != nil {
		t.Fatalf("rollback changed routing snapshot: %v", err)
	}
}

func TestGeneralDisablePublishesDisabledSnapshotBeforeCacheCleanup(t *testing.T) {
	useTestChannelDB(t)
	snapshot := snapshotTestChannelGroup(t)
	t.Cleanup(func() { restoreTestChannelGroup(t, snapshot) })

	insertTestChannel(t, &Channel{
		Id: 1, Type: config.ChannelTypeCodex, Status: config.ChannelStatusEnabled,
		Name: "round22-disable", Key: `{"access_token":"token"}`, Group: "default", Models: "gpt-5",
	})
	requireChannelGroupLoad(t)

	originalInvalidate := invalidateChannelCodexDerivedCaches
	invalidationStarted := make(chan struct{})
	allowInvalidation := make(chan struct{})
	invalidateChannelCodexDerivedCaches = func(ids []int) {
		if len(ids) != 1 || ids[0] != 1 {
			t.Errorf("unexpected invalidation ids: %v", ids)
		}
		close(invalidationStarted)
		<-allowInvalidation
	}
	t.Cleanup(func() { invalidateChannelCodexDerivedCaches = originalInvalidate })

	done := make(chan error, 1)
	go func() {
		done <- (&Channel{Id: 1, Status: config.ChannelStatusManuallyDisabled}).Update(false)
	}()

	select {
	case <-invalidationStarted:
	case <-time.After(time.Second):
		t.Fatal("cache invalidation did not start")
	}

	var durable Channel
	if err := DB.First(&durable, 1).Error; err != nil || durable.Status != config.ChannelStatusManuallyDisabled {
		t.Fatalf("disable was not committed before invalidation: channel=%+v err=%v", durable, err)
	}
	if _, err := ChannelGroup.Next("default", "gpt-5"); err == nil {
		t.Fatal("channel remained routable while post-commit invalidation was blocked")
	}

	close(allowInvalidation)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("disable failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("disable did not finish after invalidation unblocked")
	}
}
