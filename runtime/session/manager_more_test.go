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
	setTestBackend(manager, &stubBindingBackend{
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
	})

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

	setTestBackend(manager, &stubBindingBackend{revocationStatusFn: func(context.Context, string) RevocationStatus { return RevocationRevoked }})
	if removed := manager.Sweep(time.Now()); removed != 1 {
		t.Fatalf("expected revoked session to be swept, removed=%d", removed)
	}
}

func TestManagerCheckRevocationUsesManagerTimeout(t *testing.T) {
	manager := NewManagerWithOptions(ManagerOptions{
		DefaultTTL:        time.Minute,
		RevocationTimeout: 15 * time.Millisecond,
	})

	var sawDeadline atomic.Bool
	setTestBackend(manager, &stubBindingBackend{
		revocationStatusFn: func(ctx context.Context, sessionKey string) RevocationStatus {
			if _, ok := ctx.Deadline(); ok {
				sawDeadline.Store(true)
			}
			<-ctx.Done()
			return RevocationUnknown
		},
	})

	startedAt := time.Now()
	if status := manager.CheckRevocation("channel:2/hash-a/session-timeout"); status != RevocationUnknown {
		t.Fatalf("expected timeout revocation to collapse to unknown, got %q", status)
	}
	if elapsed := time.Since(startedAt); elapsed > 200*time.Millisecond {
		t.Fatalf("expected manager-owned timeout to bound revocation wait, elapsed=%s", elapsed)
	}
	if !sawDeadline.Load() {
		t.Fatal("expected revocation lookup context to carry a deadline")
	}
}

func TestManagerSweepTreatsBulkLengthMismatchAsUnknown(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)
	firstMeta := Metadata{Key: "channel:2/hash-a/session-bulk-a", CallerNS: "token:1", ChannelID: 2}
	secondMeta := Metadata{Key: "channel:2/hash-a/session-bulk-b", CallerNS: "token:2", ChannelID: 2}

	if _, _, err := manager.GetOrCreate(firstMeta); err != nil {
		t.Fatalf("expected first session fixture, got %v", err)
	}
	if _, _, err := manager.GetOrCreate(secondMeta); err != nil {
		t.Fatalf("expected second session fixture, got %v", err)
	}

	setTestBackend(manager, &stubBindingBackend{
		revocationStatusesFn: func(context.Context, []string) ([]RevocationStatus, error) {
			return []RevocationStatus{RevocationRevoked}, nil
		},
	})

	if removed := manager.Sweep(time.Now()); removed != 0 {
		t.Fatalf("expected bulk length mismatch to collapse to unknown and keep sessions, removed=%d", removed)
	}
	if _, ok := manager.sessions[firstMeta.Key]; !ok {
		t.Fatal("expected first session to survive bulk length mismatch")
	}
	if _, ok := manager.sessions[secondMeta.Key]; !ok {
		t.Fatal("expected second session to survive bulk length mismatch")
	}
}

func TestManagerSweepRevokedSessionOverridesTryLockMiss(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)
	meta := Metadata{Key: "channel:2/hash-a/session-revoked-lock", CallerNS: "token:1", ChannelID: 2}
	sess, _, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected session fixture, got %v", err)
	}

	setTestBackend(manager, &stubBindingBackend{
		revocationStatusFn: func(context.Context, string) RevocationStatus {
			return RevocationRevoked
		},
	})

	sess.Lock()
	defer func() {
		sess.Unlock()
	}()

	if removed := manager.Sweep(time.Now()); removed != 1 {
		t.Fatalf("expected revoked session to be removed even when TryLock misses, removed=%d", removed)
	}
	if _, ok := manager.sessions[meta.Key]; ok {
		t.Fatal("expected revoked session to be removed from manager state")
	}
}

func TestManagerAcquireExistingReturnsFalseMissWhenSessionReplacedDuringRevocationCheck(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)
	meta := Metadata{Key: "channel:2/hash-a/session-replaced", CallerNS: "token:1", ChannelID: 2}
	original, _, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected session fixture, got %v", err)
	}

	started := make(chan struct{})
	continueRevocation := make(chan struct{})
	setTestBackend(manager, &stubBindingBackend{
		revocationStatusFn: func(context.Context, string) RevocationStatus {
			close(started)
			<-continueRevocation
			return RevocationNotRevoked
		},
	})

	type acquireResult struct {
		session *ExecutionSession
		release func()
		ok      bool
	}
	resultCh := make(chan acquireResult, 1)
	go func() {
		session, release, ok := manager.AcquireExisting(meta.Key)
		resultCh <- acquireResult{session: session, release: release, ok: ok}
	}()

	<-started

	replacement := NewExecutionSession(meta)
	manager.mu.Lock()
	manager.deleteSessionLocked(meta.Key)
	manager.sessions[meta.Key] = replacement
	manager.addCapacityIndexLocked(replacement)
	manager.mu.Unlock()

	close(continueRevocation)
	result := <-resultCh
	if result.ok || result.session != nil || result.release != nil {
		t.Fatalf("expected AcquireExisting to false-miss on replacement, got session=%+v ok=%v", result.session, result.ok)
	}
	if manager.sessions[meta.Key] != replacement {
		t.Fatal("expected replacement session to remain the live manager entry")
	}
	if manager.sessions[meta.Key] == original {
		t.Fatal("expected original session to be removed during replacement")
	}
}

func TestManagerGetOrCreateRechecksReplacementRevocationAfterRestartExhausted(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)
	meta := Metadata{Key: "channel:2/hash-a/session-replaced-revoked", CallerNS: "token:1", ChannelID: 2}
	original, _, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected session fixture, got %v", err)
	}

	startedFirst := make(chan struct{})
	continueFirst := make(chan struct{})
	startedSecond := make(chan struct{})
	continueSecond := make(chan struct{})
	var calls atomic.Int32
	setTestBackend(manager, &stubBindingBackend{
		revocationStatusFn: func(context.Context, string) RevocationStatus {
			switch calls.Add(1) {
			case 1:
				close(startedFirst)
				<-continueFirst
				return RevocationNotRevoked
			case 2:
				close(startedSecond)
				<-continueSecond
				return RevocationNotRevoked
			default:
				return RevocationRevoked
			}
		},
	})

	type getOrCreateResult struct {
		session *ExecutionSession
		created bool
		err     error
	}
	resultCh := make(chan getOrCreateResult, 1)
	go func() {
		session, created, err := manager.GetOrCreate(meta)
		resultCh <- getOrCreateResult{session: session, created: created, err: err}
	}()

	<-startedFirst

	replacementA := NewExecutionSession(meta)
	manager.mu.Lock()
	manager.deleteSessionLocked(meta.Key)
	manager.sessions[meta.Key] = replacementA
	manager.addCapacityIndexLocked(replacementA)
	manager.mu.Unlock()

	close(continueFirst)
	<-startedSecond

	replacementB := NewExecutionSession(meta)
	manager.mu.Lock()
	manager.deleteSessionLocked(meta.Key)
	manager.sessions[meta.Key] = replacementB
	manager.addCapacityIndexLocked(replacementB)
	manager.mu.Unlock()

	close(continueSecond)
	result := <-resultCh
	if result.err != nil {
		t.Fatalf("expected GetOrCreate to recover from replacement churn, got %v", result.err)
	}
	if !result.created || result.session == nil {
		t.Fatalf("expected GetOrCreate to create a fresh session after revoked replacements, session=%+v created=%v", result.session, result.created)
	}
	if result.session == original || result.session == replacementA || result.session == replacementB {
		t.Fatalf("expected a fresh session instance, got %+v", result.session)
	}
	if manager.sessions[meta.Key] != result.session {
		t.Fatal("expected fresh session to remain the live manager entry")
	}
	if calls.Load() < 3 {
		t.Fatalf("expected replacement revalidation to issue a third revocation check, calls=%d", calls.Load())
	}
}

func TestManagerGetOrCreateBoundRechecksReplacementRevocationAfterRestartExhausted(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)
	meta := Metadata{
		Key:        "channel:2/hash-a/session-bound-replaced-revoked",
		BindingKey: BuildBindingKey("token:1", BindingScopeChatRealtime, "session-bound-replaced-revoked"),
		SessionID:  "session-bound-replaced-revoked",
		CallerNS:   "token:1",
		ChannelID:  2,
	}
	original, created, conflict, err := manager.GetOrCreateBound(meta)
	if err != nil || !created || conflict != nil {
		t.Fatalf("expected bound session fixture, session=%+v created=%v conflict=%+v err=%v", original, created, conflict, err)
	}

	startedFirst := make(chan struct{})
	continueFirst := make(chan struct{})
	startedSecond := make(chan struct{})
	continueSecond := make(chan struct{})
	var calls atomic.Int32
	setTestBackend(manager, &stubBindingBackend{
		revocationStatusFn: func(context.Context, string) RevocationStatus {
			switch calls.Add(1) {
			case 1, 3:
				return RevocationNotRevoked
			case 2:
				close(startedFirst)
				<-continueFirst
				return RevocationNotRevoked
			case 4:
				close(startedSecond)
				<-continueSecond
				return RevocationNotRevoked
			default:
				return RevocationRevoked
			}
		},
	})

	type boundResult struct {
		session  *ExecutionSession
		created  bool
		conflict *Binding
		err      error
	}
	resultCh := make(chan boundResult, 1)
	go func() {
		session, created, conflict, err := manager.GetOrCreateBound(meta)
		resultCh <- boundResult{session: session, created: created, conflict: conflict, err: err}
	}()

	<-startedFirst

	replacementA := NewExecutionSession(meta)
	manager.mu.Lock()
	manager.deleteSessionLocked(meta.Key)
	manager.sessions[meta.Key] = replacementA
	manager.addCapacityIndexLocked(replacementA)
	manager.upsertBindingLocked(replacementA)
	manager.mu.Unlock()

	close(continueFirst)
	<-startedSecond

	replacementB := NewExecutionSession(meta)
	manager.mu.Lock()
	manager.deleteSessionLocked(meta.Key)
	manager.sessions[meta.Key] = replacementB
	manager.addCapacityIndexLocked(replacementB)
	manager.upsertBindingLocked(replacementB)
	manager.mu.Unlock()

	close(continueSecond)
	result := <-resultCh
	if result.err != nil {
		t.Fatalf("expected GetOrCreateBound to recover from replacement churn, got %v", result.err)
	}
	if result.conflict != nil {
		t.Fatalf("expected latest truth to stay on same binding owner key, got conflict=%+v", result.conflict)
	}
	if !result.created || result.session == nil {
		t.Fatalf("expected GetOrCreateBound to create a fresh session after revoked replacements, session=%+v created=%v", result.session, result.created)
	}
	if result.session == original || result.session == replacementA || result.session == replacementB {
		t.Fatalf("expected a fresh bound session instance, got %+v", result.session)
	}
	if manager.sessions[meta.Key] != result.session {
		t.Fatal("expected fresh bound session to remain the live manager entry")
	}
	manager.mu.RLock()
	binding := manager.bindings[meta.BindingKey]
	manager.mu.RUnlock()
	if binding == nil || binding.SessionKey != result.session.Key {
		t.Fatalf("expected local binding to point at the fresh session, binding=%+v", binding)
	}
	if calls.Load() < 5 {
		t.Fatalf("expected bound replacement revalidation to reach the replacement revocation check, calls=%d", calls.Load())
	}
}

func TestManagerResolveLocalReturnsFalseMissWhenBindingReplacedDuringRevocationCheck(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)
	bindingKey := BuildBindingKey("token:1", BindingScopeChatRealtime, "resolve-local-replaced")
	originalMeta := Metadata{
		Key:        "channel:2/hash-a/session-resolve-local-original",
		BindingKey: bindingKey,
		SessionID:  "resolve-local-original",
		CallerNS:   "token:1",
		ChannelID:  2,
	}
	if _, created, conflict, err := manager.GetOrCreateBound(originalMeta); err != nil || !created || conflict != nil {
		t.Fatalf("expected original bound fixture, created=%v conflict=%+v err=%v", created, conflict, err)
	}

	replacementMetaA := originalMeta
	replacementMetaA.Key = "channel:2/hash-a/session-resolve-local-replacement-a"
	replacementMetaA.SessionID = "resolve-local-replacement-a"
	replacementMetaB := originalMeta
	replacementMetaB.Key = "channel:2/hash-a/session-resolve-local-replacement-b"
	replacementMetaB.SessionID = "resolve-local-replacement-b"

	startedFirst := make(chan struct{})
	continueFirst := make(chan struct{})
	startedSecond := make(chan struct{})
	continueSecond := make(chan struct{})
	var calls atomic.Int32
	setTestBackend(manager, &stubBindingBackend{
		revocationStatusFn: func(context.Context, string) RevocationStatus {
			switch calls.Add(1) {
			case 1:
				close(startedFirst)
				<-continueFirst
				return RevocationNotRevoked
			case 2:
				close(startedSecond)
				<-continueSecond
				return RevocationNotRevoked
			default:
				return RevocationRevoked
			}
		},
	})

	type resolveResult struct {
		binding *Binding
		ok      bool
	}
	resultCh := make(chan resolveResult, 1)
	go func() {
		binding, ok := manager.ResolveLocal(bindingKey)
		resultCh <- resolveResult{binding: binding, ok: ok}
	}()

	<-startedFirst
	replacementA := NewExecutionSession(replacementMetaA)
	manager.mu.Lock()
	manager.deleteSessionLocked(originalMeta.Key)
	manager.sessions[replacementMetaA.Key] = replacementA
	manager.addCapacityIndexLocked(replacementA)
	manager.upsertBindingLocked(replacementA)
	manager.mu.Unlock()
	close(continueFirst)

	<-startedSecond
	replacementB := NewExecutionSession(replacementMetaB)
	manager.mu.Lock()
	manager.deleteSessionLocked(replacementMetaA.Key)
	manager.sessions[replacementMetaB.Key] = replacementB
	manager.addCapacityIndexLocked(replacementB)
	manager.upsertBindingLocked(replacementB)
	manager.mu.Unlock()
	close(continueSecond)

	result := <-resultCh
	if result.ok || result.binding != nil {
		t.Fatalf("expected ResolveLocal to false-miss on replaced binding, binding=%+v ok=%v", result.binding, result.ok)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected two revocation checks before false miss, calls=%d", calls.Load())
	}
}

func TestManagerSweepUsesProvidedNowDuringPhaseThree(t *testing.T) {
	manager := NewManager(30*time.Millisecond, 0, nil)
	meta := Metadata{
		Key:       "channel:2/hash-a/session-sweep-now-anchor",
		CallerNS:  "token:1",
		ChannelID: 2,
		IdleTTL:   30 * time.Millisecond,
	}
	sess, _, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected sweep fixture session, got %v", err)
	}

	lastUsedAt := time.Now()
	sess.Lock()
	sess.LastUsedAt = lastUsedAt
	sess.Unlock()

	setTestBackend(manager, &stubBindingBackend{
		revocationStatusFn: func(context.Context, string) RevocationStatus {
			time.Sleep(50 * time.Millisecond)
			return RevocationNotRevoked
		},
	})

	if removed := manager.Sweep(lastUsedAt.Add(10 * time.Millisecond)); removed != 0 {
		t.Fatalf("expected Sweep(now) to respect caller time anchor and keep session, removed=%d", removed)
	}
	if _, ok := manager.sessions[meta.Key]; !ok {
		t.Fatal("expected session to remain after Sweep(now) phase-three recheck")
	}
}

func TestManagerGetOrCreateBoundRechecksReplacementBindingRevocationAfterRestartExhausted(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)
	bindingKey := BuildBindingKey("token:1", BindingScopeChatRealtime, "bound-binding-replaced")
	conflictMeta := Metadata{
		Key:        "channel:2/hash-a/session-bound-binding-original",
		BindingKey: bindingKey,
		SessionID:  "bound-binding-original",
		CallerNS:   "token:1",
		ChannelID:  2,
	}
	if _, created, conflict, err := manager.GetOrCreateBound(conflictMeta); err != nil || !created || conflict != nil {
		t.Fatalf("expected original conflict fixture, created=%v conflict=%+v err=%v", created, conflict, err)
	}

	targetMeta := Metadata{
		Key:        "channel:2/hash-a/session-bound-binding-target",
		BindingKey: bindingKey,
		SessionID:  "bound-binding-target",
		CallerNS:   "token:1",
		ChannelID:  2,
	}
	replacementMetaA := conflictMeta
	replacementMetaA.Key = "channel:2/hash-a/session-bound-binding-replacement-a"
	replacementMetaA.SessionID = "bound-binding-replacement-a"
	replacementMetaB := conflictMeta
	replacementMetaB.Key = "channel:2/hash-a/session-bound-binding-replacement-b"
	replacementMetaB.SessionID = "bound-binding-replacement-b"

	startedFirst := make(chan struct{})
	continueFirst := make(chan struct{})
	startedSecond := make(chan struct{})
	continueSecond := make(chan struct{})
	var calls atomic.Int32
	setTestBackend(manager, &stubBindingBackend{
		revocationStatusFn: func(context.Context, string) RevocationStatus {
			switch calls.Add(1) {
			case 1:
				close(startedFirst)
				<-continueFirst
				return RevocationNotRevoked
			case 2:
				close(startedSecond)
				<-continueSecond
				return RevocationNotRevoked
			default:
				return RevocationRevoked
			}
		},
	})

	type boundResult struct {
		session  *ExecutionSession
		created  bool
		conflict *Binding
		err      error
	}
	resultCh := make(chan boundResult, 1)
	go func() {
		session, created, conflict, err := manager.GetOrCreateBound(targetMeta)
		resultCh <- boundResult{session: session, created: created, conflict: conflict, err: err}
	}()

	<-startedFirst
	replacementA := NewExecutionSession(replacementMetaA)
	manager.mu.Lock()
	manager.deleteSessionLocked(conflictMeta.Key)
	manager.sessions[replacementMetaA.Key] = replacementA
	manager.addCapacityIndexLocked(replacementA)
	manager.upsertBindingLocked(replacementA)
	manager.mu.Unlock()
	close(continueFirst)

	<-startedSecond
	replacementB := NewExecutionSession(replacementMetaB)
	manager.mu.Lock()
	manager.deleteSessionLocked(replacementMetaA.Key)
	manager.sessions[replacementMetaB.Key] = replacementB
	manager.addCapacityIndexLocked(replacementB)
	manager.upsertBindingLocked(replacementB)
	manager.mu.Unlock()
	close(continueSecond)

	result := <-resultCh
	if result.err != nil {
		t.Fatalf("expected bound replacement revocation recheck to recover, got %v", result.err)
	}
	if result.conflict != nil {
		t.Fatalf("expected revoked replacement binding not to surface as conflict, conflict=%+v", result.conflict)
	}
	if !result.created || result.session == nil || result.session.Key != targetMeta.Key {
		t.Fatalf("expected fresh target session after revoked replacement binding, session=%+v created=%v", result.session, result.created)
	}
	manager.mu.RLock()
	binding := manager.bindings[bindingKey]
	manager.mu.RUnlock()
	if binding == nil || binding.SessionKey != targetMeta.Key {
		t.Fatalf("expected local binding to converge to target session, binding=%+v", binding)
	}
	if calls.Load() < 3 {
		t.Fatalf("expected replacement binding to be revalidated on the final attempt, calls=%d", calls.Load())
	}
}

func TestManagerGetOrCreateBoundFinalFallbackRevalidatesConflictAfterSessionLatestTruth(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)
	bindingKey := BuildBindingKey("token:1", BindingScopeChatRealtime, "bound-final-fallback-session-latest-truth")
	targetMeta := Metadata{
		Key:        "channel:2/hash-a/session-bound-final-target",
		BindingKey: bindingKey,
		SessionID:  "bound-final-target",
		CallerNS:   "token:1",
		ChannelID:  2,
	}
	if _, created, conflict, err := manager.GetOrCreateBound(targetMeta); err != nil || !created || conflict != nil {
		t.Fatalf("expected target fixture session, created=%v conflict=%+v err=%v", created, conflict, err)
	}

	startedFirst := make(chan struct{})
	continueFirst := make(chan struct{})
	startedSecond := make(chan struct{})
	continueSecond := make(chan struct{})
	startedFinalSessionObserve := make(chan struct{})
	continueFinalSessionObserve := make(chan struct{})
	var calls atomic.Int32
	setTestBackend(manager, &stubBindingBackend{
		revocationStatusFn: func(context.Context, string) RevocationStatus {
			switch calls.Add(1) {
			case 1:
				close(startedFirst)
				<-continueFirst
				return RevocationNotRevoked
			case 2:
				close(startedSecond)
				<-continueSecond
				return RevocationNotRevoked
			case 4:
				close(startedFinalSessionObserve)
				<-continueFinalSessionObserve
				return RevocationNotRevoked
			default:
				return RevocationNotRevoked
			}
		},
	})

	type boundResult struct {
		session  *ExecutionSession
		created  bool
		conflict *Binding
		err      error
	}
	resultCh := make(chan boundResult, 1)
	go func() {
		session, created, conflict, err := manager.GetOrCreateBound(targetMeta)
		resultCh <- boundResult{session: session, created: created, conflict: conflict, err: err}
	}()

	<-startedFirst
	replacementA := NewExecutionSession(targetMeta)
	manager.mu.Lock()
	manager.deleteSessionLocked(targetMeta.Key)
	manager.sessions[targetMeta.Key] = replacementA
	manager.addCapacityIndexLocked(replacementA)
	manager.upsertBindingLocked(replacementA)
	manager.mu.Unlock()
	close(continueFirst)

	<-startedSecond
	replacementB := NewExecutionSession(targetMeta)
	manager.mu.Lock()
	manager.deleteSessionLocked(targetMeta.Key)
	manager.sessions[targetMeta.Key] = replacementB
	manager.addCapacityIndexLocked(replacementB)
	manager.upsertBindingLocked(replacementB)
	manager.mu.Unlock()
	close(continueSecond)

	<-startedFinalSessionObserve
	conflictMeta := targetMeta
	conflictMeta.Key = "channel:2/hash-a/session-bound-final-conflict"
	conflictMeta.SessionID = "bound-final-conflict"
	conflictSess := NewExecutionSession(conflictMeta)
	manager.mu.Lock()
	manager.sessions[conflictMeta.Key] = conflictSess
	manager.addCapacityIndexLocked(conflictSess)
	manager.upsertBindingLocked(conflictSess)
	manager.mu.Unlock()
	close(continueFinalSessionObserve)

	result := <-resultCh
	if result.err != nil {
		t.Fatalf("expected final fallback to succeed, got %v", result.err)
	}
	if result.session != nil || result.created {
		t.Fatalf("expected final fallback to surface a conflict, session=%+v created=%v", result.session, result.created)
	}
	if result.conflict == nil || result.conflict.SessionKey != conflictMeta.Key {
		t.Fatalf("expected verified latest conflict binding, conflict=%+v", result.conflict)
	}
	if calls.Load() < 5 {
		t.Fatalf("expected final fallback to revalidate the rebound conflict owner, calls=%d", calls.Load())
	}
}
