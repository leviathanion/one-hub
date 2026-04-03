package channelaffinity

import (
	"testing"
	"time"
)

func TestManagerSetGetDelete(t *testing.T) {
	manager := NewManager(time.Minute, 0)

	manager.Set("responses/key-a", 42, time.Minute)
	record, ok := manager.Get("responses/key-a")
	if !ok {
		t.Fatal("expected affinity record to be present")
	}
	if record.ChannelID != 42 {
		t.Fatalf("expected channel id 42, got %d", record.ChannelID)
	}

	manager.Delete("responses/key-a")
	if _, ok := manager.Get("responses/key-a"); ok {
		t.Fatal("expected affinity record to be removed")
	}
}

func TestManagerSetRecordPreservesResumeFingerprint(t *testing.T) {
	manager := NewManager(time.Minute, 0)

	manager.SetRecord("responses/key-fingerprint", Record{
		ChannelID:         77,
		ResumeFingerprint: "model:gpt-5",
	}, time.Minute)

	record, ok := manager.Get("responses/key-fingerprint")
	if !ok {
		t.Fatal("expected affinity record with resume fingerprint to be present")
	}
	if record.ChannelID != 77 {
		t.Fatalf("expected channel id 77, got %d", record.ChannelID)
	}
	if record.ResumeFingerprint != "model:gpt-5" {
		t.Fatalf("expected resume fingerprint to be preserved, got %q", record.ResumeFingerprint)
	}
}

func TestManagerSweepExpiresEntries(t *testing.T) {
	manager := NewManager(time.Minute, 0)

	manager.Set("realtime/key-a", 99, time.Millisecond)
	time.Sleep(5 * time.Millisecond)

	if removed := manager.Sweep(time.Now()); removed != 1 {
		t.Fatalf("expected one expired affinity entry to be removed, got %d", removed)
	}
	if _, ok := manager.Get("realtime/key-a"); ok {
		t.Fatal("expected expired affinity record to be gone")
	}
}

func TestManagerEnforcesMaxEntries(t *testing.T) {
	manager := NewManagerWithOptions(ManagerOptions{
		DefaultTTL: time.Minute,
		MaxEntries: 2,
	})

	manager.Set("responses/key-a", 1, time.Minute)
	manager.Set("responses/key-b", 2, time.Minute)
	manager.Set("responses/key-c", 3, time.Minute)

	stats := manager.Stats()
	if stats.LocalEntries != 2 {
		t.Fatalf("expected local entry count to be capped at 2, got %d", stats.LocalEntries)
	}
	if _, ok := manager.Get("responses/key-a"); ok {
		t.Fatal("expected the oldest affinity entry to be evicted")
	}
}
