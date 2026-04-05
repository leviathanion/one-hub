package session

import (
	"context"
	stderrors "errors"
	"net"
	"testing"
	"time"

	"one-api/internal/testutil/fakeredis"

	"github.com/redis/go-redis/v9"
)

func newSessionFakeRedisBackend(t *testing.T) (*redisBindingBackend, *fakeredis.Server) {
	t.Helper()

	server, err := fakeredis.Start()
	if err != nil {
		t.Fatalf("failed to start fake redis: %v", err)
	}
	t.Cleanup(func() {
		_ = server.Close()
	})

	server.RegisterSessionBindingScripts(fakeredis.SessionScriptHashes{
		CreateBindingIfAbsent:                createBindingIfAbsentScript.Hash(),
		ReplaceBindingIfSessionMatches:       replaceBindingIfSessionMatchesScript.Hash(),
		DeleteBindingIfSessionMatches:        deleteBindingIfSessionMatchesScript.Hash(),
		TouchBindingIfSessionMatches:         touchBindingIfSessionMatchesScript.Hash(),
		DeleteBindingAndRevokeIfSessionMatch: deleteBindingAndRevokeIfSessionMatchesScript.Hash(),
	})

	backend, ok := newRedisBindingBackend(server.Client(), "test:execution-session").(*redisBindingBackend)
	if !ok || backend == nil {
		t.Fatal("expected redis binding backend")
	}
	return backend, server
}

func newFailingRedisClient() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:            "fake-redis.invalid:6379",
		Protocol:        2,
		DisableIdentity: true,
		MaxRetries:      0,
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			return nil, stderrors.New("dial failed")
		},
	})
}

func testBinding(bindingKey, sessionKey string) *Binding {
	return &Binding{
		Key:               bindingKey,
		SessionKey:        sessionKey,
		Scope:             BindingScopeChatRealtime,
		SessionID:         "session-1",
		CallerNS:          "token:1",
		ChannelID:         7,
		CompatibilityHash: "hash-a",
	}
}

func TestRedisBindingBackendResolveAndRevocationPaths(t *testing.T) {
	backend, server := newSessionFakeRedisBackend(t)
	ctx := context.Background()

	binding := testBinding("token:1/chat-realtime/session-1", "channel:7/hash-a/session-1")
	payload, ok := marshalPersistedBinding(binding)
	if !ok {
		t.Fatal("expected persisted binding payload")
	}
	server.SetRaw(backend.redisBindingKey(binding.Key), payload)

	resolved, status := backend.ResolveBinding(ctx, binding.Key)
	if status != ResolveHit || resolved == nil || resolved.SessionKey != binding.SessionKey {
		t.Fatalf("expected resolve hit for stored binding, got status=%q binding=%+v", status, resolved)
	}

	if resolved, status = backend.ResolveBinding(ctx, ""); status != ResolveMiss || resolved != nil {
		t.Fatalf("expected blank binding key to resolve as miss, got status=%q binding=%+v", status, resolved)
	}

	server.SetRaw(backend.redisBindingKey("broken"), "{bad-json")
	if resolved, status = backend.ResolveBinding(ctx, "broken"); status != ResolveMiss || resolved != nil {
		t.Fatalf("expected invalid persisted binding to resolve as miss, got status=%q binding=%+v", status, resolved)
	}
	if _, ok := server.GetRaw(backend.redisBindingKey("broken")); ok {
		t.Fatal("expected invalid persisted binding to be deleted")
	}

	server.FailNext("GET", "ERR forced get failure")
	if resolved, status = backend.ResolveBinding(ctx, "unstable"); status != ResolveBackendError || resolved != nil {
		t.Fatalf("expected forced GET failure to return backend error, got status=%q binding=%+v", status, resolved)
	}

	if status := backend.RevocationStatus(ctx, ""); status != RevocationNotRevoked {
		t.Fatalf("expected blank session key to be not revoked, got %q", status)
	}
	if status := backend.RevocationStatus(ctx, binding.SessionKey); status != RevocationNotRevoked {
		t.Fatalf("expected session key to start as not revoked, got %q", status)
	}
	server.SetRaw(backend.redisRevocationKey(binding.SessionKey), "1")
	if status := backend.RevocationStatus(ctx, binding.SessionKey); status != RevocationRevoked {
		t.Fatalf("expected stored revocation tombstone, got %q", status)
	}
	server.FailNext("EXISTS", "ERR forced exists failure")
	if status := backend.RevocationStatus(ctx, "channel:7/hash-a/unknown"); status != RevocationUnknown {
		t.Fatalf("expected forced EXISTS failure to return unknown, got %q", status)
	}
}

func TestRedisBindingBackendBulkRevocationStatuses(t *testing.T) {
	backend, server := newSessionFakeRedisBackend(t)
	ctx := context.Background()

	revokedA := "channel:7/hash-a/session-revoked-a"
	revokedB := "channel:7/hash-a/session-revoked-b"
	notRevoked := "channel:7/hash-a/session-live"

	server.SetRaw(backend.redisRevocationKey(revokedA), "1")
	server.SetRaw(backend.redisRevocationKey(revokedB), "1")

	statuses, err := backend.RevocationStatuses(ctx, []string{
		revokedA,
		"  ",
		notRevoked,
		revokedB,
		revokedA,
	})
	if err != nil {
		t.Fatalf("expected bulk revocation lookup to succeed, got %v", err)
	}

	expected := []RevocationStatus{
		RevocationRevoked,
		RevocationNotRevoked,
		RevocationNotRevoked,
		RevocationRevoked,
		RevocationRevoked,
	}
	if len(statuses) != len(expected) {
		t.Fatalf("expected %d bulk statuses, got %d", len(expected), len(statuses))
	}
	for i := range expected {
		if statuses[i] != expected[i] {
			t.Fatalf("expected bulk status[%d]=%q, got %q", i, expected[i], statuses[i])
		}
	}

	if statuses, err = backend.RevocationStatuses(ctx, nil); err != nil || len(statuses) != 0 {
		t.Fatalf("expected empty bulk revocation lookup to succeed with empty result, statuses=%v err=%v", statuses, err)
	}
}

func TestRedisBindingBackendWriteHelpersAndCount(t *testing.T) {
	backend, server := newSessionFakeRedisBackend(t)
	ctx := context.Background()

	bindingKey := "token:1/chat-realtime/session-2"
	oldBinding := testBinding(bindingKey, "channel:7/hash-a/session-old")
	newBinding := testBinding(bindingKey, "channel:7/hash-a/session-new")

	if status := backend.CreateBindingIfAbsent(ctx, oldBinding, time.Minute); status != BindingWriteApplied {
		t.Fatalf("expected create-if-absent to apply, got %q", status)
	}
	if status := backend.CreateBindingIfAbsent(ctx, oldBinding, time.Minute); status != BindingWriteApplied {
		t.Fatalf("expected idempotent create-if-absent to apply, got %q", status)
	}
	if status := backend.CreateBindingIfAbsent(ctx, newBinding, time.Minute); status != BindingWriteConditionMismatch {
		t.Fatalf("expected create-if-absent mismatch, got %q", status)
	}

	if status := backend.ReplaceBindingIfSessionMatches(ctx, bindingKey, oldBinding.SessionKey, newBinding, time.Minute); status != BindingWriteApplied {
		t.Fatalf("expected replace-if-matches to apply, got %q", status)
	}
	if status := backend.ReplaceBindingIfSessionMatches(ctx, bindingKey, oldBinding.SessionKey, newBinding, time.Minute); status != BindingWriteApplied {
		t.Fatalf("expected idempotent replace-if-matches to apply, got %q", status)
	}
	if status := backend.ReplaceBindingIfSessionMatches(ctx, bindingKey, oldBinding.SessionKey, testBinding(bindingKey, "channel:7/hash-a/session-other"), time.Minute); status != BindingWriteConditionMismatch {
		t.Fatalf("expected replace-if-matches mismatch, got %q", status)
	}
	if status := backend.ReplaceBindingIfSessionMatches(ctx, "missing", oldBinding.SessionKey, newBinding, time.Minute); status != BindingWriteConditionMismatch {
		t.Fatalf("expected replace-if-matches on missing binding to mismatch, got %q", status)
	}

	if status := backend.TouchBindingIfSessionMatches(ctx, bindingKey, newBinding.SessionKey, time.Minute); status != BindingWriteApplied {
		t.Fatalf("expected touch-if-matches to apply, got %q", status)
	}
	if status := backend.TouchBindingIfSessionMatches(ctx, bindingKey, oldBinding.SessionKey, time.Minute); status != BindingWriteConditionMismatch {
		t.Fatalf("expected touch-if-matches mismatch, got %q", status)
	}

	if status := backend.DeleteBindingIfSessionMatches(ctx, bindingKey, oldBinding.SessionKey); status != BindingWriteConditionMismatch {
		t.Fatalf("expected delete-if-matches mismatch for stale session, got %q", status)
	}
	if status := backend.DeleteBindingIfSessionMatches(ctx, bindingKey, newBinding.SessionKey); status != BindingWriteApplied {
		t.Fatalf("expected delete-if-matches to apply, got %q", status)
	}
	if status := backend.DeleteBindingIfSessionMatches(ctx, bindingKey, newBinding.SessionKey); status != BindingWriteApplied {
		t.Fatalf("expected delete-if-matches to be idempotent on miss, got %q", status)
	}

	if status := backend.CreateBindingIfAbsent(ctx, oldBinding, time.Minute); status != BindingWriteApplied {
		t.Fatalf("expected recreate binding to apply, got %q", status)
	}
	if status := backend.DeleteBindingAndRevokeIfSessionMatches(ctx, bindingKey, oldBinding.SessionKey, time.Minute); status != BindingWriteApplied {
		t.Fatalf("expected delete-and-revoke to apply, got %q", status)
	}
	if _, ok := server.GetRaw(backend.redisBindingKey(bindingKey)); ok {
		t.Fatal("expected delete-and-revoke to remove binding key")
	}
	if value, ok := server.GetRaw(backend.redisRevocationKey(oldBinding.SessionKey)); !ok || value != "1" {
		t.Fatalf("expected delete-and-revoke to persist tombstone, got %q ok=%v", value, ok)
	}
	if status := backend.DeleteBindingAndRevokeIfSessionMatches(ctx, bindingKey, oldBinding.SessionKey, time.Minute); status != BindingWriteConditionMismatch {
		t.Fatalf("expected delete-and-revoke on missing binding to mismatch, got %q", status)
	}

	if status := backend.CreateBindingIfAbsent(ctx, testBinding("token:1/chat-realtime/session-3", "channel:7/hash-a/session-3"), time.Minute); status != BindingWriteApplied {
		t.Fatalf("expected second binding creation, got %q", status)
	}
	if count := backend.CountBindings(ctx); count != 1 {
		t.Fatalf("expected one live binding after delete-and-revoke cleanup, got %d", count)
	}
}

func TestRedisBindingBackendHelperAndFailureBranches(t *testing.T) {
	ctx := context.Background()
	var nilBackend *redisBindingBackend
	if binding, status := nilBackend.ResolveBinding(ctx, "binding"); status != ResolveBackendError || binding != nil {
		t.Fatalf("expected nil backend resolve to report backend error, got status=%q binding=%+v", status, binding)
	}
	if status := nilBackend.RevocationStatus(ctx, "session"); status != RevocationUnknown {
		t.Fatalf("expected nil backend revocation lookup to be unknown, got %q", status)
	}
	if statuses, err := nilBackend.RevocationStatuses(ctx, []string{"session"}); err == nil || len(statuses) != 0 {
		t.Fatalf("expected nil backend bulk revocation lookup to error, statuses=%v err=%v", statuses, err)
	}
	if status := nilBackend.CreateBindingIfAbsent(ctx, testBinding("binding", "session"), time.Minute); status != BindingWriteBackendError {
		t.Fatalf("expected nil backend create to fail, got %q", status)
	}
	if count := nilBackend.CountBindings(ctx); count != 0 {
		t.Fatalf("expected nil backend count to be zero, got %d", count)
	}

	failing := &redisBindingBackend{client: newFailingRedisClient(), prefix: normalizeSessionRedisPrefix(" custom-prefix ")}
	if binding, status := failing.ResolveBinding(ctx, "binding"); status != ResolveBackendError || binding != nil {
		t.Fatalf("expected failing backend resolve to report backend error, got status=%q binding=%+v", status, binding)
	}
	if binding, status := failing.ResolveBinding(ctx, ""); status != ResolveMiss || binding != nil {
		t.Fatalf("expected empty binding key to short-circuit before dialing")
	}
	if status := failing.RevocationStatus(ctx, "session"); status != RevocationUnknown {
		t.Fatalf("expected failing backend revocation lookup to be unknown, got %q", status)
	}
	if status := failing.RevocationStatus(ctx, ""); status != RevocationNotRevoked {
		t.Fatalf("expected empty revocation lookup to be not revoked, got %q", status)
	}
	if statuses, err := failing.RevocationStatuses(ctx, []string{"session"}); err == nil || len(statuses) != 0 {
		t.Fatalf("expected failing backend bulk revocation lookup to error, statuses=%v err=%v", statuses, err)
	}
	if status := failing.CreateBindingIfAbsent(ctx, testBinding("binding", "session"), time.Minute); status != BindingWriteBackendError {
		t.Fatalf("expected failing backend create to error, got %q", status)
	}
	if status := failing.ReplaceBindingIfSessionMatches(ctx, "binding", "session", testBinding("binding", "session-2"), time.Minute); status != BindingWriteBackendError {
		t.Fatalf("expected failing backend replace to error, got %q", status)
	}
	if status := failing.DeleteBindingIfSessionMatches(ctx, "binding", "session"); status != BindingWriteBackendError {
		t.Fatalf("expected failing backend delete to error, got %q", status)
	}
	if status := failing.TouchBindingIfSessionMatches(ctx, "binding", "session", time.Minute); status != BindingWriteBackendError {
		t.Fatalf("expected failing backend touch to error, got %q", status)
	}
	if status := failing.DeleteBindingAndRevokeIfSessionMatches(ctx, "binding", "session", time.Minute); status != BindingWriteBackendError {
		t.Fatalf("expected failing backend delete-and-revoke to error, got %q", status)
	}
	if status := failing.runBindingWriteScript(ctx, createBindingIfAbsentScript, []string{"binding"}, "", time.Minute); status != BindingWriteBackendError {
		t.Fatalf("expected empty payload to fail runBindingWriteScript, got %q", status)
	}
	if status := failing.runBindingWriteScript(ctx, createBindingIfAbsentScript, []string{"binding"}, "payload", time.Minute); status != BindingWriteBackendError {
		t.Fatalf("expected failing client runBindingWriteScript to error, got %q", status)
	}
	if count := failing.CountBindings(ctx); count != 0 {
		t.Fatalf("expected failing backend scan to return zero, got %d", count)
	}

	if newRedisBindingBackend(nil, "ignored") != nil {
		t.Fatal("expected nil redis client to produce nil backend")
	}
	if backend, ok := newRedisBindingBackend(newFailingRedisClient(), "  custom-prefix  ").(*redisBindingBackend); !ok || backend == nil || backend.prefix != "custom-prefix" {
		t.Fatalf("expected normalized redis prefix on concrete backend, got %+v", backend)
	}
	if got := bindingKeyOf(nil); got != "" {
		t.Fatalf("expected nil binding key lookup to be empty, got %q", got)
	}
	if got := bindingKeyOf(&Binding{Key: "  binding  "}); got != "binding" {
		t.Fatalf("expected binding key helper to trim spaces, got %q", got)
	}
	if binding := decodePersistedBinding(""); binding != nil {
		t.Fatalf("expected empty persisted binding payload to decode as nil, got %+v", binding)
	}
	if binding := decodePersistedBinding("{bad-json"); binding != nil {
		t.Fatalf("expected invalid persisted binding payload to decode as nil, got %+v", binding)
	}
	if payload, ok := marshalPersistedBinding(nil); ok || payload != "" {
		t.Fatalf("expected nil binding marshal to fail, got payload=%q ok=%v", payload, ok)
	}
	validBinding := testBinding("binding", "session")
	payload, ok := marshalPersistedBinding(validBinding)
	if !ok || payload == "" {
		t.Fatal("expected valid binding payload to marshal")
	}
	if decoded := decodePersistedBinding(payload); decoded == nil || decoded.SessionKey != validBinding.SessionKey {
		t.Fatalf("expected valid binding payload to decode, got %+v", decoded)
	}
	if ttl := normalizeRedisTTL(0); ttl != 1000 {
		t.Fatalf("expected zero ttl to normalize to one second, got %d", ttl)
	}
	if ttl := normalizeRedisTTL(time.Nanosecond); ttl != 1 {
		t.Fatalf("expected sub-millisecond ttl to normalize to one millisecond, got %d", ttl)
	}
	if status := translateBindingWriteResult(1, nil); status != BindingWriteApplied {
		t.Fatalf("expected result 1 to translate to applied, got %q", status)
	}
	if status := translateBindingWriteResult(2, nil); status != BindingWriteConditionMismatch {
		t.Fatalf("expected non-1 result to translate to mismatch, got %q", status)
	}
	if status := translateBindingWriteResult(0, stderrors.New("boom")); status != BindingWriteBackendError {
		t.Fatalf("expected error result to translate to backend error, got %q", status)
	}
	if prefix := normalizeSessionRedisPrefix("   "); prefix != defaultExecutionSessionRedisPrefix {
		t.Fatalf("expected blank prefix to normalize to default, got %q", prefix)
	}
}

func TestRedisBindingBackendAdditionalGuardBranches(t *testing.T) {
	ctx := context.Background()
	var nilManager *Manager
	if ttl := nilManager.bindingTTLForSessionLocked("session"); ttl != 0 {
		t.Fatalf("expected nil manager binding ttl lookup to return zero, got %s", ttl)
	}

	manager := NewManager(time.Minute, 0, nil)
	if ttl := manager.bindingTTLForSessionLocked("missing"); ttl != time.Minute {
		t.Fatalf("expected missing session binding ttl to fall back to default ttl, got %s", ttl)
	}
	manager.sessions["session"] = &ExecutionSession{IdleTTL: 2 * time.Minute}
	if ttl := manager.bindingTTLForSessionLocked("session"); ttl != 2*time.Minute {
		t.Fatalf("expected live session binding ttl to prefer per-session ttl, got %s", ttl)
	}

	backend, _ := newSessionFakeRedisBackend(t)
	if resolved, status := backend.ResolveBinding(ctx, "missing-binding"); status != ResolveMiss || resolved != nil {
		t.Fatalf("expected missing binding lookup to resolve as miss, got status=%q binding=%+v", status, resolved)
	}
	if status := backend.runBindingWriteScript(ctx, createBindingIfAbsentScript, nil, "payload", time.Minute); status != BindingWriteBackendError {
		t.Fatalf("expected missing key list to fail write script, got %q", status)
	}
	if status := backend.runBindingWriteScript(ctx, createBindingIfAbsentScript, []string{"  "}, "payload", time.Minute); status != BindingWriteBackendError {
		t.Fatalf("expected blank redis key to fail write script, got %q", status)
	}

	if payload, ok := marshalPersistedBinding(&Binding{Key: "", SessionKey: "session"}); ok || payload != "" {
		t.Fatalf("expected blank binding key marshal to fail, got payload=%q ok=%v", payload, ok)
	}
	if payload, ok := marshalPersistedBinding(&Binding{Key: "binding", SessionKey: ""}); ok || payload != "" {
		t.Fatalf("expected blank session key marshal to fail, got payload=%q ok=%v", payload, ok)
	}
}
