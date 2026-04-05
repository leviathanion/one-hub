package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type stubBindingBackend struct {
	resolveBindingFn                     func(context.Context, string) (*Binding, ResolveStatus)
	revocationStatusFn                   func(context.Context, string) RevocationStatus
	revocationStatusesFn                 func(context.Context, []string) ([]RevocationStatus, error)
	createBindingIfAbsentFn              func(context.Context, *Binding, time.Duration) BindingWriteStatus
	replaceBindingIfSessionMatchesFn     func(context.Context, string, string, *Binding, time.Duration) BindingWriteStatus
	deleteBindingIfSessionMatchesFn      func(context.Context, string, string) BindingWriteStatus
	touchBindingIfSessionMatchesFn       func(context.Context, string, string, time.Duration) BindingWriteStatus
	deleteBindingAndRevokeIfSessionMatch func(context.Context, string, string, time.Duration) BindingWriteStatus
	countBindingsFn                      func(context.Context) int64
}

func (s *stubBindingBackend) ResolveBinding(ctx context.Context, bindingKey string) (*Binding, ResolveStatus) {
	if s != nil && s.resolveBindingFn != nil {
		return s.resolveBindingFn(ctx, bindingKey)
	}
	return nil, ResolveMiss
}

func (s *stubBindingBackend) RevocationStatus(ctx context.Context, sessionKey string) RevocationStatus {
	if s != nil && s.revocationStatusFn != nil {
		return s.revocationStatusFn(ctx, sessionKey)
	}
	return RevocationNotRevoked
}

func (s *stubBindingBackend) RevocationStatuses(ctx context.Context, sessionKeys []string) ([]RevocationStatus, error) {
	if s != nil && s.revocationStatusesFn != nil {
		return s.revocationStatusesFn(ctx, sessionKeys)
	}
	statuses := make([]RevocationStatus, len(sessionKeys))
	for i, sessionKey := range sessionKeys {
		statuses[i] = s.RevocationStatus(ctx, sessionKey)
	}
	return statuses, nil
}

func (s *stubBindingBackend) CreateBindingIfAbsent(ctx context.Context, binding *Binding, ttl time.Duration) BindingWriteStatus {
	if s != nil && s.createBindingIfAbsentFn != nil {
		return s.createBindingIfAbsentFn(ctx, binding, ttl)
	}
	return BindingWriteApplied
}

func (s *stubBindingBackend) ReplaceBindingIfSessionMatches(ctx context.Context, bindingKey, expectedSessionKey string, replacement *Binding, ttl time.Duration) BindingWriteStatus {
	if s != nil && s.replaceBindingIfSessionMatchesFn != nil {
		return s.replaceBindingIfSessionMatchesFn(ctx, bindingKey, expectedSessionKey, replacement, ttl)
	}
	return BindingWriteApplied
}

func (s *stubBindingBackend) DeleteBindingIfSessionMatches(ctx context.Context, bindingKey, expectedSessionKey string) BindingWriteStatus {
	if s != nil && s.deleteBindingIfSessionMatchesFn != nil {
		return s.deleteBindingIfSessionMatchesFn(ctx, bindingKey, expectedSessionKey)
	}
	return BindingWriteApplied
}

func (s *stubBindingBackend) TouchBindingIfSessionMatches(ctx context.Context, bindingKey, expectedSessionKey string, ttl time.Duration) BindingWriteStatus {
	if s != nil && s.touchBindingIfSessionMatchesFn != nil {
		return s.touchBindingIfSessionMatchesFn(ctx, bindingKey, expectedSessionKey, ttl)
	}
	return BindingWriteApplied
}

func (s *stubBindingBackend) DeleteBindingAndRevokeIfSessionMatches(ctx context.Context, bindingKey, expectedSessionKey string, revokeTTL time.Duration) BindingWriteStatus {
	if s != nil && s.deleteBindingAndRevokeIfSessionMatch != nil {
		return s.deleteBindingAndRevokeIfSessionMatch(ctx, bindingKey, expectedSessionKey, revokeTTL)
	}
	return BindingWriteApplied
}

func (s *stubBindingBackend) CountBindings(ctx context.Context) int64 {
	if s != nil && s.countBindingsFn != nil {
		return s.countBindingsFn(ctx)
	}
	return 0
}

func setTestBackend(manager *Manager, backend bindingBackend) {
	if manager == nil {
		return
	}
	manager.remoteMu.Lock()
	manager.remoteConfig.backend = backend
	manager.remoteConfig.revocationTimeout = normalizeRevocationTimeout(manager.remoteConfig.revocationTimeout)
	manager.remoteMu.Unlock()
}

func TestManagerGetOrCreateReusesExistingSession(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)

	meta := Metadata{
		Key:       "token:1/2/session-a",
		SessionID: "session-a",
		CallerNS:  "token:1",
		ChannelID: 2,
		Model:     "gpt-5",
		Protocol:  "codex-responses-ws",
	}

	first, created, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected first GetOrCreate call to succeed, got %v", err)
	}
	if !created {
		t.Fatalf("expected first GetOrCreate call to create the session")
	}

	second, created, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected second GetOrCreate call to reuse the session, got %v", err)
	}
	if created {
		t.Fatalf("expected second GetOrCreate call to reuse the session")
	}
	if first != second {
		t.Fatalf("expected manager to return the existing session instance")
	}
}

func TestManagerSweepExpiresIdleSessionsUsingPerSessionTTL(t *testing.T) {
	var cleaned atomic.Int32
	manager := NewManager(time.Minute, 0, func(*ExecutionSession) {
		cleaned.Add(1)
	})

	meta := Metadata{
		Key:       "token:1/2/session-expire",
		SessionID: "session-expire",
		CallerNS:  "token:1",
		ChannelID: 2,
		Model:     "gpt-5",
		Protocol:  "codex-responses-ws",
		IdleTTL:   time.Second,
	}

	sess, _, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected GetOrCreate to succeed, got %v", err)
	}
	sess.Lock()
	sess.LastUsedAt = time.Now().Add(-2 * time.Second)
	sess.Unlock()

	if removed := manager.Sweep(time.Now()); removed != 1 {
		t.Fatalf("expected one expired session to be removed, got %d", removed)
	}
	if cleaned.Load() != 1 {
		t.Fatalf("expected cleanup to run once, got %d", cleaned.Load())
	}
}

func TestManagerGetOrCreateReplacesExpiredSessionImmediately(t *testing.T) {
	var cleaned atomic.Int32
	manager := NewManager(time.Minute, 0, func(*ExecutionSession) {
		cleaned.Add(1)
	})

	meta := Metadata{
		Key:       "token:1/2/session-stale",
		SessionID: "session-stale",
		CallerNS:  "token:1",
		ChannelID: 2,
		Model:     "gpt-5",
		Protocol:  "codex-responses-ws",
		IdleTTL:   time.Second,
	}

	first, created, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected first GetOrCreate call to succeed, got %v", err)
	}
	if !created {
		t.Fatalf("expected first GetOrCreate call to create the session")
	}

	first.Lock()
	first.LastUsedAt = time.Now().Add(-2 * time.Second)
	first.Unlock()

	second, created, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected replacement GetOrCreate call to succeed, got %v", err)
	}
	if !created {
		t.Fatalf("expected expired session to be replaced immediately")
	}
	if first == second {
		t.Fatalf("expected expired session instance to be replaced")
	}
	if cleaned.Load() != 1 {
		t.Fatalf("expected cleanup to run once for the expired session, got %d", cleaned.Load())
	}
}

func TestManagerDeleteIfRemovesOnlyTheExpectedSession(t *testing.T) {
	var cleaned atomic.Int32
	manager := NewManager(time.Minute, 0, func(*ExecutionSession) {
		cleaned.Add(1)
	})

	meta := Metadata{
		Key:        "token:1/2/session-delete-if",
		BindingKey: BuildBindingKey("token:1", BindingScopeChatRealtime, "session-delete-if"),
		SessionID:  "session-delete-if",
		CallerNS:   "token:1",
		ChannelID:  2,
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
	}

	first, created, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected first GetOrCreate call to succeed, got %v", err)
	}
	if !created {
		t.Fatalf("expected first GetOrCreate call to create the session")
	}

	first.Lock()
	first.MarkClosed("test_closed")
	first.Unlock()

	replacement, created, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected replacement GetOrCreate call to succeed, got %v", err)
	}
	if !created {
		t.Fatalf("expected closed session to be replaced immediately")
	}
	if replacement == first {
		t.Fatalf("expected replacement session instance")
	}
	if cleaned.Load() != 1 {
		t.Fatalf("expected cleanup to run once for the replaced session, got %d", cleaned.Load())
	}

	if removed := manager.DeleteIf(meta.Key, first); removed != nil {
		t.Fatalf("expected DeleteIf to ignore a stale session pointer")
	}
	if removed := manager.DeleteIf(meta.Key, replacement); removed != replacement {
		t.Fatalf("expected DeleteIf to remove the matching replacement session")
	}
	if cleaned.Load() != 2 {
		t.Fatalf("expected cleanup to run for the matching DeleteIf removal, got %d", cleaned.Load())
	}
}

func TestManagerDeleteIfRemovesIndexedBindingsForSession(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)

	firstBindingKey := BuildBindingKey("token:1", BindingScopeChatRealtime, "session-indexed")
	secondBindingKey := BuildBindingKey("token:1", BindingScopeChatRealtime, "session-indexed-secondary")
	meta := Metadata{
		Key:        "token:1/pool-a/hash-a/session-indexed",
		BindingKey: firstBindingKey,
		SessionID:  "session-indexed",
		CallerNS:   "token:1",
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
	}

	sess, created, err := manager.GetOrCreate(meta)
	if err != nil || !created {
		t.Fatalf("expected indexed session creation to succeed, created=%v err=%v", created, err)
	}

	manager.mu.Lock()
	sess.BindingKey = secondBindingKey
	manager.upsertBindingLocked(sess)
	manager.mu.Unlock()

	if binding, ok := manager.Resolve(firstBindingKey); !ok || binding == nil {
		t.Fatal("expected original binding to remain resolvable before deletion")
	}
	if binding, ok := manager.Resolve(secondBindingKey); !ok || binding == nil {
		t.Fatal("expected secondary binding to remain resolvable before deletion")
	}

	if removed := manager.DeleteIf(meta.Key, sess); removed != sess {
		t.Fatalf("expected DeleteIf to remove indexed session, got %#v", removed)
	}

	if binding, ok := manager.Resolve(firstBindingKey); ok || binding != nil {
		t.Fatalf("expected original indexed binding to be removed")
	}
	if binding, ok := manager.Resolve(secondBindingKey); ok || binding != nil {
		t.Fatalf("expected secondary indexed binding to be removed")
	}
	if got := len(manager.index); got != 0 {
		t.Fatalf("expected reverse index to be cleared, got %d sessions", got)
	}
}

func TestManagerInvalidateBindingDeletesBoundSessionAndRunsCleanup(t *testing.T) {
	var cleaned atomic.Int32
	manager := NewManager(time.Minute, 0, func(*ExecutionSession) {
		cleaned.Add(1)
	})

	meta := Metadata{
		Key:        "channel:2/hash-a/session-force-fresh",
		BindingKey: BuildBindingKey("token:1", BindingScopeChatRealtime, "session-force-fresh"),
		SessionID:  "session-force-fresh",
		CallerNS:   "token:1",
		ChannelID:  2,
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
	}

	sess, created, err := manager.GetOrCreate(meta)
	if err != nil || !created {
		t.Fatalf("expected initial session creation to succeed, created=%v err=%v", created, err)
	}

	invalidated := manager.InvalidateBinding(meta.BindingKey)
	if invalidated == nil {
		t.Fatal("expected InvalidateBinding to return the invalidated binding")
	}
	if invalidated.SessionKey != sess.Key {
		t.Fatalf("expected invalidated binding session key %q, got %q", sess.Key, invalidated.SessionKey)
	}
	if cleaned.Load() != 1 {
		t.Fatalf("expected cleanup to run once for the invalidated session, got %d", cleaned.Load())
	}
	if binding, ok := manager.Resolve(meta.BindingKey); ok || binding != nil {
		t.Fatalf("expected binding to be removed after invalidation")
	}
	if got := len(manager.sessions); got != 0 {
		t.Fatalf("expected invalidated session to be removed from the manager, got %d", got)
	}
	if got := len(manager.capacityIndex); got != 0 {
		t.Fatalf("expected invalidated session to be removed from caller capacity tracking, got %d", got)
	}
}

func TestManagerGetOrCreateBoundRejectsConflictingBindingOwnerAtomically(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)

	bindingKey := BuildBindingKey("token:1", BindingScopeChatRealtime, "session-conflict")
	metaA := Metadata{
		Key:               "channel:2/hash-a/session-conflict",
		BindingKey:        bindingKey,
		SessionID:         "session-conflict",
		CallerNS:          "token:1",
		ChannelID:         2,
		CompatibilityHash: "hash-a",
		Model:             "gpt-5",
		Protocol:          "codex-responses-ws",
	}
	metaB := Metadata{
		Key:               "channel:3/hash-b/session-conflict",
		BindingKey:        bindingKey,
		SessionID:         "session-conflict",
		CallerNS:          "token:1",
		ChannelID:         3,
		CompatibilityHash: "hash-b",
		Model:             "gpt-5",
		Protocol:          "codex-responses-ws",
	}

	type result struct {
		meta     Metadata
		session  *ExecutionSession
		created  bool
		conflict *Binding
	}

	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, meta := range []Metadata{metaA, metaB} {
		wg.Add(1)
		go func(meta Metadata) {
			defer wg.Done()
			<-start
			sess, created, conflict, err := manager.GetOrCreateBound(meta)
			if err != nil {
				t.Errorf("expected GetOrCreateBound to succeed, got %v", err)
				return
			}
			results <- result{
				meta:     meta,
				session:  sess,
				created:  created,
				conflict: conflict,
			}
		}(meta)
	}

	close(start)
	wg.Wait()
	close(results)

	var created []result
	var conflicts []result
	for result := range results {
		switch {
		case result.conflict != nil:
			conflicts = append(conflicts, result)
		case result.session != nil:
			created = append(created, result)
		default:
			t.Fatalf("expected either a created session or a binding conflict for %+v", result.meta)
		}
	}

	if len(created) != 1 {
		t.Fatalf("expected exactly one live binding owner, got %d", len(created))
	}
	if len(conflicts) != 1 {
		t.Fatalf("expected exactly one binding conflict, got %d", len(conflicts))
	}
	if conflicts[0].conflict.SessionKey != created[0].meta.Key {
		t.Fatalf("expected conflict to point at the winning session key %q, got %q", created[0].meta.Key, conflicts[0].conflict.SessionKey)
	}

	binding, ok := manager.Resolve(bindingKey)
	if !ok || binding == nil {
		t.Fatal("expected live binding owner after concurrent acquisition")
	}
	if binding.SessionKey != created[0].meta.Key {
		t.Fatalf("expected binding to remain pinned to %q, got %q", created[0].meta.Key, binding.SessionKey)
	}
}

func TestManagerUpsertBindingReassignsReverseIndexWhenBindingOwnerChanges(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)

	bindingKey := BuildBindingKey("token:1", BindingScopeChatRealtime, "session-reassigned")
	metaA := Metadata{
		Key:        "token:1/pool-a/hash-a/session-reassigned-a",
		BindingKey: bindingKey,
		SessionID:  "session-reassigned",
		CallerNS:   "token:1",
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
	}
	metaB := Metadata{
		Key:        "token:1/pool-b/hash-b/session-reassigned-b",
		BindingKey: bindingKey,
		SessionID:  "session-reassigned",
		CallerNS:   "token:1",
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
	}

	first, created, err := manager.GetOrCreate(metaA)
	if err != nil || !created {
		t.Fatalf("expected first binding owner creation to succeed, created=%v err=%v", created, err)
	}

	second := NewExecutionSession(metaB)
	manager.mu.Lock()
	manager.sessions[second.Key] = second
	manager.upsertBindingLocked(second)
	manager.mu.Unlock()

	if removed := manager.DeleteIf(first.Key, first); removed != first {
		t.Fatalf("expected DeleteIf to remove the superseded first owner, got %#v", removed)
	}

	binding, ok := manager.Resolve(bindingKey)
	if !ok || binding == nil {
		t.Fatal("expected reassigned binding to remain resolvable")
	}
	if binding.SessionKey != second.Key {
		t.Fatalf("expected binding to move to %q, got %q", second.Key, binding.SessionKey)
	}
	if _, ok := manager.index[first.Key]; ok {
		t.Fatalf("expected superseded owner to be removed from reverse index")
	}
}

func TestManagerStatsReportsLocalSessionAndBindingCounts(t *testing.T) {
	manager := NewManagerWithOptions(ManagerOptions{
		DefaultTTL:           2 * time.Minute,
		MaxSessions:          8,
		MaxSessionsPerCaller: 2,
	})

	firstMeta := Metadata{
		Key:        "channel:2/hash-a/session-stats-a",
		BindingKey: BuildBindingKey("token:1", BindingScopeChatRealtime, "session-stats-a"),
		SessionID:  "session-stats-a",
		CallerNS:   "token:1",
		ChannelID:  2,
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
	}
	secondMeta := Metadata{
		Key:        "channel:3/hash-b/session-stats-b",
		BindingKey: BuildBindingKey("token:1", BindingScopeChatRealtime, "session-stats-b"),
		SessionID:  "session-stats-b",
		CallerNS:   "token:1",
		ChannelID:  3,
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
	}

	if _, _, err := manager.GetOrCreate(firstMeta); err != nil {
		t.Fatalf("expected first session creation to succeed, got %v", err)
	}
	if _, _, err := manager.GetOrCreate(secondMeta); err != nil {
		t.Fatalf("expected second session creation to succeed, got %v", err)
	}

	stats := manager.Stats()
	if stats.Backend != "memory" {
		t.Fatalf("expected memory backend, got %q", stats.Backend)
	}
	if stats.LocalSessions != 2 {
		t.Fatalf("expected 2 local sessions, got %d", stats.LocalSessions)
	}
	if stats.LocalBindings != 2 {
		t.Fatalf("expected 2 local bindings, got %d", stats.LocalBindings)
	}
	if stats.MaxSessions != 8 {
		t.Fatalf("expected max_sessions 8, got %d", stats.MaxSessions)
	}
	if stats.MaxSessionsPerCaller != 2 {
		t.Fatalf("expected max_sessions_per_caller 2, got %d", stats.MaxSessionsPerCaller)
	}
	if stats.DefaultTTLSeconds != int64((2*time.Minute)/time.Second) {
		t.Fatalf("unexpected default ttl seconds: %d", stats.DefaultTTLSeconds)
	}
}

func TestManagerResolveDropsClosedSessionsImmediately(t *testing.T) {
	var cleaned atomic.Int32
	manager := NewManager(time.Minute, 0, func(*ExecutionSession) {
		cleaned.Add(1)
	})

	meta := Metadata{
		Key:        "token:1/2/session-resolve-closed",
		BindingKey: BuildBindingKey("token:1", BindingScopeChatRealtime, "session-resolve-closed"),
		SessionID:  "session-resolve-closed",
		CallerNS:   "token:1",
		ChannelID:  2,
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
	}

	sess, _, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected GetOrCreate to succeed, got %v", err)
	}
	sess.Lock()
	sess.MarkClosed("test_closed")
	sess.Unlock()

	if binding, ok := manager.Resolve(meta.BindingKey); ok || binding != nil {
		t.Fatalf("expected Resolve to reject a closed session binding")
	}
	if cleaned.Load() != 1 {
		t.Fatalf("expected cleanup to run when Resolve drops the closed session, got %d", cleaned.Load())
	}

	replacement, created, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected replacement GetOrCreate call to succeed, got %v", err)
	}
	if !created {
		t.Fatalf("expected Resolve cleanup to allow a fresh session to be created immediately")
	}
	if replacement == sess {
		t.Fatalf("expected closed session instance to be removed before recreation")
	}
}

func TestManagerRejectsNewSessionsWhenCapacityIsReached(t *testing.T) {
	manager := NewManagerWithOptions(ManagerOptions{
		DefaultTTL:  time.Minute,
		MaxSessions: 1,
	})

	firstMeta := Metadata{
		Key:       "token:1/2/session-cap-1",
		SessionID: "session-cap-1",
		CallerNS:  "token:1",
		ChannelID: 2,
		Model:     "gpt-5",
		Protocol:  "codex-responses-ws",
	}
	secondMeta := Metadata{
		Key:       "token:1/2/session-cap-2",
		SessionID: "session-cap-2",
		CallerNS:  "token:1",
		ChannelID: 2,
		Model:     "gpt-5",
		Protocol:  "codex-responses-ws",
	}

	if _, created, err := manager.GetOrCreate(firstMeta); err != nil || !created {
		t.Fatalf("expected first session creation to succeed, created=%v err=%v", created, err)
	}

	if _, created, err := manager.GetOrCreate(firstMeta); err != nil || created {
		t.Fatalf("expected existing session reuse at capacity, created=%v err=%v", created, err)
	}

	if _, created, err := manager.GetOrCreate(secondMeta); !errors.Is(err, ErrCapacityExceeded) || created {
		t.Fatalf("expected capacity error for second session, created=%v err=%v", created, err)
	}
}

func TestManagerRejectsNewSessionsWhenCallerCapacityIsReached(t *testing.T) {
	manager := NewManagerWithOptions(ManagerOptions{
		DefaultTTL:           time.Minute,
		MaxSessions:          4,
		MaxSessionsPerCaller: 1,
	})

	firstMeta := Metadata{
		Key:        "token:1/2/session-caller-cap-1",
		SessionID:  "session-caller-cap-1",
		CallerNS:   "token:1",
		CapacityNS: "user:1",
		ChannelID:  2,
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
	}
	secondMeta := Metadata{
		Key:        "token:2/2/session-caller-cap-2",
		SessionID:  "session-caller-cap-2",
		CallerNS:   "token:2",
		CapacityNS: "user:1",
		ChannelID:  2,
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
	}
	otherCallerMeta := Metadata{
		Key:        "token:3/2/session-caller-cap-1",
		SessionID:  "session-caller-cap-1",
		CallerNS:   "token:3",
		CapacityNS: "user:2",
		ChannelID:  2,
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
	}

	if _, created, err := manager.GetOrCreate(firstMeta); err != nil || !created {
		t.Fatalf("expected first caller session creation to succeed, created=%v err=%v", created, err)
	}
	if _, created, err := manager.GetOrCreate(firstMeta); err != nil || created {
		t.Fatalf("expected existing caller session reuse at caller capacity, created=%v err=%v", created, err)
	}
	if _, created, err := manager.GetOrCreate(secondMeta); !errors.Is(err, ErrCallerCapacityExceeded) || created {
		t.Fatalf("expected caller capacity error for second capacity-owned session, created=%v err=%v", created, err)
	}
	if _, created, err := manager.GetOrCreate(otherCallerMeta); err != nil || !created {
		t.Fatalf("expected other capacity namespace to keep independent capacity, created=%v err=%v", created, err)
	}
}

func TestManagerResolvePreservesEscapedBindingSegments(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)

	callerNS := "auth:tenant/a"
	scope := "chat/realtime"
	sessionID := "session/a/b"
	bindingKey := BuildBindingKey(callerNS, scope, sessionID)

	meta := Metadata{
		Key:        "auth-tenant/session-escaped",
		BindingKey: bindingKey,
		SessionID:  sessionID,
		CallerNS:   callerNS,
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
	}

	if _, created, conflict, err := manager.GetOrCreateBound(meta); err != nil || !created || conflict != nil {
		t.Fatalf("expected escaped binding segments to create successfully, created=%v conflict=%+v err=%v", created, conflict, err)
	}

	binding, ok := manager.Resolve(bindingKey)
	if !ok || binding == nil {
		t.Fatal("expected escaped binding key to resolve")
	}
	if binding.Scope != scope {
		t.Fatalf("expected scope %q, got %q", scope, binding.Scope)
	}
	if binding.CallerNS != callerNS {
		t.Fatalf("expected caller namespace %q, got %q", callerNS, binding.CallerNS)
	}
	if binding.SessionID != sessionID {
		t.Fatalf("expected session id %q, got %q", sessionID, binding.SessionID)
	}
}

func TestManagerAcquireOrCreateBoundKeepsLeasedSessionAliveAcrossSweep(t *testing.T) {
	manager := NewManager(5*time.Millisecond, 0, nil)

	meta := Metadata{
		Key:        "token:1/pool-a/hash-a/session-leased",
		BindingKey: BuildBindingKey("token:1", BindingScopeChatRealtime, "session-leased"),
		SessionID:  "session-leased",
		CallerNS:   "token:1",
		ChannelID:  2,
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
		IdleTTL:    5 * time.Millisecond,
	}

	sess, created, conflict, release, err := manager.AcquireOrCreateBound(meta)
	if err != nil {
		t.Fatalf("expected AcquireOrCreateBound to succeed, got %v", err)
	}
	if !created || conflict != nil {
		t.Fatalf("expected leased session creation without conflict, created=%v conflict=%+v", created, conflict)
	}
	if release == nil {
		t.Fatal("expected a lease release function")
	}

	time.Sleep(10 * time.Millisecond)
	if removed := manager.Sweep(time.Now()); removed != 0 {
		t.Fatalf("expected sweep to preserve a leased session, removed=%d", removed)
	}

	release()
	if removed := manager.Sweep(time.Now()); removed != 1 {
		t.Fatalf("expected sweep to remove the expired session after lease release, removed=%d", removed)
	}
	if binding, ok := manager.Resolve(meta.BindingKey); ok || binding != nil {
		t.Fatalf("expected released expired leased session binding to be removed")
	}
	if removed := manager.DeleteIf(meta.Key, sess); removed != nil {
		t.Fatalf("expected DeleteIf to ignore the stale leased pointer after sweep")
	}
}

func TestManagerResolveBindingDoesNotFallbackToLocalOnBackendError(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)
	setTestBackend(manager, &stubBindingBackend{
		resolveBindingFn: func(context.Context, string) (*Binding, ResolveStatus) {
			return nil, ResolveBackendError
		},
	})

	bindingKey := BuildBindingKey("token:1", BindingScopeChatRealtime, "session-backend-error")
	meta := Metadata{
		Key:        "channel:2/hash-a/session-backend-error",
		BindingKey: bindingKey,
		SessionID:  "session-backend-error",
		CallerNS:   "token:1",
		ChannelID:  2,
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
	}

	if _, _, err := manager.GetOrCreate(meta); err != nil {
		t.Fatalf("expected local session creation, got %v", err)
	}

	binding, status := manager.ResolveBinding(bindingKey)
	if status != ResolveBackendError || binding != nil {
		t.Fatalf("expected backend_error without local fallback, got status=%q binding=%+v", status, binding)
	}
	if localBinding, ok := manager.ResolveLocal(bindingKey); !ok || localBinding == nil || localBinding.SessionKey != meta.Key {
		t.Fatalf("expected local near-cache binding to remain accessible, got %+v ok=%v", localBinding, ok)
	}
}

func TestManagerTouchBindingConditionMismatchClearsLocalBinding(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)
	setTestBackend(manager, &stubBindingBackend{
		touchBindingIfSessionMatchesFn: func(context.Context, string, string, time.Duration) BindingWriteStatus {
			return BindingWriteConditionMismatch
		},
	})

	bindingKey := BuildBindingKey("token:1", BindingScopeChatRealtime, "session-touch-mismatch")
	meta := Metadata{
		Key:        "channel:2/hash-a/session-touch-mismatch",
		BindingKey: bindingKey,
		SessionID:  "session-touch-mismatch",
		CallerNS:   "token:1",
		ChannelID:  2,
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
	}

	if _, _, err := manager.GetOrCreate(meta); err != nil {
		t.Fatalf("expected local session creation, got %v", err)
	}
	manager.TouchBinding(bindingKey, meta.Key, 3)

	if binding, ok := manager.ResolveLocal(bindingKey); ok || binding != nil {
		t.Fatalf("expected condition mismatch to clear local near-cache binding, got %+v", binding)
	}
}

func TestManagerTouchBindingBackendErrorPreservesLiveSessionAndMarksUncertain(t *testing.T) {
	manager := NewManager(time.Minute, 0, nil)
	setTestBackend(manager, &stubBindingBackend{
		touchBindingIfSessionMatchesFn: func(context.Context, string, string, time.Duration) BindingWriteStatus {
			return BindingWriteBackendError
		},
	})

	bindingKey := BuildBindingKey("token:1", BindingScopeChatRealtime, "session-touch-backend-error")
	meta := Metadata{
		Key:        "channel:2/hash-a/session-touch-backend-error",
		BindingKey: bindingKey,
		SessionID:  "session-touch-backend-error",
		CallerNS:   "token:1",
		ChannelID:  2,
		Model:      "gpt-5",
		Protocol:   "codex-responses-ws",
	}

	sess, _, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected local session creation, got %v", err)
	}
	manager.TouchBinding(bindingKey, meta.Key, 3)

	if binding, ok := manager.ResolveLocal(bindingKey); !ok || binding == nil || binding.SessionKey != meta.Key {
		t.Fatalf("expected backend error to preserve local near-cache binding, got %+v ok=%v", binding, ok)
	}
	sess.Lock()
	defer sess.Unlock()
	if !sess.SharedStateUncertain {
		t.Fatal("expected backend error to mark shared state uncertain")
	}
}
