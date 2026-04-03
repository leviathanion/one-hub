package openai

import (
	"strings"
	"testing"
	"time"

	runtimesession "one-api/runtime/session"
	"one-api/types"
)

func TestOpenAIRealtimeTurnStateLifecycleAndFinalize(t *testing.T) {
	startedAt := time.Unix(1700000000, 0)
	firstResponseAt := startedAt.Add(250 * time.Millisecond)
	completedAt := startedAt.Add(2 * time.Second)
	observer := runtimesession.TurnObserverFunc(func(payload runtimesession.TurnFinalizePayload) {
		_ = payload
	})

	state := newOpenAIRealtimeTurnState(7, startedAt, observer)
	if state.seq != 7 || state.observer == nil {
		t.Fatalf("expected turn state initialization, got %+v", state)
	}

	state.observeSupplierEvent(types.EventTypeSessionCreated, "resp_bootstrap", firstResponseAt, false)
	if !state.firstResponseAt.IsZero() {
		t.Fatalf("expected session.created not to count as first response, got %v", state.firstResponseAt)
	}
	state.observeSupplierEvent("response.output_text.delta", "resp_1", firstResponseAt, false)
	state.observeSupplierEvent("response.output_text.delta", "resp_1", completedAt, false)
	state.observeSupplierEvent("response.output_text.delta", "resp_error", time.Time{}, true)

	if !state.matchesResponseID("resp_1") || !state.matchesResponseID("resp_error") {
		t.Fatalf("expected response IDs to be tracked, got %#v", state.responseIDs())
	}
	if state.matchesResponseID("missing") {
		t.Fatal("expected unknown response id not to match")
	}
	if got := state.responseIDs(); len(got) != 3 {
		t.Fatalf("expected 3 unique response ids, got %#v", got)
	}

	firstDelta := state.applyUsageSnapshot(&types.UsageEvent{
		InputTokens:  3,
		OutputTokens: 5,
		TotalTokens:  8,
		ExtraTokens: map[string]int{
			"cached": 2,
		},
		ExtraBilling: map[string]types.ExtraBilling{
			types.APIToolTypeWebSearchPreview: {Type: "high", CallCount: 1},
		},
	})
	if firstDelta == nil || firstDelta.TotalTokens != 8 {
		t.Fatalf("expected first usage delta to be reported, got %+v", firstDelta)
	}

	secondDelta := state.applyUsageSnapshot(&types.UsageEvent{
		InputTokens:  4,
		OutputTokens: 8,
		TotalTokens:  12,
		ExtraTokens: map[string]int{
			"cached": 5,
		},
		ExtraBilling: map[string]types.ExtraBilling{
			types.APIToolTypeWebSearchPreview: {Type: "high", CallCount: 3},
		},
	})
	if secondDelta == nil || secondDelta.InputTokens != 1 || secondDelta.OutputTokens != 3 || secondDelta.TotalTokens != 4 {
		t.Fatalf("expected second usage delta to reflect incremental usage, got %+v", secondDelta)
	}
	if got := secondDelta.ExtraTokens["cached"]; got != 3 {
		t.Fatalf("expected extra token delta=3, got %d", got)
	}
	if got := secondDelta.ExtraBilling[types.APIToolTypeWebSearchPreview].CallCount; got != 2 {
		t.Fatalf("expected extra billing delta=2, got %d", got)
	}
	if noDelta := state.applyUsageSnapshot(&types.UsageEvent{
		InputTokens:  4,
		OutputTokens: 8,
		TotalTokens:  12,
	}); noDelta != nil {
		t.Fatalf("expected identical usage snapshot not to emit a delta, got %+v", noDelta)
	}

	finalObserver, payload := state.finalize("session-123", "gpt-4o-realtime-preview", "response.done", completedAt)
	if finalObserver == nil {
		t.Fatal("expected finalize to return observer")
	}
	if payload.SessionID != "session-123" || payload.Model != "gpt-4o-realtime-preview" || payload.TurnSeq != 7 {
		t.Fatalf("unexpected finalize payload identity: %+v", payload)
	}
	if payload.LastResponseID != "resp_error" {
		t.Fatalf("expected last response id to track latest seen id, got %q", payload.LastResponseID)
	}
	if payload.TerminationReason != "response.done" {
		t.Fatalf("expected termination reason to persist, got %q", payload.TerminationReason)
	}
	if payload.FirstResponseAt != firstResponseAt || payload.CompletedAt != completedAt {
		t.Fatalf("unexpected finalize timing payload: %+v", payload)
	}
	if payload.Usage == nil || payload.Usage.TotalTokens != 12 {
		t.Fatalf("expected finalized payload to carry resolved usage, got %+v", payload.Usage)
	}
}

func TestOpenAIRealtimeTurnStateHelpersAndUsageSnapshots(t *testing.T) {
	if got := newOpenAIRealtimeTurnState(1, time.Time{}, nil); got == nil || got.startedAt.IsZero() {
		t.Fatal("expected zero start time to be backfilled")
	}
	var nilState *openAIRealtimeTurnState
	nilState.observeSupplierEvent("response.done", "resp", time.Time{}, false)
	if nilState.matchesResponseID("resp") {
		t.Fatal("expected nil turn state not to match response ids")
	}
	if got := nilState.responseIDs(); got != nil {
		t.Fatalf("expected nil state response IDs to be nil, got %#v", got)
	}
	if delta := nilState.applyUsageSnapshot(&types.UsageEvent{TotalTokens: 1}); delta != nil {
		t.Fatalf("expected nil state to ignore usage snapshots, got %+v", delta)
	}
	if observer, payload := nilState.finalize("session", "model", "reason", time.Time{}); observer != nil || payload != (runtimesession.TurnFinalizePayload{}) {
		t.Fatalf("expected nil state finalize to be empty, got observer=%v payload=%+v", observer, payload)
	}

	base := &types.UsageEvent{
		InputTokens:  3,
		OutputTokens: 1,
		TotalTokens:  4,
		InputTokenDetails: types.PromptTokensDetails{
			TextTokens: 2,
		},
		OutputTokenDetails: types.CompletionTokensDetails{
			TextTokens: 1,
		},
		ExtraTokens: map[string]int{"cached": 1},
		ExtraBilling: map[string]types.ExtraBilling{
			types.APIToolTypeCodeInterpreter: {CallCount: 1},
		},
	}
	update := &types.UsageEvent{
		InputTokens:  5,
		OutputTokens: 6,
		TotalTokens:  11,
		InputTokenDetails: types.PromptTokensDetails{
			TextTokens:        4,
			CachedWriteTokens: 2,
		},
		OutputTokenDetails: types.CompletionTokensDetails{
			TextTokens:      5,
			ReasoningTokens: 2,
		},
		ExtraTokens: map[string]int{"cached": 4, "image": 2},
		ExtraBilling: map[string]types.ExtraBilling{
			types.APIToolTypeCodeInterpreter: {CallCount: 3},
			types.APIToolTypeFileSearch:      {Type: "semantic", CallCount: 1},
		},
	}

	merged := mergeOpenAIRealtimeUsageSnapshot(base, update)
	if merged == nil || merged.TotalTokens != 11 || merged.InputTokenDetails.CachedWriteTokens != 2 || merged.OutputTokenDetails.ReasoningTokens != 2 {
		t.Fatalf("expected merge to keep max usage fields, got %+v", merged)
	}
	if got := merged.ExtraTokens["cached"]; got != 4 {
		t.Fatalf("expected merged cached tokens=4, got %d", got)
	}
	if got := merged.ExtraBilling[types.APIToolTypeCodeInterpreter].CallCount; got != 3 {
		t.Fatalf("expected merged code interpreter count=3, got %d", got)
	}
	if got := merged.ExtraBilling[types.APIToolTypeFileSearch].Type; got != "semantic" {
		t.Fatalf("expected merged extra billing type to persist, got %q", got)
	}

	delta := deltaOpenAIRealtimeUsageSnapshot(merged, base)
	if delta == nil || delta.TotalTokens != 7 || delta.InputTokens != 2 || delta.OutputTokens != 5 {
		t.Fatalf("expected delta snapshot to be incremental, got %+v", delta)
	}
	if got := delta.ExtraTokens["cached"]; got != 3 {
		t.Fatalf("expected extra token delta=3, got %d", got)
	}
	if got := delta.ExtraBilling[types.APIToolTypeCodeInterpreter].CallCount; got != 2 {
		t.Fatalf("expected extra billing delta=2, got %d", got)
	}

	if mergeOpenAIRealtimeUsageSnapshot(nil, update).TotalTokens != 11 {
		t.Fatal("expected nil base merge to clone update")
	}
	if mergeOpenAIRealtimeUsageSnapshot(base, nil).TotalTokens != 4 {
		t.Fatal("expected nil update merge to clone base")
	}
	if deltaOpenAIRealtimeUsageSnapshot(nil, base) != nil {
		t.Fatal("expected nil snapshot delta to be nil")
	}
	if deltaOpenAIRealtimeUsageSnapshot(base, merged) != nil {
		t.Fatal("expected non-increasing snapshot delta to be nil")
	}

	if !openAIRealtimeUsageHasValue(&types.UsageEvent{ExtraTokens: map[string]int{"cached": 1}}) {
		t.Fatal("expected extra tokens to count as usage")
	}
	if !openAIRealtimeUsageHasValue(&types.UsageEvent{ExtraBilling: map[string]types.ExtraBilling{
		types.APIToolTypeFileSearch: {CallCount: 1},
	}}) {
		t.Fatal("expected extra billing to count as usage")
	}
	if openAIRealtimeUsageHasValue(&types.UsageEvent{}) {
		t.Fatal("expected zero usage to be considered empty")
	}

	if usageEventField(nil, func(usage *types.UsageEvent) int { return usage.TotalTokens }) != 0 {
		t.Fatal("expected nil usageEventField to return zero")
	}
	if usageEventField(base, nil) != 0 {
		t.Fatal("expected nil getter to return zero")
	}
	if promptTokenDetailsField(nil) != (types.PromptTokensDetails{}) {
		t.Fatal("expected nil prompt details field to be zero-valued")
	}
	if completionTokenDetailsField(nil) != (types.CompletionTokensDetails{}) {
		t.Fatal("expected nil completion details field to be zero-valued")
	}
	if usageEventExtraTokens(nil) != nil || usageEventExtraBilling(nil) != nil {
		t.Fatal("expected nil usage extra fields to remain nil")
	}

	if maxOpenAIRealtimeUsageInt(4, 2) != 4 || maxOpenAIRealtimeUsageInt(4, 5) != 5 {
		t.Fatal("expected max helper to choose larger token count")
	}
	if deltaOpenAIRealtimeUsageInt(3, 5) != 0 || deltaOpenAIRealtimeUsageInt(8, 5) != 3 {
		t.Fatal("expected delta helper to clamp negative deltas")
	}
	if !openAIRealtimePromptTokenDetailsHasValue(types.PromptTokensDetails{CachedReadTokens: 1}) {
		t.Fatal("expected prompt token details value detection")
	}
	if openAIRealtimePromptTokenDetailsHasValue(types.PromptTokensDetails{}) {
		t.Fatal("expected zero prompt token details to be empty")
	}
	if !openAIRealtimeCompletionTokenDetailsHasValue(types.CompletionTokensDetails{ReasoningTokens: 1}) {
		t.Fatal("expected completion token details value detection")
	}
	if openAIRealtimeCompletionTokenDetailsHasValue(types.CompletionTokensDetails{}) {
		t.Fatal("expected zero completion token details to be empty")
	}

	if cloneOpenAIRealtimeExtraTokens(nil) != nil || cloneOpenAIRealtimeExtraBilling(nil) != nil {
		t.Fatal("expected nil extra field clones to stay nil")
	}
	clonedExtraTokens := cloneOpenAIRealtimeExtraTokens(map[string]int{"cached": 1})
	clonedExtraTokens["cached"] = 9
	clonedExtraBilling := cloneOpenAIRealtimeExtraBilling(map[string]types.ExtraBilling{
		types.APIToolTypeCodeInterpreter: {CallCount: 1},
	})
	clonedExtraBilling[types.APIToolTypeCodeInterpreter] = types.ExtraBilling{CallCount: 9}
	if maxOpenAIRealtimeExtraTokens(nil, nil) != nil || maxOpenAIRealtimeExtraBilling(nil, nil) != nil {
		t.Fatal("expected empty max merges to remain nil")
	}
	if deltaOpenAIRealtimeExtraTokens(nil, nil) != nil || deltaOpenAIRealtimeExtraBilling(nil, nil) != nil {
		t.Fatal("expected empty extra deltas to remain nil")
	}

	err := newOpenAIRealtimeClientError(" session_busy ", "already inflight")
	if err == nil || !strings.Contains(err.Error(), `"code":"session_busy"`) {
		t.Fatalf("expected client error helper to trim code and embed it, got %v", err)
	}
}

func TestOpenAIRealtimeTurnStateAdditionalBranches(t *testing.T) {
	state := newOpenAIRealtimeTurnState(9, time.Time{}, nil)
	if state.matchesResponseID("   ") {
		t.Fatal("expected blank response ids not to match")
	}

	var nilState *openAIRealtimeTurnState
	nilState.rememberResponseID("resp_ignored")

	finalizedState := &openAIRealtimeTurnState{}
	_, payload := finalizedState.finalize("session-9", "gpt-4o-realtime-preview", "", time.Time{})
	if payload.StartedAt.IsZero() || payload.CompletedAt.IsZero() {
		t.Fatalf("expected finalize to backfill missing timing values, got %+v", payload)
	}

	if openAIRealtimeUsageHasValue(nil) {
		t.Fatal("expected nil usage snapshots to be empty")
	}
	if !openAIRealtimeUsageHasValue(&types.UsageEvent{
		InputTokenDetails: types.PromptTokensDetails{
			AudioTokens: 1,
		},
	}) {
		t.Fatal("expected prompt token details to count as usage")
	}

	if merged := maxOpenAIRealtimeExtraTokens(nil, map[string]int{"cached": 1}); merged["cached"] != 1 {
		t.Fatalf("expected maxOpenAIRealtimeExtraTokens to initialize merged state, got %+v", merged)
	}
	if merged := maxOpenAIRealtimeExtraTokens(nil, map[string]int{"cached": 0}); merged != nil {
		t.Fatalf("expected zero-valued extra tokens to collapse back to nil, got %+v", merged)
	}

	mergedBilling := maxOpenAIRealtimeExtraBilling(nil, map[string]types.ExtraBilling{
		types.APIToolTypeCodeInterpreter: {
			CallCount: 1,
		},
	})
	if mergedBilling[types.APIToolTypeCodeInterpreter].CallCount != 1 {
		t.Fatalf("expected maxOpenAIRealtimeExtraBilling to initialize merged state, got %+v", mergedBilling)
	}
}
