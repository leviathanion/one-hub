package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"one-api/internal/testutil/fakeredis"

	"github.com/redis/go-redis/v9"
)

func TestManagerAcquireOrCreateAndAcquireExistingLifecycle(t *testing.T) {
	manager := NewManager(5*time.Millisecond, 0, nil)
	meta := Metadata{
		Key:        "channel:2/hash-a/session-acquire-existing",
		BindingKey: BuildBindingKey("token:1", BindingScopeChatRealtime, "session-acquire-existing"),
		SessionID:  "session-acquire-existing",
		CallerNS:   "token:1",
		ChannelID:  2,
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
		IdleTTL:    5 * time.Millisecond,
	}

	sess, created, release, err := manager.AcquireOrCreate(meta)
	if err != nil || !created || release == nil {
		t.Fatalf("expected AcquireOrCreate to create a leased session, created=%v release_nil=%v err=%v", created, release == nil, err)
	}
	if acquired, releaseExisting, ok := manager.AcquireExisting(meta.Key); !ok || acquired != sess || releaseExisting == nil {
		t.Fatalf("expected AcquireExisting to lease the live session, acquired=%+v ok=%v", acquired, ok)
	} else {
		releaseExisting()
	}

	sess.Lock()
	sess.LastUsedAt = time.Now().Add(-time.Minute)
	sess.Unlock()
	if removed := manager.Sweep(time.Now()); removed != 0 {
		t.Fatalf("expected leased session to survive sweep, removed=%d", removed)
	}
	release()
	if removed := manager.Sweep(time.Now()); removed != 1 {
		t.Fatalf("expected released expired session to be removed, removed=%d", removed)
	}
	if acquired, releaseExisting, ok := manager.AcquireExisting(meta.Key); ok || acquired != nil || releaseExisting != nil {
		t.Fatalf("expected AcquireExisting on removed session to fail, acquired=%+v ok=%v", acquired, ok)
	}
}

func TestManagerAcquireOrCreateReturnsCapacityError(t *testing.T) {
	manager := NewManagerWithOptions(ManagerOptions{DefaultTTL: time.Minute, MaxSessions: 1})
	firstMeta := Metadata{Key: "channel:2/hash-a/session-cap-1", CallerNS: "token:1", ChannelID: 2}
	secondMeta := Metadata{Key: "channel:2/hash-a/session-cap-2", CallerNS: "token:2", ChannelID: 2}

	if _, _, _, err := manager.AcquireOrCreate(firstMeta); err != nil {
		t.Fatalf("expected first AcquireOrCreate to succeed, got %v", err)
	}
	if sess, created, release, err := manager.AcquireOrCreate(secondMeta); !errors.Is(err, ErrCapacityExceeded) || sess != nil || created || release != nil {
		t.Fatalf("expected AcquireOrCreate to surface capacity error, sess=%+v created=%v release_nil=%v err=%v", sess, created, release == nil, err)
	}
}

func TestManagerDeleteDeleteBindingAndWrapperStatuses(t *testing.T) {
	var deleted atomic.Int32
	manager := NewManager(time.Minute, 0, nil)
	manager.backend = &stubBindingBackend{
		deleteBindingIfSessionMatchesFn: func(context.Context, string, string) BindingWriteStatus {
			deleted.Add(1)
			return BindingWriteApplied
		},
		createBindingIfAbsentFn: func(context.Context, *Binding, time.Duration) BindingWriteStatus {
			return BindingWriteConditionMismatch
		},
		replaceBindingIfSessionMatchesFn: func(context.Context, string, string, *Binding, time.Duration) BindingWriteStatus {
			return BindingWriteBackendError
		},
		touchBindingIfSessionMatchesFn: func(context.Context, string, string, time.Duration) BindingWriteStatus {
			return BindingWriteApplied
		},
		deleteBindingAndRevokeIfSessionMatch: func(context.Context, string, string, time.Duration) BindingWriteStatus {
			return BindingWriteConditionMismatch
		},
		revocationStatusFn: func(context.Context, string) RevocationStatus {
			return RevocationUnknown
		},
		countBindingsFn: func(context.Context) int64 { return 7 },
	}

	meta := Metadata{
		Key:        "channel:2/hash-a/session-delete",
		BindingKey: BuildBindingKey("token:1", BindingScopeChatRealtime, "session-delete"),
		SessionID:  "session-delete",
		CallerNS:   "token:1",
		ChannelID:  2,
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
	}
	sess, _, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected session fixture, got %v", err)
	}

	if status := manager.CreateBindingIfAbsent(sess.BuildBinding(), time.Minute); status != BindingWriteConditionMismatch {
		t.Fatalf("expected create wrapper to forward backend status, got %q", status)
	}
	if status := manager.ReplaceBindingIfSessionMatches(meta.BindingKey, meta.Key, sess.BuildBinding(), time.Minute); status != BindingWriteBackendError {
		t.Fatalf("expected replace wrapper to forward backend status, got %q", status)
	}
	if status := manager.TouchBindingIfSessionMatches(meta.BindingKey, meta.Key, time.Minute); status != BindingWriteApplied {
		t.Fatalf("expected touch wrapper to forward backend status, got %q", status)
	}
	if status := manager.DeleteBindingAndRevokeIfSessionMatches(meta.BindingKey, meta.Key, time.Minute); status != BindingWriteConditionMismatch {
		t.Fatalf("expected delete-and-revoke wrapper to forward backend status, got %q", status)
	}
	if status := manager.CheckRevocation(meta.Key); status != RevocationUnknown {
		t.Fatalf("expected revocation wrapper to forward backend status, got %q", status)
	}

	if removed := manager.Delete(meta.Key); removed != sess {
		t.Fatalf("expected Delete to remove session, got %+v", removed)
	}
	if deleted.Load() != 1 {
		t.Fatalf("expected Delete to propagate pending binding deletion once, got %d", deleted.Load())
	}
	if binding := manager.DeleteBinding(meta.BindingKey); binding != nil {
		t.Fatalf("expected DeleteBinding to return nil once local binding is gone, got %+v", binding)
	}
	if deleted := manager.Delete("missing"); deleted != nil {
		t.Fatalf("expected Delete on missing session to be nil, got %+v", deleted)
	}
	if stats := manager.Stats(); stats.Backend != "redis" || stats.BackendBindings != 7 {
		t.Fatalf("expected backend stats to use stub backend count, got %+v", stats)
	}
}

func TestManagerNilReceiverAndGuardedWrapperBranches(t *testing.T) {
	var nilManager *Manager
	binding := testBinding(BuildBindingKey("token:1", BindingScopeChatRealtime, "session-nil-manager"), "channel:2/hash-a/session-nil-manager")

	nilManager.ConfigureRedis(nil, "ignored")
	if stats := nilManager.Stats(); stats != (Stats{}) {
		t.Fatalf("expected nil manager stats to be zero-value, got %+v", stats)
	}
	if resolved, status := nilManager.ResolveBinding(""); status != ResolveMiss || resolved != nil {
		t.Fatalf("expected blank resolve on nil manager to miss, got status=%q binding=%+v", status, resolved)
	}
	if resolved, status := nilManager.ResolveBinding(binding.Key); status != ResolveMiss || resolved != nil {
		t.Fatalf("expected resolve on nil manager to miss, got status=%q binding=%+v", status, resolved)
	}
	if status := nilManager.CheckRevocation(""); status != RevocationNotRevoked {
		t.Fatalf("expected blank revocation on nil manager to be not_revoked, got %q", status)
	}
	if status := nilManager.CheckRevocation(binding.SessionKey); status != RevocationNotRevoked {
		t.Fatalf("expected revocation on nil manager to be not_revoked, got %q", status)
	}
	if status := nilManager.CreateBindingIfAbsent(nil, time.Minute); status != BindingWriteBackendError {
		t.Fatalf("expected nil binding create to fail, got %q", status)
	}
	if status := nilManager.CreateBindingIfAbsent(binding, time.Minute); status != BindingWriteApplied {
		t.Fatalf("expected nil manager create wrapper to short-circuit as applied, got %q", status)
	}
	if status := nilManager.ReplaceBindingIfSessionMatches(binding.Key, binding.SessionKey, nil, time.Minute); status != BindingWriteBackendError {
		t.Fatalf("expected nil replacement replace wrapper to fail, got %q", status)
	}
	if status := nilManager.ReplaceBindingIfSessionMatches(binding.Key, binding.SessionKey, binding, time.Minute); status != BindingWriteApplied {
		t.Fatalf("expected nil manager replace wrapper to short-circuit as applied, got %q", status)
	}
	if status := nilManager.DeleteBindingIfSessionMatches(binding.Key, binding.SessionKey); status != BindingWriteApplied {
		t.Fatalf("expected nil manager delete wrapper to short-circuit as applied, got %q", status)
	}
	if status := nilManager.TouchBindingIfSessionMatches(binding.Key, binding.SessionKey, time.Minute); status != BindingWriteApplied {
		t.Fatalf("expected nil manager touch wrapper to short-circuit as applied, got %q", status)
	}
	if status := nilManager.DeleteBindingAndRevokeIfSessionMatches(binding.Key, binding.SessionKey, time.Minute); status != BindingWriteApplied {
		t.Fatalf("expected nil manager delete-and-revoke wrapper to short-circuit as applied, got %q", status)
	}
	if ttl := nilManager.RevocationTTLForSession(binding.SessionKey); ttl != minimumSessionRevocationTTL {
		t.Fatalf("expected nil manager revocation ttl floor, got %s", ttl)
	}
}

func TestManagerConfigureRedisCloseRevocationTTLAndJanitor(t *testing.T) {
	server, err := fakeredis.Start()
	if err != nil {
		t.Fatalf("failed to start fake redis: %v", err)
	}
	defer server.Close()

	manager := NewManagerWithOptions(ManagerOptions{
		DefaultTTL:      5 * time.Millisecond,
		JanitorInterval: 5 * time.Millisecond,
		Cleanup:         nil,
	})
	defer manager.Close()
	manager.ConfigureRedis(server.Client(), " test:execution-session ")
	manager.ConfigureRedis((*redis.Client)(nil), "")

	meta := Metadata{
		Key:        "channel:2/hash-a/session-janitor",
		BindingKey: BuildBindingKey("token:1", BindingScopeChatRealtime, "session-janitor"),
		SessionID:  "session-janitor",
		CallerNS:   "token:1",
		ChannelID:  2,
		IdleTTL:    5 * time.Millisecond,
	}
	sess, _, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected janitor session fixture, got %v", err)
	}
	sess.Lock()
	sess.LastUsedAt = time.Now().Add(-time.Minute)
	sess.Unlock()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := manager.ResolveLocal(meta.BindingKey); !ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := manager.ResolveLocal(meta.BindingKey); ok {
		t.Fatal("expected janitor to sweep expired local binding")
	}
	if ttl := manager.RevocationTTLForSession(meta.Key); ttl < minimumSessionRevocationTTL {
		t.Fatalf("expected revocation ttl to honor minimum floor, got %s", ttl)
	}
	manager.Close()
	manager.Close()
}

func TestManagerDeleteBindingReturnsLocalBindingAndAcquireExistingDropsExpiredSession(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)
	meta := Metadata{
		Key:        "channel:2/hash-a/session-delete-binding",
		BindingKey: BuildBindingKey("token:1", BindingScopeChatRealtime, "session-delete-binding"),
		SessionID:  "session-delete-binding",
		CallerNS:   "token:1",
		ChannelID:  2,
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
	}
	sess, _, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected session fixture, got %v", err)
	}
	if binding := manager.DeleteBinding(meta.BindingKey); binding == nil || binding.SessionKey != sess.Key {
		t.Fatalf("expected DeleteBinding to return removed binding, got %+v", binding)
	}

	manager.mu.Lock()
	manager.upsertBindingLocked(sess)
	sess.LastUsedAt = time.Now().Add(-2 * time.Minute)
	manager.mu.Unlock()
	if acquired, release, ok := manager.AcquireExisting(meta.Key); ok || acquired != nil || release != nil {
		t.Fatalf("expected expired AcquireExisting path to evict stale session, acquired=%+v ok=%v", acquired, ok)
	}
}

func TestManagerBoundAndCleanupGuardBranches(t *testing.T) {
	manager := NewManagerWithOptions(ManagerOptions{DefaultTTL: time.Minute, MaxSessions: 1})
	metaA := Metadata{
		Key:        "channel:2/hash-a/session-bound-a",
		BindingKey: BuildBindingKey("token:1", BindingScopeChatRealtime, "session-bound-a"),
		SessionID:  "session-bound-a",
		CallerNS:   "token:1",
		ChannelID:  2,
	}
	metaB := Metadata{
		Key:        "channel:2/hash-a/session-bound-b",
		BindingKey: BuildBindingKey("token:2", BindingScopeChatRealtime, "session-bound-b"),
		SessionID:  "session-bound-b",
		CallerNS:   "token:2",
		ChannelID:  2,
	}

	if _, created, conflict, err := manager.GetOrCreateBound(metaA); err != nil || !created || conflict != nil {
		t.Fatalf("expected first GetOrCreateBound to create session, created=%v conflict=%+v err=%v", created, conflict, err)
	}
	if sess, created, conflict, err := manager.GetOrCreateBound(metaB); !errors.Is(err, ErrCapacityExceeded) || sess != nil || created || conflict != nil {
		t.Fatalf("expected bound GetOrCreate to surface capacity error, sess=%+v created=%v conflict=%+v err=%v", sess, created, conflict, err)
	}
	if sess, created, conflict, release, err := manager.AcquireOrCreateBound(metaB); !errors.Is(err, ErrCapacityExceeded) || sess != nil || created || conflict != nil || release != nil {
		t.Fatalf("expected bound AcquireOrCreate to surface capacity error, sess=%+v created=%v conflict=%+v release_nil=%v err=%v", sess, created, conflict, release == nil, err)
	}

	manager.TouchBinding("", metaA.Key, 0)
	manager.TouchBinding(metaA.BindingKey, "", 0)
	manager.TouchBinding(metaA.BindingKey, "other-session", 9)
	if binding, ok := manager.ResolveLocal(metaA.BindingKey); !ok || binding.SessionKey != metaA.Key {
		t.Fatalf("expected stale TouchBinding guard not to mutate local binding, binding=%+v ok=%v", binding, ok)
	}

	manager.mu.Lock()
	manager.removeBindingIndexLocked("", metaA.BindingKey)
	manager.removeBindingIndexLocked(metaA.Key, "")
	manager.removeBindingIndexLocked("missing-session", metaA.BindingKey)
	manager.mu.Unlock()

	manager.applyPendingBindingDeletes([]pendingBindingDelete{{bindingKey: " ", sessionKey: metaA.Key}})
	manager.cleanupSessions([]*ExecutionSession{nil})
}

func TestManagerAcquireOrCreateBoundConflictAndInvalidateOrphanBinding(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)
	bindingKey := BuildBindingKey("token:1", BindingScopeChatRealtime, "session-bound-conflict")
	metaA := Metadata{Key: "channel:2/hash-a/session-bound-a", BindingKey: bindingKey, SessionID: "session-bound", CallerNS: "token:1", ChannelID: 2}
	metaB := Metadata{Key: "channel:3/hash-b/session-bound-b", BindingKey: bindingKey, SessionID: "session-bound", CallerNS: "token:1", ChannelID: 3}

	if _, created, conflict, release, err := manager.AcquireOrCreateBound(metaA); err != nil || !created || conflict != nil || release == nil {
		t.Fatalf("expected first AcquireOrCreateBound to create without conflict, created=%v conflict=%+v release_nil=%v err=%v", created, conflict, release == nil, err)
	} else {
		release()
	}
	if sess, created, conflict, release, err := manager.AcquireOrCreateBound(metaB); err != nil || created || sess != nil || release != nil || conflict == nil || conflict.SessionKey != metaA.Key {
		t.Fatalf("expected conflicting AcquireOrCreateBound to return existing binding, sess=%+v created=%v conflict=%+v release_nil=%v err=%v", sess, created, conflict, release == nil, err)
	}
	if invalidated := manager.InvalidateBinding(""); invalidated != nil {
		t.Fatalf("expected blank invalidation to be ignored, got %+v", invalidated)
	}

	orphanManager := NewManager(time.Minute, 0, nil)
	orphan, _, err := orphanManager.GetOrCreate(metaA)
	if err != nil {
		t.Fatalf("expected orphan fixture session, got %v", err)
	}
	orphanManager.mu.Lock()
	delete(orphanManager.sessions, orphan.Key)
	orphanManager.mu.Unlock()
	if invalidated := orphanManager.InvalidateBinding(bindingKey); invalidated != nil {
		t.Fatalf("expected orphaned binding invalidation to return nil after local cleanup, got %+v", invalidated)
	}
	if binding, ok := orphanManager.ResolveLocal(bindingKey); ok || binding != nil {
		t.Fatalf("expected orphaned binding to be cleaned from local cache, got %+v ok=%v", binding, ok)
	}
}

func TestManagerSweepSkipsLockedSessionsAndRemovesRevokedSessions(t *testing.T) {
	manager := NewManager(5*time.Millisecond, 0, nil)
	meta := Metadata{Key: "channel:2/hash-a/session-locked", BindingKey: BuildBindingKey("token:1", BindingScopeChatRealtime, "session-locked"), SessionID: "session-locked", CallerNS: "token:1", ChannelID: 2, IdleTTL: 5 * time.Millisecond}
	sess, _, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected locked session fixture, got %v", err)
	}
	sess.Lock()
	sess.LastUsedAt = time.Now().Add(-time.Minute)
	if removed := manager.Sweep(time.Now()); removed != 0 {
		t.Fatalf("expected locked session to survive sweep, removed=%d", removed)
	}
	sess.Unlock()

	manager.backend = &stubBindingBackend{revocationStatusFn: func(context.Context, string) RevocationStatus { return RevocationRevoked }}
	if removed := manager.Sweep(time.Now()); removed != 1 {
		t.Fatalf("expected revoked session to be swept, removed=%d", removed)
	}
}
