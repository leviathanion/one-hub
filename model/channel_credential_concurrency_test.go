package model

import (
	"context"
	"errors"
	"fmt"
	"one-api/common/credentials"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"

	"gorm.io/gorm"
)

func setupCredentialDatabase(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.AutoMigrate(&Channel{}); err != nil {
		t.Fatal(err)
	}
	oldDB := DB
	oldChooser := snapshotTestChannelGroup(t)
	DB = db
	restoreTestChannelGroup(t, testChannelGroupSnapshot{})
	t.Cleanup(func() { DB = oldDB; restoreTestChannelGroup(t, oldChooser) })
}

func TestCredentialRecoveryInvalidatesOldAttempt(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		setupCredentialDatabase(t, db)
		insertCredentialRotationChannel(t, 33001, "old")
		ticket := credentials.Ticket{ChannelID: 33001, Type: config.ChannelTypeCodex, AttemptID: "Attempt-A"}
		if outcome, err := testRotation().Claim(t.Context(), ticket, time.Now()); err != nil || outcome != credentials.ClaimAcquired {
			t.Fatal(outcome, err)
		}
		snapshot, err := testRotation().Store.Load(t.Context(), 33001)
		if err != nil {
			t.Fatal(err)
		}
		if err := testRotation().Recover(t.Context(), snapshot, "attempt-a", "new"); !errors.Is(err, credentials.ErrConflict) {
			t.Fatal("case-mismatched attempt accepted", err)
		}
		if err := testRotation().Recover(t.Context(), snapshot, ticket.AttemptID, "new"); err != nil {
			t.Fatal(err)
		}
		if err := testRotation().Recover(t.Context(), snapshot, ticket.AttemptID, "other"); !errors.Is(err, credentials.ErrConflict) {
			t.Fatal("stale recovery accepted", err)
		}
		if outcome, err := testRotation().Commit(t.Context(), ticket, "late"); err != nil || outcome != credentials.CommitSuperseded {
			t.Fatal(outcome, err)
		}
		if canceled, err := testRotation().Cancel(t.Context(), ticket); err != nil || canceled {
			t.Fatal(canceled, err)
		}
		if ChannelGroup.publishGeneration.Load() == 0 {
			t.Fatal("credential snapshot was not invalidated")
		}
	})
}

func TestCredentialCommitPreservesConcurrentMetadataAndNamespaces(t *testing.T) {
	useTestChannelDB(t)
	row := Channel{Id: 33002, Type: config.ChannelTypeCodex, Key: "old", InternalState: []byte(`{"vendor":{"x":1},"credentials":{"extension":true}}`)}
	if err := DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	ticket := credentials.Ticket{ChannelID: row.Id, Type: row.Type, AttemptID: "attempt"}
	if _, err := testRotation().Claim(t.Context(), ticket, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := (&Channel{Id: row.Id, Name: "updated"}).UpdateRaw(false); err != nil {
		t.Fatal(err)
	}
	if outcome, err := testRotation().Commit(t.Context(), ticket, "new"); err != nil || outcome != credentials.CommitApplied {
		t.Fatal(outcome, err)
	}
	saved, err := GetChannelById(row.Id)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Key != "new" || saved.Name != "updated" || saved.Version != 3 || string(saved.InternalState) != `{"credentials":{"extension":true},"vendor":{"x":1}}` {
		t.Fatalf("unexpected snapshot: version=%d internal_state=%s", saved.Version, saved.InternalState)
	}
}

func TestConcurrentCredentialClaimsHaveOneWinner(t *testing.T) {
	useTestChannelDB(t)
	insertCredentialRotationChannel(t, 33003, "old")
	var wg sync.WaitGroup
	var winners atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ticket := credentials.Ticket{ChannelID: 33003, Type: config.ChannelTypeCodex, AttemptID: fmt.Sprint(i)}
			outcome, err := testRotation().Claim(context.Background(), ticket, time.Now())
			if err == nil && outcome == credentials.ClaimAcquired {
				winners.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("winners=%d", winners.Load())
	}
}
