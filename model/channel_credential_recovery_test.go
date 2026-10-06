package model

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/credentials"

	"gorm.io/gorm"
)

func seedCredentialRecovery(t *testing.T, channelID int) (credentials.Ticket, credentials.Snapshot) {
	t.Helper()
	insertCredentialRotationChannel(t, channelID, "old")
	ticket := credentials.Ticket{ChannelID: channelID, Type: config.ChannelTypeCodex, AttemptID: "Unresolved-Refresh"}
	if outcome, err := testRotation().Claim(t.Context(), ticket, time.Now()); err != nil || outcome != credentials.ClaimAcquired {
		t.Fatalf("claim unresolved refresh: outcome=%v err=%v", outcome, err)
	}
	snapshot, err := testRotation().Store.Load(t.Context(), channelID)
	if err != nil {
		t.Fatal(err)
	}
	return ticket, snapshot
}

func TestCredentialRecoveryRejectsChangedSnapshotAcrossDatabases(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		setupCredentialDatabase(t, db)
		for index, scenario := range []string{"version", "fence", "fence-case", "type", "account", "deleted", "empty-attempt"} {
			t.Run(scenario, func(t *testing.T) {
				ticket, expected := seedCredentialRecovery(t, 33100+index)
				// All channel edits advance Version under the current row CAS contract.
				// Account validation of newly authorized tokens belongs to the Codex
				// controller and is covered by its OAuth recovery tests.
				updates := map[string]any{"version": expected.Version + 1}
				switch scenario {
				case "fence", "fence-case":
					attempt := "another-attempt"
					if scenario == "fence-case" {
						attempt = "unresolved-refresh"
					}
					data, err := credentials.WriteRefresh(expected.InternalState, &credentials.Refresh{AttemptID: attempt, StartedAt: time.Now().Unix()})
					if err != nil {
						t.Fatal(err)
					}
					updates["internal_state"] = string(data)
				case "type":
					updates["type"] = config.ChannelTypeOpenAI
				case "account":
					updates["key"] = `{"access_token":"other","account_id":"account-b"}`
				case "deleted":
					// Soft-deleted rows must be excluded even if the version matches.
					updates = map[string]any{"deleted_at": time.Now()}
				case "empty-attempt":
					updates = nil
					ticket.AttemptID = ""
				}
				if len(updates) > 0 {
					if err := db.Model(&Channel{}).Where("id = ?", expected.ChannelID).Updates(updates).Error; err != nil {
						t.Fatal(err)
					}
				}
				before, err := testRotation().Store.Load(t.Context(), expected.ChannelID)
				if err != nil {
					t.Fatal(err)
				}
				if err := testRotation().Recover(t.Context(), expected, ticket.AttemptID, "authorized"); !errors.Is(err, credentials.ErrConflict) {
					t.Fatalf("stale or invalid recovery must conflict: %v", err)
				}
				after, err := testRotation().Store.Load(t.Context(), expected.ChannelID)
				if err != nil || !reflect.DeepEqual(after, before) {
					t.Fatalf("rejected recovery changed credential snapshot: err=%v", err)
				}
			})
		}
	})
}

func TestConcurrentCredentialRecoveryHasOneWinnerAcrossDatabases(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		setupCredentialDatabase(t, db)
		ticket, expected := seedCredentialRecovery(t, 33200)
		start := make(chan struct{})
		type result struct {
			key string
			err error
		}
		results := make(chan result, 2)
		for i := 0; i < 2; i++ {
			go func(i int) {
				key := fmt.Sprintf("authorized-%d", i)
				<-start
				results <- result{key, testRotation().Recover(t.Context(), expected, ticket.AttemptID, key)}
			}(i)
		}
		close(start)
		winner, successes := "", 0
		for i := 0; i < 2; i++ {
			result := <-results
			if result.err == nil {
				winner = result.key
				successes++
			} else if !errors.Is(result.err, credentials.ErrConflict) {
				t.Errorf("losing recovery must report a CAS conflict: %v", result.err)
			}
		}
		after, err := loadRotationTestSnapshot(t.Context(), expected.ChannelID)
		if err != nil || successes != 1 || after.Key != winner || after.Version != expected.Version+1 || after.Fence != nil || after.StartedAt != nil || after.Deleted {
			t.Fatalf("concurrent recovery must commit exactly one credential: successes=%d version=%d fenced=%v err=%v", successes, after.Version, after.Fence != nil, err)
		}
	})
}

func TestCredentialRecoveryWriteFailureKeepsFence(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		setupCredentialDatabase(t, db)
		ticket, before := seedCredentialRecovery(t, 33300)
		writeErr := errors.New("injected recovery SQL failure")
		const callbackName = "credential_recovery:write_failure"
		if err := db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) { tx.AddError(writeErr) }); err != nil {
			t.Fatal(err)
		}
		defer db.Callback().Update().Remove(callbackName)
		if err := testRotation().Recover(t.Context(), before, ticket.AttemptID, "authorized"); !errors.Is(err, writeErr) {
			t.Fatalf("recovery must return its SQL write failure: %v", err)
		}
		after, err := testRotation().Store.Load(t.Context(), before.ChannelID)
		if err != nil || !reflect.DeepEqual(after, before) {
			t.Fatalf("failed recovery changed credential or durable fence: err=%v", err)
		}
	})
}
