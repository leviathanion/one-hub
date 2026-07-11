package codex

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func deletePendingCredentialTestEntries(ids ...int) {
	pendingCredentialCommits.Lock()
	defer pendingCredentialCommits.Unlock()
	for _, id := range ids {
		delete(pendingCredentialCommits.items, id)
	}
}

func TestPendingCredentialsNeverExpireByElapsedTime(t *testing.T) {
	const channelID = 99801
	deletePendingCredentialTestEntries(channelID)
	t.Cleanup(func() { deletePendingCredentialTestEntries(channelID) })

	if err := rememberPendingCredentialCommit(channelID, "old", "rotated"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	pending, ok := getPendingCredentialCommit(channelID)
	if !ok || pending.expectedKey != "old" || pending.rotatedKey != "rotated" {
		t.Fatalf("elapsed time discarded recovery journal: %+v exists=%v", pending, ok)
	}
}

func TestPendingCredentialRejectsDifferentReplacement(t *testing.T) {
	const channelID = 99802
	deletePendingCredentialTestEntries(channelID)
	t.Cleanup(func() { deletePendingCredentialTestEntries(channelID) })

	if err := rememberPendingCredentialCommit(channelID, "old", "rotated-a"); err != nil {
		t.Fatal(err)
	}
	if err := rememberPendingCredentialCommit(channelID, "old", "rotated-a"); err != nil {
		t.Fatalf("idempotent journal write failed: %v", err)
	}
	if err := rememberPendingCredentialCommit(channelID, "old", "rotated-b"); err == nil {
		t.Fatal("different rotation overwrote unresolved journal entry")
	}
	pending, _ := getPendingCredentialCommit(channelID)
	if pending.rotatedKey != "rotated-a" {
		t.Fatalf("valid pending rotation was replaced: %+v", pending)
	}
}

func TestPendingCredentialClearRequiresMatchingRotation(t *testing.T) {
	const channelID = 99803
	deletePendingCredentialTestEntries(channelID)
	t.Cleanup(func() { deletePendingCredentialTestEntries(channelID) })

	if err := rememberPendingCredentialCommit(channelID, "old", "rotated"); err != nil {
		t.Fatal(err)
	}
	clearPendingCredentialCommit(channelID, "stale")
	if _, ok := getPendingCredentialCommit(channelID); !ok {
		t.Fatal("stale completion removed current journal entry")
	}
	clearPendingCredentialCommit(channelID, "rotated")
	if _, ok := getPendingCredentialCommit(channelID); ok {
		t.Fatal("matching completion did not clear journal entry")
	}
}

func TestPendingCredentialJournalIsRaceSafeAcrossChannels(t *testing.T) {
	const firstID = 99810
	const workers = 8
	ids := make([]int, workers)
	for i := range ids {
		ids[i] = firstID + i
	}
	deletePendingCredentialTestEntries(ids...)
	t.Cleanup(func() { deletePendingCredentialTestEntries(ids...) })

	var wg sync.WaitGroup
	for worker, id := range ids {
		wg.Add(1)
		go func(id, worker int) {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				expected := fmt.Sprintf("expected-%d-%d", worker, n)
				rotated := fmt.Sprintf("rotated-%d-%d", worker, n)
				if err := rememberPendingCredentialCommit(id, expected, rotated); err == nil {
					clearPendingCredentialCommit(id, rotated)
				}
			}
		}(id, worker)
	}
	wg.Wait()
}
