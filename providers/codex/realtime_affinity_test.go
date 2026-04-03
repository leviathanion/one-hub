package codex

import (
	stderrors "errors"
	"net/http"
	"strings"
	"testing"
	"time"

	commonredis "one-api/common/redis"
	"one-api/internal/testutil/fakeredis"
	runtimesession "one-api/runtime/session"
)

func withCodexExecutionManager(t *testing.T, manager *runtimesession.Manager) {
	t.Helper()
	originalManager := codexExecutionSessions
	codexExecutionSessions = manager
	t.Cleanup(func() {
		codexExecutionSessions = originalManager
		manager.Close()
	})
}

func newCodexFakeRedisManager(t *testing.T) (*runtimesession.Manager, *fakeredis.Server, string) {
	t.Helper()
	server, err := fakeredis.Start()
	if err != nil {
		t.Fatalf("failed to start fake redis: %v", err)
	}
	prefix := "test:codex-affinity"
	manager := runtimesession.NewManagerWithOptions(runtimesession.ManagerOptions{
		DefaultTTL:      time.Minute,
		Cleanup:         cleanupCodexExecutionSession,
		RedisClient:     server.Client(),
		RedisPrefix:     prefix,
		JanitorInterval: 0,
	})
	withCodexExecutionManager(t, manager)
	t.Cleanup(func() {
		_ = server.Close()
	})
	return manager, server, prefix
}

func codexTestMeta(t *testing.T, provider *CodexProvider) runtimesession.Metadata {
	t.Helper()
	meta, errWithCode := provider.buildExecutionSessionMetadata("gpt-5", runtimesession.RealtimeOpenOptions{})
	if errWithCode != nil {
		t.Fatalf("expected execution session metadata, got %v", errWithCode)
	}
	return meta
}

func TestCodexPlanRealtimeOpenBranches(t *testing.T) {
	t.Run("miss publishes create_if_absent", func(t *testing.T) {
		manager, _, _ := newCodexFakeRedisManager(t)
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{"X-Session-Id": "plan-miss"})
		provider.Context.Set("token_id", 401)
		meta := codexTestMeta(t, provider)

		plan := provider.planRealtimeOpen(meta, runtimesession.RealtimeOpenOptions{})
		if plan.publishIntent != runtimesession.PublishIntentCreateIfAbsent || plan.candidateSessionKey != "" {
			t.Fatalf("expected miss plan to publish create_if_absent, got %+v (manager=%p)", plan, manager)
		}
	})

	t.Run("compatible hit resumes existing candidate", func(t *testing.T) {
		manager, _, _ := newCodexFakeRedisManager(t)
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{"X-Session-Id": "plan-hit"})
		provider.Context.Set("token_id", 402)
		meta := codexTestMeta(t, provider)
		binding := (&runtimesession.ExecutionSession{
			Key:               meta.Key,
			BindingKey:        meta.BindingKey,
			CallerNS:          meta.CallerNS,
			ChannelID:         meta.ChannelID,
			CompatibilityHash: meta.CompatibilityHash,
		}).BuildBinding()
		if status := manager.CreateBindingIfAbsent(binding, time.Minute); status != runtimesession.BindingWriteApplied {
			t.Fatalf("expected shared binding fixture, got %q", status)
		}

		plan := provider.planRealtimeOpen(meta, runtimesession.RealtimeOpenOptions{})
		if plan.candidateSessionKey != binding.SessionKey || !plan.sharedHitCompatible || plan.publishIntent != runtimesession.PublishIntentNone {
			t.Fatalf("expected compatible hit plan to reuse candidate, got %+v", plan)
		}
	})

	t.Run("compatible hit with unknown revocation avoids resume", func(t *testing.T) {
		manager, server, _ := newCodexFakeRedisManager(t)
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{"X-Session-Id": "plan-unknown-revocation"})
		provider.Context.Set("token_id", 403)
		meta := codexTestMeta(t, provider)
		if status := manager.CreateBindingIfAbsent((&runtimesession.ExecutionSession{
			Key:               meta.Key,
			BindingKey:        meta.BindingKey,
			CallerNS:          meta.CallerNS,
			ChannelID:         meta.ChannelID,
			CompatibilityHash: meta.CompatibilityHash,
		}).BuildBinding(), time.Minute); status != runtimesession.BindingWriteApplied {
			t.Fatalf("expected shared binding fixture, got %q", status)
		}
		server.FailNext("EXISTS", "ERR forced exists failure")

		plan := provider.planRealtimeOpen(meta, runtimesession.RealtimeOpenOptions{})
		if plan.candidateSessionKey != "" || plan.publishIntent != runtimesession.PublishIntentNone {
			t.Fatalf("expected revocation-unknown plan to avoid resume and publish, got %+v", plan)
		}
	})

	t.Run("incompatible hit plans replace_if_matches", func(t *testing.T) {
		manager, _, prefix := newCodexFakeRedisManager(t)
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{"X-Session-Id": "plan-incompatible"})
		provider.Context.Set("token_id", 404)
		meta := codexTestMeta(t, provider)
		incompatible := (&runtimesession.ExecutionSession{
			Key:               "channel:99/hash-old/session-old",
			BindingKey:        meta.BindingKey,
			CallerNS:          meta.CallerNS,
			ChannelID:         99,
			CompatibilityHash: "hash-old",
		}).BuildBinding()
		if status := manager.CreateBindingIfAbsent(incompatible, time.Minute); status != runtimesession.BindingWriteApplied {
			t.Fatalf("expected incompatible shared binding fixture, got %q", status)
		}

		plan := provider.planRealtimeOpen(meta, runtimesession.RealtimeOpenOptions{})
		if plan.publishIntent != runtimesession.PublishIntentReplaceIfMatch || plan.expectedOldSessionKey != incompatible.SessionKey || plan.candidateSessionKey != "" {
			t.Fatalf("expected incompatible hit plan to replace old binding, got %+v (prefix=%s)", plan, prefix)
		}
	})

	t.Run("backend error returns empty plan", func(t *testing.T) {
		_, server, _ := newCodexFakeRedisManager(t)
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{"X-Session-Id": "plan-backend-error"})
		provider.Context.Set("token_id", 405)
		meta := codexTestMeta(t, provider)
		server.FailNext("GET", "ERR forced get failure")

		plan := provider.planRealtimeOpen(meta, runtimesession.RealtimeOpenOptions{})
		if plan.candidateSessionKey != "" || plan.publishIntent != runtimesession.PublishIntentNone || plan.expectedOldSessionKey != "" {
			t.Fatalf("expected backend error to return empty plan, got %+v", plan)
		}
	})

	t.Run("revoked compatible hit plans legal replace", func(t *testing.T) {
		manager, server, prefix := newCodexFakeRedisManager(t)
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{"X-Session-Id": "plan-revoked"})
		provider.Context.Set("token_id", 406)
		meta := codexTestMeta(t, provider)
		binding := (&runtimesession.ExecutionSession{
			Key:               meta.Key,
			BindingKey:        meta.BindingKey,
			CallerNS:          meta.CallerNS,
			ChannelID:         meta.ChannelID,
			CompatibilityHash: meta.CompatibilityHash,
		}).BuildBinding()
		if status := manager.CreateBindingIfAbsent(binding, time.Minute); status != runtimesession.BindingWriteApplied {
			t.Fatalf("expected shared binding fixture, got %q", status)
		}
		server.SetRaw(prefix+":revoked:"+meta.Key, "1")

		plan := provider.planRealtimeOpen(meta, runtimesession.RealtimeOpenOptions{})
		if plan.publishIntent != runtimesession.PublishIntentReplaceIfMatch || plan.expectedOldSessionKey != meta.Key {
			t.Fatalf("expected revoked plan to replace old binding, got %+v", plan)
		}
	})
}

func TestCodexPlanForceFreshRealtimeOpenBranches(t *testing.T) {
	t.Run("miss publishes create_if_absent", func(t *testing.T) {
		_, _, _ = newCodexFakeRedisManager(t)
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{"X-Session-Id": "force-fresh-miss"})
		provider.Context.Set("token_id", 407)
		meta := codexTestMeta(t, provider)
		if plan := provider.planForceFreshRealtimeOpen(meta); plan.publishIntent != runtimesession.PublishIntentCreateIfAbsent {
			t.Fatalf("expected force-fresh miss plan to publish create_if_absent, got %+v", plan)
		}
	})

	t.Run("hit revokes shared binding and deletes local session", func(t *testing.T) {
		manager, _, _ := newCodexFakeRedisManager(t)
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{"X-Session-Id": "force-fresh-hit"})
		provider.Context.Set("token_id", 408)
		meta := codexTestMeta(t, provider)
		sess, _, err := manager.GetOrCreate(meta)
		if err != nil {
			t.Fatalf("expected local execution session fixture, got %v", err)
		}
		if status := manager.CreateBindingIfAbsent(sess.BuildBinding(), time.Minute); status != runtimesession.BindingWriteApplied {
			t.Fatalf("expected shared binding fixture, got %q", status)
		}

		plan := provider.planForceFreshRealtimeOpen(meta)
		if plan.publishIntent != runtimesession.PublishIntentCreateIfAbsent {
			t.Fatalf("expected force-fresh hit to publish new binding, got %+v", plan)
		}
		if removed := manager.DeleteIf(meta.Key, sess); removed != nil {
			t.Fatalf("expected force-fresh planning to remove stale local session, got %+v", removed)
		}
	})

	t.Run("backend error returns empty plan", func(t *testing.T) {
		_, server, _ := newCodexFakeRedisManager(t)
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{"X-Session-Id": "force-fresh-backend-error"})
		provider.Context.Set("token_id", 409)
		meta := codexTestMeta(t, provider)
		server.FailNext("GET", "ERR forced get failure")

		if plan := provider.planForceFreshRealtimeOpen(meta); plan.candidateSessionKey != "" || plan.publishIntent != runtimesession.PublishIntentNone || plan.expectedOldSessionKey != "" {
			t.Fatalf("expected backend error to produce empty force-fresh plan, got %+v", plan)
		}
	})
}

func TestCodexMaybePromoteExecutionSessionBranches(t *testing.T) {
	t.Run("backend error leaves session local_only", func(t *testing.T) {
		manager, server, _ := newCodexFakeRedisManager(t)
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{"X-Session-Id": "promote-backend-error"})
		provider.Context.Set("token_id", 410)
		meta := codexTestMeta(t, provider)
		exec, _, err := manager.GetOrCreate(meta)
		if err != nil {
			t.Fatalf("expected local execution session fixture, got %v", err)
		}
		exec.Lock()
		codexMarkExecutionSessionLocalOnlyLocked(exec, runtimesession.PublishIntentCreateIfAbsent, "")
		exec.Unlock()
		server.FailNext("GET", "ERR forced get failure")

		codexMaybePromoteExecutionSession(exec)
		exec.Lock()
		defer exec.Unlock()
		if exec.Visibility != runtimesession.VisibilityLocalOnly || exec.PublishIntent != runtimesession.PublishIntentCreateIfAbsent {
			t.Fatalf("expected backend error to preserve local-only publish intent, got visibility=%q intent=%q", exec.Visibility, exec.PublishIntent)
		}
	})

	t.Run("existing shared binding marks session shared", func(t *testing.T) {
		manager, _, _ := newCodexFakeRedisManager(t)
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{"X-Session-Id": "promote-shared"})
		provider.Context.Set("token_id", 411)
		meta := codexTestMeta(t, provider)
		exec, _, err := manager.GetOrCreate(meta)
		if err != nil {
			t.Fatalf("expected local execution session fixture, got %v", err)
		}
		exec.Lock()
		codexMarkExecutionSessionLocalOnlyLocked(exec, runtimesession.PublishIntentCreateIfAbsent, "stale")
		exec.SharedStateUncertain = true
		exec.Unlock()
		if status := manager.CreateBindingIfAbsent(exec.BuildBinding(), time.Minute); status != runtimesession.BindingWriteApplied {
			t.Fatalf("expected shared binding fixture, got %q", status)
		}

		codexMaybePromoteExecutionSession(exec)
		exec.Lock()
		defer exec.Unlock()
		if exec.Visibility != runtimesession.VisibilityShared || exec.PublishIntent != runtimesession.PublishIntentNone || exec.ExpectedOldSessionKey != "" || exec.SharedStateUncertain {
			t.Fatalf("expected matching shared binding to mark session shared, got visibility=%q intent=%q expected_old=%q uncertain=%v", exec.Visibility, exec.PublishIntent, exec.ExpectedOldSessionKey, exec.SharedStateUncertain)
		}
	})

	t.Run("create_if_absent conflict stops republish", func(t *testing.T) {
		manager, _, _ := newCodexFakeRedisManager(t)
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{"X-Session-Id": "promote-create-conflict"})
		provider.Context.Set("token_id", 412)
		meta := codexTestMeta(t, provider)
		exec, _, err := manager.GetOrCreate(meta)
		if err != nil {
			t.Fatalf("expected local execution session fixture, got %v", err)
		}
		exec.Lock()
		codexMarkExecutionSessionLocalOnlyLocked(exec, runtimesession.PublishIntentCreateIfAbsent, "")
		exec.Unlock()
		otherBinding := (&runtimesession.ExecutionSession{Key: "channel:2/hash-a/other", BindingKey: meta.BindingKey, CallerNS: meta.CallerNS, ChannelID: meta.ChannelID, CompatibilityHash: meta.CompatibilityHash}).BuildBinding()
		if status := manager.CreateBindingIfAbsent(otherBinding, time.Minute); status != runtimesession.BindingWriteApplied {
			t.Fatalf("expected conflicting shared binding fixture, got %q", status)
		}

		codexMaybePromoteExecutionSession(exec)
		exec.Lock()
		defer exec.Unlock()
		if exec.Visibility != runtimesession.VisibilityLocalOnly || exec.PublishIntent != runtimesession.PublishIntentNone {
			t.Fatalf("expected create-if-absent conflict to stop republish, got visibility=%q intent=%q", exec.Visibility, exec.PublishIntent)
		}
	})

	t.Run("replace_if_matches success promotes to shared", func(t *testing.T) {
		manager, _, _ := newCodexFakeRedisManager(t)
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{"X-Session-Id": "promote-replace-success"})
		provider.Context.Set("token_id", 413)
		meta := codexTestMeta(t, provider)
		exec, _, err := manager.GetOrCreate(meta)
		if err != nil {
			t.Fatalf("expected local execution session fixture, got %v", err)
		}
		oldSessionKey := "channel:2/hash-a/old"
		exec.Lock()
		codexMarkExecutionSessionLocalOnlyLocked(exec, runtimesession.PublishIntentReplaceIfMatch, oldSessionKey)
		exec.Unlock()
		oldBinding := (&runtimesession.ExecutionSession{Key: oldSessionKey, BindingKey: meta.BindingKey, CallerNS: meta.CallerNS, ChannelID: meta.ChannelID, CompatibilityHash: meta.CompatibilityHash}).BuildBinding()
		if status := manager.CreateBindingIfAbsent(oldBinding, time.Minute); status != runtimesession.BindingWriteApplied {
			t.Fatalf("expected old shared binding fixture, got %q", status)
		}

		codexMaybePromoteExecutionSession(exec)
		exec.Lock()
		defer exec.Unlock()
		if exec.Visibility != runtimesession.VisibilityShared || exec.PublishIntent != runtimesession.PublishIntentNone {
			t.Fatalf("expected replace-if-match success to promote session, got visibility=%q intent=%q", exec.Visibility, exec.PublishIntent)
		}
	})

	t.Run("replace_if_matches mismatch stops republish", func(t *testing.T) {
		manager, _, _ := newCodexFakeRedisManager(t)
		provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{"X-Session-Id": "promote-replace-mismatch"})
		provider.Context.Set("token_id", 414)
		meta := codexTestMeta(t, provider)
		exec, _, err := manager.GetOrCreate(meta)
		if err != nil {
			t.Fatalf("expected local execution session fixture, got %v", err)
		}
		exec.Lock()
		codexMarkExecutionSessionLocalOnlyLocked(exec, runtimesession.PublishIntentReplaceIfMatch, "channel:2/hash-a/expected-old")
		exec.Unlock()
		otherBinding := (&runtimesession.ExecutionSession{Key: "channel:2/hash-a/other", BindingKey: meta.BindingKey, CallerNS: meta.CallerNS, ChannelID: meta.ChannelID, CompatibilityHash: meta.CompatibilityHash}).BuildBinding()
		if status := manager.CreateBindingIfAbsent(otherBinding, time.Minute); status != runtimesession.BindingWriteApplied {
			t.Fatalf("expected conflicting shared binding fixture, got %q", status)
		}

		codexMaybePromoteExecutionSession(exec)
		exec.Lock()
		defer exec.Unlock()
		if exec.Visibility != runtimesession.VisibilityLocalOnly || exec.PublishIntent != runtimesession.PublishIntentNone {
			t.Fatalf("expected replace-if-match mismatch to stop republish, got visibility=%q intent=%q", exec.Visibility, exec.PublishIntent)
		}
	})
}

func TestCodexAcquireLocalOnlyExecutionSessionBranches(t *testing.T) {
	manager, _, _ := newCodexFakeRedisManager(t)
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{"X-Session-Id": "acquire-local-only"})
	provider.Context.Set("token_id", 415)
	meta := codexTestMeta(t, provider)

	if exec, release, ok := codexAcquireLocalOnlyExecutionSession("", ""); ok || exec != nil || release != nil {
		t.Fatalf("expected blank binding key lookup to fail, exec=%+v ok=%v", exec, ok)
	}

	sharedExec, _, err := manager.GetOrCreate(meta)
	if err != nil {
		t.Fatalf("expected shared execution session fixture, got %v", err)
	}
	sharedExec.Lock()
	codexMarkExecutionSessionSharedLocked(sharedExec)
	sharedExec.Unlock()
	if exec, release, ok := codexAcquireLocalOnlyExecutionSession(meta.BindingKey, ""); ok || exec != nil || release != nil {
		t.Fatalf("expected shared execution session not to be treated as local-only, exec=%+v ok=%v", exec, ok)
	}

	localMeta := meta
	localMeta.Key = meta.Key + "-local"
	localExec, _, err := manager.GetOrCreate(localMeta)
	if err != nil {
		t.Fatalf("expected local-only execution session fixture, got %v", err)
	}
	localExec.Lock()
	codexMarkExecutionSessionLocalOnlyLocked(localExec, runtimesession.PublishIntentCreateIfAbsent, "")
	localExec.Unlock()

	if exec, release, ok := codexAcquireLocalOnlyExecutionSession(meta.BindingKey, localExec.Key); ok || exec != nil || release != nil {
		t.Fatalf("expected excluded local-only session not to be acquired, exec=%+v ok=%v", exec, ok)
	}
	if exec, release, ok := codexAcquireLocalOnlyExecutionSession(meta.BindingKey, ""); !ok || exec != localExec || release == nil {
		t.Fatalf("expected local-only execution session to be acquired, exec=%+v ok=%v", exec, ok)
	} else {
		release()
	}
}

func TestExecutionSessionStatsUsesConfiguredRedisBackend(t *testing.T) {
	manager, server, _ := newCodexFakeRedisManager(t)
	originalRedis := commonredis.RDB
	commonredis.RDB = server.Client()
	t.Cleanup(func() {
		commonredis.RDB = originalRedis
	})

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, map[string]string{"X-Session-Id": "stats"})
	provider.Context.Set("token_id", 416)
	meta := codexTestMeta(t, provider)
	manager.ConfigureRedis(server.Client(), "one-hub:execution-session")
	if exec, _, err := manager.GetOrCreate(meta); err != nil {
		t.Fatalf("expected execution session fixture, got %v", err)
	} else if status := manager.CreateBindingIfAbsent(exec.BuildBinding(), time.Minute); status != runtimesession.BindingWriteApplied {
		t.Fatalf("expected shared binding fixture, got %q", status)
	}

	stats := ExecutionSessionStats()
	if stats.Backend != "redis" || stats.BackendBindings < 1 {
		t.Fatalf("expected execution session stats to reflect redis backend, got %+v", stats)
	}
}

func TestCodexRealtimeSessionIDAndErrorHelpers(t *testing.T) {
	if sessionID, generated, err := ensureCodexRealtimeExecutionSessionID(nil); err != nil || generated || sessionID != "" {
		t.Fatalf("expected nil request to be a no-op, session=%q generated=%v err=%v", sessionID, generated, err)
	}

	req, err := http.NewRequest(http.MethodGet, "/", nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("x-session-id", "existing-session")
	if sessionID, generated, err := ensureCodexRealtimeExecutionSessionID(req); err != nil || generated || sessionID != "existing-session" {
		t.Fatalf("expected existing x-session-id to be preserved, session=%q generated=%v err=%v", sessionID, generated, err)
	}

	fallbackReq, err := http.NewRequest(http.MethodGet, "/", nil)
	if err != nil {
		t.Fatalf("failed to build fallback request: %v", err)
	}
	fallbackReq.Header.Set("session_id", "legacy-session")
	if sessionID, generated, err := ensureCodexRealtimeExecutionSessionID(fallbackReq); err != nil || generated || sessionID != "legacy-session" || fallbackReq.Header.Get("x-session-id") != "legacy-session" {
		t.Fatalf("expected legacy session_id header to populate x-session-id, session=%q generated=%v header=%q err=%v", sessionID, generated, fallbackReq.Header.Get("x-session-id"), err)
	}

	generatedReq, err := http.NewRequest(http.MethodGet, "/", nil)
	if err != nil {
		t.Fatalf("failed to build generated request: %v", err)
	}
	if sessionID, generated, err := ensureCodexRealtimeExecutionSessionID(generatedReq); err != nil || !generated || sessionID == "" || generatedReq.Header.Get("x-session-id") != sessionID {
		t.Fatalf("expected missing session headers to generate a valid id, session=%q generated=%v header=%q err=%v", sessionID, generated, generatedReq.Header.Get("x-session-id"), err)
	} else if err := validateCodexRealtimeExecutionSessionID(sessionID); err != nil {
		t.Fatalf("expected generated session id to validate, got %v", err)
	}

	invalidReq, err := http.NewRequest(http.MethodGet, "/", nil)
	if err != nil {
		t.Fatalf("failed to build invalid request: %v", err)
	}
	invalidReq.Header.Set("x-session-id", "bad/session")
	if _, _, err := ensureCodexRealtimeExecutionSessionID(invalidReq); err == nil {
		t.Fatal("expected invalid x-session-id to fail validation")
	}

	if wrapped := codexRealtimeInvalidSessionIDError(nil); wrapped != nil {
		t.Fatalf("expected nil invalid-session wrapper input to return nil, got %+v", wrapped)
	}
	if wrapped := codexRealtimeInvalidSessionIDError(stderrors.New("bad session")); wrapped == nil || wrapped.Code != "invalid_session_id" || wrapped.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected invalid-session wrapper to preserve code/status, got %+v", wrapped)
	}
	if wrapped := codexRealtimeManagerError(nil); wrapped != nil {
		t.Fatalf("expected nil manager error to return nil, got %+v", wrapped)
	}
	if wrapped := codexRealtimeManagerError(runtimesession.ErrCallerCapacityExceeded); wrapped == nil || wrapped.Code != "session_caller_capacity_exceeded" || wrapped.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected caller-capacity manager error, got %+v", wrapped)
	}
	if wrapped := codexRealtimeManagerError(runtimesession.ErrCapacityExceeded); wrapped == nil || wrapped.Code != "session_capacity_exceeded" || wrapped.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected capacity manager error, got %+v", wrapped)
	}
	if wrapped := codexRealtimeManagerError(stderrors.New("boom")); wrapped == nil || wrapped.Code != "execution_session_failed" || wrapped.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected generic manager error wrapper, got %+v", wrapped)
	}
}

func TestCodexRealtimeAdminAndIdentityHelpers(t *testing.T) {
	manager, server, _ := newCodexFakeRedisManager(t)
	originalRedis := commonredis.RDB
	commonredis.RDB = server.Client()
	t.Cleanup(func() {
		commonredis.RDB = originalRedis
	})
	manager.ConfigureRedis(server.Client(), "one-hub:execution-session")

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_retry_cooldown_seconds":15,"websocket_mode":"weird"}`, map[string]string{"X-Session-Id": "admin-stats"})
	provider.Context.Set("token_id", 417)
	meta := codexTestMeta(t, provider)
	if exec, _, err := manager.GetOrCreate(meta); err != nil {
		t.Fatalf("expected execution session fixture, got %v", err)
	} else if status := manager.CreateBindingIfAbsent(exec.BuildBinding(), time.Minute); status != runtimesession.BindingWriteApplied {
		t.Fatalf("expected shared binding fixture, got %q", status)
	}
	if stats := GetExecutionSessionStats(); stats.Backend != "redis" || stats.BackendBindings < 1 {
		t.Fatalf("expected admin stats wrapper to reflect redis backend, got %+v", stats)
	}

	if got := provider.getWebsocketRetryCooldown(); got != 15*time.Second {
		t.Fatalf("expected websocket retry cooldown from channel options, got %s", got)
	}
	if got := provider.getWebsocketMode(); got != codexWebsocketModeAuto {
		t.Fatalf("expected invalid websocket mode to normalize to auto, got %q", got)
	}
	if got := (*CodexProvider)(nil).readRealtimeCallerNamespace(); got != "anonymous" {
		t.Fatalf("expected nil provider caller namespace to be anonymous, got %q", got)
	}
	if got := (*CodexProvider)(nil).readRealtimeCapacityNamespace(); got != "anonymous" {
		t.Fatalf("expected nil provider capacity namespace to be anonymous, got %q", got)
	}
	if got := (*CodexProvider)(nil).readRealtimeCredentialIdentity(); got != "credentials:none" {
		t.Fatalf("expected nil provider credential identity fallback, got %q", got)
	}

	accountProvider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"websocket_mode":"off"}`, nil)
	if got := accountProvider.readRealtimeCredentialIdentity(); got != "account:acct-123" {
		t.Fatalf("expected account credential identity, got %q", got)
	}
	refreshProvider := newTestCodexProviderWithContext(t, `{"refresh_token":"refresh-secret"}`, `{"websocket_mode":"off"}`, nil)
	if got := refreshProvider.readRealtimeCredentialIdentity(); !strings.HasPrefix(got, "refresh:") {
		t.Fatalf("expected refresh-token credential identity, got %q", got)
	}
	accessProvider := newTestCodexProviderWithContext(t, `{"access_token":"access-secret"}`, `{"websocket_mode":"off"}`, nil)
	if got := accessProvider.readRealtimeCredentialIdentity(); !strings.HasPrefix(got, "access:") {
		t.Fatalf("expected access-token credential identity, got %q", got)
	}
	channelKeyProvider := newTestCodexProviderWithContext(t, `{"access_token":"access-token"}`, `{"websocket_mode":"off"}`, nil)
	channelKeyProvider.Credentials = nil
	channelKeyProvider.Channel.Key = "channel-secret"
	if got := channelKeyProvider.readRealtimeCredentialIdentity(); !strings.HasPrefix(got, "channel_key:") {
		t.Fatalf("expected channel-key credential identity, got %q", got)
	}

	if _, _, _, ok := parseCodexExecutionSessionKey(""); ok {
		t.Fatal("expected blank execution session key to fail parsing")
	}
	if _, _, _, ok := parseCodexExecutionSessionKey("channel:not-a-number/hash/session"); ok {
		t.Fatal("expected non-numeric channel id to fail parsing")
	}
	if _, _, _, ok := parseCodexExecutionSessionKey("channel:1//session"); ok {
		t.Fatal("expected blank compatibility hash to fail parsing")
	}

	codexMarkExecutionSessionSharedLocked(nil)
	codexMarkExecutionSessionLocalOnlyLocked(nil, runtimesession.PublishIntentNone, "")
	codexStopExecutionSessionRepublishLocked(nil)
	codexMaybePromoteExecutionSession(nil)
}
