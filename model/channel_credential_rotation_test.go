package model

import (
	"context"
	"errors"
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

	first := CredentialRotationTicket{ChannelID: 31001, AttemptID: "attempt-a", ExpectedRevision: 0}
	second := CredentialRotationTicket{ChannelID: 31001, AttemptID: "attempt-b", ExpectedRevision: 0}
	if outcome, err := ClaimCredentialRotation(context.Background(), first, time.Now()); err != nil || outcome != CredentialRotationClaimAcquired {
		t.Fatalf("first claim = %v, %v", outcome, err)
	}
	if outcome, err := ClaimCredentialRotation(context.Background(), second, time.Now()); err != nil || outcome != CredentialRotationClaimBusy {
		t.Fatalf("second claim = %v, %v", outcome, err)
	}
	if canceled, err := CancelCredentialRotationBeforeDispatch(context.Background(), second); err != nil || canceled {
		t.Fatalf("stale cancel = %v, %v", canceled, err)
	}
	if outcome, err := CommitCredentialRotation(context.Background(), first, "rotated"); err != nil || outcome != CredentialRotationCommitApplied {
		t.Fatalf("commit = %v, %v", outcome, err)
	}
	if outcome, err := CommitCredentialRotation(context.Background(), first, "rotated"); err != nil || outcome != CredentialRotationCommitAlreadyApplied {
		t.Fatalf("commit replay classification = %v, %v", outcome, err)
	}

	snapshot, err := LoadCredentialRotationSnapshot(context.Background(), 31001)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Key != "rotated" || snapshot.Revision != 1 || snapshot.Fence != nil {
		t.Fatalf("unexpected committed snapshot: %+v", snapshot)
	}
}

func TestCredentialReplacementRequiresNewBytesToResolveFence(t *testing.T) {
	useTestChannelDB(t)
	insertCredentialRotationChannel(t, 31002, "old")
	ticket := CredentialRotationTicket{ChannelID: 31002, AttemptID: "attempt-a", ExpectedRevision: 0}
	if _, err := ClaimCredentialRotation(context.Background(), ticket, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplaceChannelCredentialWithContext(context.Background(), 31002, "old"); !errors.Is(err, ErrSameCredentialCannotResolveRefreshFence) {
		t.Fatalf("same credential replacement error = %v", err)
	}
	if updated, err := ReplaceChannelCredentialWithContext(context.Background(), 31002, "manual-new"); err != nil || !updated {
		t.Fatalf("new credential replacement = %v, %v", updated, err)
	}
	if outcome, err := CommitCredentialRotation(context.Background(), ticket, "stale-rotated"); err != nil || outcome != CredentialRotationCommitSuperseded {
		t.Fatalf("stale commit = %v, %v", outcome, err)
	}
}

func TestSoftDeleteSupersedesCredentialRotationWithoutClearingFence(t *testing.T) {
	useTestChannelDB(t)
	insertCredentialRotationChannel(t, 31003, "old")
	ticket := CredentialRotationTicket{ChannelID: 31003, AttemptID: "attempt-a", ExpectedRevision: 0}
	if _, err := ClaimCredentialRotation(context.Background(), ticket, time.Now()); err != nil {
		t.Fatal(err)
	}
	channel := Channel{Id: 31003}
	if err := channel.Delete(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadCredentialRotationSnapshot(context.Background(), 31003)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Deleted || snapshot.Revision != 1 || snapshot.Fence == nil || *snapshot.Fence != ticket.AttemptID {
		t.Fatalf("unexpected deleted snapshot: %+v", snapshot)
	}
	if outcome, err := CommitCredentialRotation(context.Background(), ticket, "rotated"); err != nil || outcome != CredentialRotationCommitSuperseded {
		t.Fatalf("post-delete commit = %v, %v", outcome, err)
	}
	if _, err := RestoreChannelWithCredentialWithContext(context.Background(), 31003, "old"); !errors.Is(err, ErrSameCredentialCannotResolveRefreshFence) {
		t.Fatalf("restore with old credential error = %v", err)
	}
	if restored, err := RestoreChannelWithCredentialWithContext(context.Background(), 31003, "restored-new"); err != nil || !restored {
		t.Fatalf("restore with new credential = %v, %v", restored, err)
	}
	restored, err := LoadCredentialRotationSnapshot(context.Background(), 31003)
	if err != nil || restored.Deleted || restored.Revision != 2 || restored.Fence != nil || restored.Key != "restored-new" {
		t.Fatalf("unexpected restored snapshot: %+v err=%v", restored, err)
	}
}
