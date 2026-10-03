package model

import (
	"context"
	"one-api/common/credentials"
	"testing"
	"time"

	"one-api/common/config"
)

func insertCredentialRotationChannel(t *testing.T, id int, key string) {
	t.Helper()
	channel := Channel{Id: id, Type: config.ChannelTypeCodex, Key: key, Status: config.ChannelStatusEnabled, Name: "fence-test", Models: "gpt-5", Group: "default"}
	if err := DB.Create(&channel).Error; err != nil {
		t.Fatalf("insert channel: %v", err)
	}
}

func TestCredentialRotationFenceStateMachine(t *testing.T) {
	useTestChannelDB(t)
	insertCredentialRotationChannel(t, 31001, "old")

	first := credentials.Ticket{Type: config.ChannelTypeCodex, ChannelID: 31001, AttemptID: "attempt-a", ExpectedVersion: 0}
	second := credentials.Ticket{Type: config.ChannelTypeCodex, ChannelID: 31001, AttemptID: "attempt-b", ExpectedVersion: 0}
	if outcome, err := testRotation().Claim(context.Background(), first, time.Now()); err != nil || outcome != credentials.ClaimAcquired {
		t.Fatalf("first claim = %v, %v", outcome, err)
	}
	if outcome, err := testRotation().Claim(context.Background(), second, time.Now()); err != nil || outcome != credentials.ClaimBusy {
		t.Fatalf("second claim = %v, %v", outcome, err)
	}
	if canceled, err := testRotation().Cancel(context.Background(), second); err != nil || canceled {
		t.Fatalf("stale cancel = %v, %v", canceled, err)
	}
	if canceled, err := testRotation().Cancel(context.Background(), first); err != nil || !canceled {
		t.Fatalf("owner cancel = %v, %v", canceled, err)
	}
	if canceled, err := testRotation().Cancel(context.Background(), first); err != nil || canceled {
		t.Fatalf("already canceled owner = %v, %v", canceled, err)
	}
	first.ExpectedVersion = 2
	first.AttemptID = "attempt-c"
	if outcome, err := testRotation().Claim(context.Background(), first, time.Now()); err != nil || outcome != credentials.ClaimAcquired {
		t.Fatalf("reclaim after pre-dispatch cancel = %v, %v", outcome, err)
	}
	if outcome, err := testRotation().Commit(context.Background(), first, "rotated"); err != nil || outcome != credentials.CommitApplied {
		t.Fatalf("commit = %v, %v", outcome, err)
	}
	if outcome, err := testRotation().Commit(context.Background(), first, "rotated"); err != nil || outcome != credentials.CommitAlreadyApplied {
		t.Fatalf("commit replay classification = %v, %v", outcome, err)
	}

	snapshot, err := loadRotationTestSnapshot(context.Background(), 31001)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Key != "rotated" || snapshot.Version != 4 || snapshot.Fence != nil {
		t.Fatalf("unexpected committed snapshot: %+v", snapshot)
	}
}

func TestSoftDeleteSupersedesCredentialRotationWithoutClearingFence(t *testing.T) {
	useTestChannelDB(t)
	insertCredentialRotationChannel(t, 31003, "old")
	ticket := credentials.Ticket{Type: config.ChannelTypeCodex, ChannelID: 31003, AttemptID: "attempt-a", ExpectedVersion: 0}
	if _, err := testRotation().Claim(context.Background(), ticket, time.Now()); err != nil {
		t.Fatal(err)
	}
	channel := Channel{Id: 31003}
	if err := channel.Delete(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := loadRotationTestSnapshot(context.Background(), 31003)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Deleted || snapshot.Version != 2 || snapshot.Fence == nil || *snapshot.Fence != ticket.AttemptID {
		t.Fatalf("unexpected deleted snapshot: %+v", snapshot)
	}
	if outcome, err := testRotation().Commit(context.Background(), ticket, "rotated"); err != nil || outcome != credentials.CommitSuperseded {
		t.Fatalf("post-delete commit = %v, %v", outcome, err)
	}
	insertCredentialRotationChannel(t, 31004, "new-account-key")
	original, err := loadRotationTestSnapshot(context.Background(), 31003)
	if err != nil || !original.Deleted || original.Key != "old" {
		t.Fatalf("new channel altered original incarnation: %+v err=%v", original, err)
	}
}
