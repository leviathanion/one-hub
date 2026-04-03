package session

import (
	stderrors "errors"
	"testing"
	"time"

	"one-api/types"
)

func TestClientPayloadErrorHelpersWithCauseAndNilReceiver(t *testing.T) {
	if err := NewClientPayloadError(nil, nil); err != nil {
		t.Fatalf("expected nil payload+cause to produce nil error, got %v", err)
	}
	payloadOnly := NewClientPayloadError(nil, []byte("payload-only"))
	if payloadOnly == nil || payloadOnly.Error() != "payload-only" {
		t.Fatalf("expected payload-only error string, got %v", payloadOnly)
	}

	baseErr := stderrors.New("base failure")
	originalPayload := []byte(`{"type":"error","message":"payload"}`)
	err := NewClientPayloadError(baseErr, originalPayload)
	if err == nil {
		t.Fatal("expected client payload error")
	}
	originalPayload[0] = '!'
	if got := string(ClientPayloadFromError(err)); got != `{"type":"error","message":"payload"}` {
		t.Fatalf("expected client payload helper to clone payload bytes, got %q", got)
	}
	if err.Error() != baseErr.Error() {
		t.Fatalf("expected client payload error string to follow cause, got %q", err.Error())
	}
	if !stderrors.Is(err, baseErr) {
		t.Fatal("expected client payload error to unwrap to original cause")
	}

	var nilErr *ClientPayloadError
	if nilErr.Error() != "" {
		t.Fatalf("expected nil ClientPayloadError string to be empty, got %q", nilErr.Error())
	}
	if nilErr.Unwrap() != nil {
		t.Fatal("expected nil ClientPayloadError unwrap to return nil")
	}
	if payload := ClientPayloadFromError(baseErr); payload != nil {
		t.Fatalf("expected non-payload errors to return nil payload, got %q", string(payload))
	}
}

func TestTurnObserverFuncAndExecutionSessionLifecycleHelpers(t *testing.T) {
	finalized := false
	observer := TurnObserverFunc(func(payload TurnFinalizePayload) {
		if payload.SessionID == "session-1" {
			finalized = true
		}
	})
	if err := observer.ObserveTurnUsage(&types.UsageEvent{TotalTokens: 1}); err != nil {
		t.Fatalf("expected TurnObserverFunc ObserveTurnUsage to be a no-op, got %v", err)
	}
	observer.FinalizeTurn(TurnFinalizePayload{SessionID: "session-1"})
	if !finalized {
		t.Fatal("expected TurnObserverFunc FinalizeTurn to invoke callback")
	}

	sess := NewExecutionSession(Metadata{
		Key:        "session-1",
		BindingKey: BuildBindingKey("token:1", BindingScopeChatRealtime, "session-1"),
		SessionID:  "session-1",
		CallerNS:   "token:1",
		ChannelID:  2,
		IdleTTL:    time.Second,
	})
	if sess.Visibility != VisibilityShared || sess.PublishIntent != PublishIntentNone {
		t.Fatalf("expected new execution session defaults, got visibility=%q publish_intent=%q", sess.Visibility, sess.PublishIntent)
	}
	if (*ExecutionSession)(nil).IsClosed() != true {
		t.Fatal("expected nil execution session to report closed")
	}
	(*ExecutionSession)(nil).MarkClosed("ignored")
	(*ExecutionSession)(nil).Reopen()
	sess.MarkClosed("")
	if sess.CloseReason != "" || sess.State != SessionStateClosed || !sess.IsClosed() {
		t.Fatalf("expected MarkClosed without reason to close session without mutating reason, got state=%q reason=%q", sess.State, sess.CloseReason)
	}
	sess.Reopen()
	sess.MarkClosed("  trimmed-close-reason  ")
	if sess.CloseReason != "trimmed-close-reason" {
		t.Fatalf("expected MarkClosed to trim close reason, got %q", sess.CloseReason)
	}
	sess.Reopen()
	if sess.IsClosed() || sess.CloseReason != "" || sess.State != SessionStateClosed {
		t.Fatalf("expected Reopen to clear closed flag and reason without mutating state, got closed=%v reason=%q state=%q", sess.IsClosed(), sess.CloseReason, sess.State)
	}

	beforeTouch := sess.LastUsedAt
	time.Sleep(time.Millisecond)
	sess.Touch(time.Time{})
	if !sess.LastUsedAt.After(beforeTouch) {
		t.Fatal("expected zero-value Touch call to refresh LastUsedAt")
	}

	sess.Attached = true
	sess.LastUsedAt = time.Now().Add(-time.Minute)
	if sess.IsExpired(time.Now(), time.Second) {
		t.Fatal("expected attached session not to expire")
	}
	sess.Attached = false
	sess.Inflight = true
	if sess.IsExpired(time.Now(), time.Second) {
		t.Fatal("expected inflight session not to expire")
	}
	sess.Inflight = false
	sess.IdleTTL = 0
	sess.LastUsedAt = time.Now().Add(-2 * time.Second)
	if !sess.IsExpired(time.Now(), time.Second) {
		t.Fatal("expected fallback default TTL to expire idle session")
	}
	if expired := (&ExecutionSession{LastUsedAt: time.Now().Add(-time.Minute)}).IsExpired(time.Time{}, 0); expired {
		t.Fatal("expected zero default TTL to keep idle session alive")
	}
	(*ExecutionSession)(nil).reserveLease()
	(*ExecutionSession)(nil).releaseLease()
	sess.reserveLease()
	if sess.IsExpired(time.Now(), time.Second) {
		t.Fatal("expected leased session to remain alive")
	}
	sess.releaseLease()
	sess.releaseLease()
	if remaining := sess.reservations.Load(); remaining != 0 {
		t.Fatalf("expected releaseLease to clamp negative reservations, got %d", remaining)
	}
}

func TestGuardTurnObserverFinalizesOnlyOnce(t *testing.T) {
	recorder := &recordingGuardTurnObserver{}
	guarded := GuardTurnObserver(recorder)
	if guarded == nil {
		t.Fatal("expected guarded observer")
	}

	if err := guarded.ObserveTurnUsage(&types.UsageEvent{TotalTokens: 1}); err != nil {
		t.Fatalf("expected observe usage to pass through, got %v", err)
	}
	guarded.FinalizeTurn(TurnFinalizePayload{SessionID: "session-1"})
	guarded.FinalizeTurn(TurnFinalizePayload{SessionID: "session-2"})
	if err := guarded.ObserveTurnUsage(&types.UsageEvent{TotalTokens: 2}); err != nil {
		t.Fatalf("expected observe usage after finalize to become a no-op, got %v", err)
	}

	if recorder.observeCount != 1 {
		t.Fatalf("expected guard to forward observe once before finalize, got %d", recorder.observeCount)
	}
	if recorder.finalizeCount != 1 {
		t.Fatalf("expected guard to forward finalize once, got %d", recorder.finalizeCount)
	}

	same := GuardTurnObserver(guarded)
	if same != guarded {
		t.Fatal("expected guarding an already-guarded observer to be idempotent")
	}
}

type recordingGuardTurnObserver struct {
	observeCount  int
	finalizeCount int
}

func (r *recordingGuardTurnObserver) ObserveTurnUsage(usage *types.UsageEvent) error {
	_ = usage
	r.observeCount++
	return nil
}

func (r *recordingGuardTurnObserver) FinalizeTurn(payload TurnFinalizePayload) {
	_ = payload
	r.finalizeCount++
}

func TestBuildBindingAndParseBindingEdgeCases(t *testing.T) {
	if binding := (*ExecutionSession)(nil).BuildBinding(); binding != nil {
		t.Fatalf("expected nil execution session to build no binding, got %+v", binding)
	}
	if binding := (&ExecutionSession{Key: "session-1"}).BuildBinding(); binding != nil {
		t.Fatalf("expected blank binding key to build no binding, got %+v", binding)
	}

	sess := &ExecutionSession{
		Key:               "session-1",
		BindingKey:        "not/a/valid/binding/key",
		CallerNS:          "token:1",
		ChannelID:         3,
		CompatibilityHash: "hash-a",
	}
	binding := sess.BuildBinding()
	if binding == nil || binding.Scope != "a" || binding.SessionID != "valid/binding/key" {
		t.Fatalf("expected parseBindingKey to split on the first two separators, got %+v", binding)
	}

	if _, _, _, ok := parseBindingKey("too/few"); ok {
		t.Fatal("expected parseBindingKey to reject invalid path shape")
	}
	if callerNS, scope, sessionID, ok := parseBindingKey(BuildBindingKey("token/1", "chat/realtime", "session/a")); !ok || callerNS != "token/1" || scope != "chat/realtime" || sessionID != "session/a" {
		t.Fatalf("expected parseBindingKey to unescape binding segments, got caller=%q scope=%q session=%q ok=%v", callerNS, scope, sessionID, ok)
	}
	if _, _, _, ok := parseBindingKey("bad%2/path/shape"); ok {
		t.Fatal("expected parseBindingKey to reject invalid escaping")
	}
}
