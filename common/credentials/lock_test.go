package credentials

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAcquireChannelRefreshLockCleansUpUnusedEntry(t *testing.T) {
	channelID := 424244

	channelRefreshLocks.mu.Lock()
	delete(channelRefreshLocks.locks, channelID)
	channelRefreshLocks.mu.Unlock()

	release, err := AcquireChannelRefreshLock(context.Background(), channelID)
	if err != nil {
		t.Fatalf("expected channel refresh lock acquisition to succeed, got %v", err)
	}

	channelRefreshLocks.mu.Lock()
	if _, ok := channelRefreshLocks.locks[channelID]; !ok {
		channelRefreshLocks.mu.Unlock()
		t.Fatalf("expected lock entry to exist while held")
	}
	channelRefreshLocks.mu.Unlock()

	release()

	channelRefreshLocks.mu.Lock()
	_, ok := channelRefreshLocks.locks[channelID]
	channelRefreshLocks.mu.Unlock()
	if ok {
		t.Fatalf("expected lock entry to be cleaned up after release")
	}
}

func TestAcquireChannelRefreshLockHonorsCancellationWhileWaiting(t *testing.T) {
	channelID := 424246

	channelRefreshLocks.mu.Lock()
	delete(channelRefreshLocks.locks, channelID)
	channelRefreshLocks.mu.Unlock()

	release, err := AcquireChannelRefreshLock(context.Background(), channelID)
	if err != nil {
		t.Fatalf("expected first lock acquisition to succeed, got %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if waitingRelease, waitErr := AcquireChannelRefreshLock(ctx, channelID); !errors.Is(waitErr, context.DeadlineExceeded) {
		if waitingRelease != nil {
			waitingRelease()
		}
		release()
		t.Fatalf("expected waiting acquisition to honor its deadline, got %v", waitErr)
	}

	release()
	channelRefreshLocks.mu.Lock()
	_, ok := channelRefreshLocks.locks[channelID]
	channelRefreshLocks.mu.Unlock()
	if ok {
		t.Fatal("expected canceled waiter and released holder to clean up lock entry")
	}
}
