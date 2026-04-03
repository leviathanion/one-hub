package channelaffinity

import (
	"testing"
	"time"

	"one-api/internal/testutil/fakeredis"
)

func newRedisBackedManager(t *testing.T, options ManagerOptions) (*Manager, *fakeredis.Server) {
	t.Helper()

	server, err := fakeredis.Start()
	if err != nil {
		t.Fatalf("failed to start fake redis: %v", err)
	}
	t.Cleanup(func() {
		_ = server.Close()
	})

	options.RedisClient = server.Client()
	if options.RedisPrefix == "" {
		options.RedisPrefix = "test:channel-affinity"
	}

	manager := NewManagerWithOptions(options)
	t.Cleanup(func() {
		manager.Close()
	})
	return manager, server
}

func resetDefaultManagerForTest(t *testing.T) {
	t.Helper()

	defaultManagerMu.Lock()
	original := defaultManager
	defaultManager = nil
	defaultManagerMu.Unlock()

	t.Cleanup(func() {
		defaultManagerMu.Lock()
		current := defaultManager
		defaultManager = original
		defaultManagerMu.Unlock()

		if current != nil && current != original {
			current.Close()
		}
	})
}

func waitForLocalEntries(t *testing.T, manager *Manager, want int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := manager.Stats().LocalEntries; got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("expected local entry count %d, got %+v", want, manager.Stats())
}

func TestManagerNilAndHelperFunctions(t *testing.T) {
	var manager *Manager

	manager.UpdateOptions(ManagerOptions{DefaultTTL: time.Second, MaxEntries: 2})
	manager.Close()
	if got, ok := manager.Get("key"); ok || got.ChannelID != 0 {
		t.Fatalf("expected nil manager Get miss, got record=%+v ok=%v", got, ok)
	}
	manager.Set("key", 1, time.Second)
	manager.SetRecord("key", Record{ChannelID: 1}, time.Second)
	manager.Delete("key")
	if got := manager.Clear(); got != 0 {
		t.Fatalf("expected nil manager Clear=0, got %d", got)
	}
	unlock := manager.Lock("key")
	unlock()
	if got := manager.Sweep(time.Time{}); got != 0 {
		t.Fatalf("expected nil manager Sweep=0, got %d", got)
	}
	if got := manager.Stats(); got != (Stats{}) {
		t.Fatalf("expected zero stats for nil manager, got %+v", got)
	}

	now := time.Unix(1700000000, 0)
	if !(entry{expiresAt: now}).expired(now) {
		t.Fatal("expected entry with equal expiresAt to be expired")
	}
	if (entry{}).expired(now) {
		t.Fatal("expected entry without expiresAt not to be expired")
	}
	if got := normalizeRedisPrefix(""); got != defaultRedisPrefix {
		t.Fatalf("expected default redis prefix, got %q", got)
	}
	if got := normalizeRedisPrefix(" custom "); got != "custom" {
		t.Fatalf("expected trimmed redis prefix, got %q", got)
	}
	if got := redisScoreForEntry(entry{expiresAt: now}); got != now.UnixNano() {
		t.Fatalf("expected expiresAt score, got %d", got)
	}
	if got := redisScoreForEntry(entry{record: Record{UpdatedAt: now}}); got != now.UnixNano() {
		t.Fatalf("expected updatedAt score, got %d", got)
	}
	if got := stringSliceToInterfaceSlice([]string{"a", "b"}); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("unexpected interface slice conversion: %#v", got)
	}
	if max(2, 1) != 2 || max(1, 2) != 2 {
		t.Fatal("expected max helper to return larger operand")
	}
}

func TestManagerLocalLifecycleAndJanitor(t *testing.T) {
	manager := NewManagerWithOptions(ManagerOptions{
		DefaultTTL:      20 * time.Millisecond,
		JanitorInterval: 5 * time.Millisecond,
		MaxEntries:      2,
		RedisPrefix:     "  ignored-prefix  ",
	})
	t.Cleanup(func() {
		manager.Close()
	})

	manager.UpdateOptions(ManagerOptions{
		DefaultTTL: 30 * time.Millisecond,
		MaxEntries: 1,
	})

	manager.SetRecord("  affinity:key  ", Record{
		ChannelID:         7,
		ResumeFingerprint: "  model:gpt-5  ",
	}, 0)

	record, ok := manager.Get("affinity:key")
	if !ok {
		t.Fatal("expected local affinity record")
	}
	if record.ChannelID != 7 {
		t.Fatalf("expected channel id 7, got %d", record.ChannelID)
	}
	if record.ResumeFingerprint != "model:gpt-5" {
		t.Fatalf("expected trimmed resume fingerprint, got %q", record.ResumeFingerprint)
	}
	if record.UpdatedAt.IsZero() {
		t.Fatal("expected UpdatedAt to be set")
	}

	manager.Set("second", 8, time.Minute)
	if _, ok := manager.Get("affinity:key"); ok {
		t.Fatal("expected maxEntries=1 to evict the oldest entry")
	}

	manager.SetRecord("soon-expire", Record{ChannelID: 9}, 10*time.Millisecond)
	time.Sleep(40 * time.Millisecond)
	if _, ok := manager.Get("soon-expire"); ok {
		t.Fatal("expected expired local entry to be removed")
	}

	stats := manager.Stats()
	if stats.Backend != "memory" || stats.MaxEntries != 1 {
		t.Fatalf("unexpected local stats: %+v", stats)
	}

	lockA := manager.lockIndex("same-key")
	lockB := manager.lockIndex("same-key")
	if lockA != lockB {
		t.Fatalf("expected stable lock index for same key, got %d and %d", lockA, lockB)
	}
	unlock := manager.Lock("same-key")
	unlock()
}

func TestManagerJanitorSweepsExpiredEntriesFromStats(t *testing.T) {
	manager := NewManagerWithOptions(ManagerOptions{
		DefaultTTL:      15 * time.Millisecond,
		JanitorInterval: 5 * time.Millisecond,
		MaxEntries:      4,
	})
	t.Cleanup(func() {
		manager.Close()
	})

	manager.Set("janitor-expiring", 17, 15*time.Millisecond)
	if got := manager.Stats().LocalEntries; got != 1 {
		t.Fatalf("expected one local entry before janitor sweep, got %d", got)
	}

	waitForLocalEntries(t, manager, 0, 150*time.Millisecond)
}

func TestManagerRedisGetSetTrimSweepAndClear(t *testing.T) {
	manager, server := newRedisBackedManager(t, ManagerOptions{
		DefaultTTL: time.Minute,
		MaxEntries: 2,
	})

	manager.SetRecord("redis-a", Record{
		ChannelID:         11,
		ResumeFingerprint: "  fp-a  ",
	}, time.Minute)
	manager.SetRecord("redis-b", Record{ChannelID: 12}, time.Minute)
	manager.SetRecord("redis-c", Record{ChannelID: 13}, time.Minute)

	stats := manager.Stats()
	if stats.Backend != "hybrid" {
		t.Fatalf("expected hybrid backend stats, got %+v", stats)
	}
	if stats.BackendEntries != 2 {
		t.Fatalf("expected redis trim to keep 2 entries, got %+v", stats)
	}

	mirror := NewManagerWithOptions(ManagerOptions{
		DefaultTTL:  time.Minute,
		MaxEntries:  2,
		RedisClient: server.Client(),
		RedisPrefix: "test:channel-affinity",
	})
	t.Cleanup(func() {
		mirror.Close()
	})

	record, ok := mirror.Get("redis-c")
	if !ok || record.ChannelID != 13 {
		t.Fatalf("expected redis-backed get to restore redis-c, got record=%+v ok=%v", record, ok)
	}
	if _, ok := mirror.Get("redis-a"); ok {
		t.Fatal("expected oldest redis entry to be trimmed away")
	}

	server.SetRaw(mirror.redisEntryKey("broken"), "{not-json")
	if _, ok := mirror.getFromRedis("broken"); ok {
		t.Fatal("expected invalid redis payload to be rejected")
	}
	if _, exists := server.GetRaw(mirror.redisEntryKey("broken")); exists {
		t.Fatal("expected invalid redis payload to be cleaned up")
	}

	manager.SetRecord("expiring", Record{ChannelID: 21}, 5*time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	if removed := manager.Sweep(time.Time{}); removed == 0 {
		t.Fatal("expected Sweep to remove expired entries from memory and redis")
	}
	if _, ok := mirror.Get("expiring"); ok {
		t.Fatal("expected expired redis entry to be removed")
	}

	if cleared := manager.Clear(); cleared < 1 {
		t.Fatalf("expected Clear to remove at least one affinity entry, got %d", cleared)
	}
	if stats := manager.Stats(); stats.BackendEntries != 0 {
		t.Fatalf("expected redis backend to be empty after Clear, got %+v", stats)
	}
}

func TestManagerAdditionalDefaultAndGuardBranches(t *testing.T) {
	originalDefault := DefaultManager()
	originalStats := originalDefault.Stats()
	t.Cleanup(func() {
		ConfigureDefault(ManagerOptions{
			DefaultTTL:  time.Duration(originalStats.DefaultTTL) * time.Second,
			MaxEntries:  originalStats.MaxEntries,
			RedisPrefix: defaultRedisPrefix,
		})
	})

	configured := ConfigureDefault(ManagerOptions{
		DefaultTTL:  2 * time.Second,
		MaxEntries:  9,
		RedisPrefix: " test:default-affinity ",
	})
	if configured != DefaultManager() {
		t.Fatal("expected ConfigureDefault to update and return the shared default manager")
	}
	if stats := configured.Stats(); stats.MaxEntries != 9 || stats.DefaultTTL != 2 {
		t.Fatalf("expected ConfigureDefault to update default manager options, got %+v", stats)
	}

	manager := NewManager(time.Minute, 0)
	manager.entries["expired"] = entry{
		record:    Record{ChannelID: 7},
		expiresAt: time.Now().Add(-time.Second),
	}
	if got, ok := manager.Get("   "); ok || got.ChannelID != 0 {
		t.Fatalf("expected blank affinity key lookup to miss, got record=%+v ok=%v", got, ok)
	}
	if got, ok := manager.Get("expired"); ok || got.ChannelID != 0 {
		t.Fatalf("expected expired local affinity entry to be evicted on read, got record=%+v ok=%v", got, ok)
	}
	if _, ok := manager.entries["expired"]; ok {
		t.Fatal("expected expired local entry to be deleted after Get")
	}
}

func TestConfigureDefaultUsesConstructionOptionsOnFirstInitialization(t *testing.T) {
	resetDefaultManagerForTest(t)

	configured := ConfigureDefault(ManagerOptions{
		DefaultTTL:      20 * time.Millisecond,
		JanitorInterval: 5 * time.Millisecond,
		MaxEntries:      3,
		RedisPrefix:     "test:default-first-init",
	})

	if configured != DefaultManager() {
		t.Fatal("expected ConfigureDefault to install the shared default manager")
	}
	if configured.janitorInterval != 5*time.Millisecond {
		t.Fatalf("expected default manager janitor interval to be set at construction, got %s", configured.janitorInterval)
	}

	configured.Set("default-expiring", 33, 20*time.Millisecond)
	if got := configured.Stats().LocalEntries; got != 1 {
		t.Fatalf("expected one local entry before janitor sweep, got %d", got)
	}

	waitForLocalEntries(t, configured, 0, 150*time.Millisecond)
}

func TestManagerUpdateOptionsDoesNotStartJanitor(t *testing.T) {
	manager := NewManagerWithOptions(ManagerOptions{
		DefaultTTL: 20 * time.Millisecond,
		MaxEntries: 4,
	})
	t.Cleanup(func() {
		manager.Close()
	})

	manager.UpdateOptions(ManagerOptions{
		JanitorInterval: 5 * time.Millisecond,
	})

	if manager.janitorInterval != 0 {
		t.Fatalf("expected UpdateOptions to leave janitor interval unchanged, got %s", manager.janitorInterval)
	}

	manager.Set("update-options-expiring", 44, 20*time.Millisecond)
	time.Sleep(60 * time.Millisecond)

	if got := manager.Stats().LocalEntries; got != 1 {
		t.Fatalf("expected expired entry to remain without janitor, got %d local entries", got)
	}
	if removed := manager.Sweep(time.Now()); removed != 1 {
		t.Fatalf("expected manual sweep to remove expired entry, got %d", removed)
	}
	if got := manager.Stats().LocalEntries; got != 0 {
		t.Fatalf("expected no local entries after manual sweep, got %d", got)
	}
}

func TestManagerSweepRedisRemovesExpiredBackendEntries(t *testing.T) {
	manager, server := newRedisBackedManager(t, ManagerOptions{
		DefaultTTL: time.Minute,
		MaxEntries: 4,
	})

	manager.SetRecord("redis-expired", Record{ChannelID: 91}, time.Millisecond)
	time.Sleep(5 * time.Millisecond)

	if removed := manager.sweepRedis(time.Now()); removed != 1 {
		t.Fatalf("expected sweepRedis to remove one expired backend entry, got %d", removed)
	}
	if _, ok := server.GetRaw(manager.redisEntryKey("redis-expired")); ok {
		t.Fatal("expected expired backend entry to be deleted from redis")
	}
}
