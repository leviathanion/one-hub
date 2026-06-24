package relay

import (
	"reflect"
	"testing"

	"one-api/common/responsesws"
	"one-api/types"
)

func responsesWSSettlementProviderProjection(observations ...responsesws.ProviderObservation) responsesws.ProviderSettlementLogProjection {
	var projection responsesws.ProviderSettlementLogProjection
	for _, observation := range observations {
		projection.Observe(observation)
	}
	return projection
}

func TestResponsesWSSettlementDecisionMatrix(t *testing.T) {
	tests := []struct {
		name      string
		input     ResponsesWSSettlementInput
		action    ResponsesWSSettlementAction
		basis     ResponsesWSSettlementBasis
		final     int64
		wantFlags []ResponsesWSSettlementFlag
	}{
		{
			name: "terminal exact below floor",
			input: ResponsesWSSettlementInput{FloorQuota: 100, Evidence: ResponsesWSSettlementEvidence{Terminal: &ResponsesWSTerminalEvidence{
				Kind: responsesws.ResponsesSuccessTerminal, HasTerminalUsage: true, BillableQuota: 10,
			}}},
			action: ResponsesWSSettlementFinalizeExactUsage,
			basis:  ResponsesWSSettlementBasisTerminalUsage,
			final:  10,
		},
		{
			name: "terminal exact explicit zero below floor",
			input: ResponsesWSSettlementInput{FloorQuota: 100, Evidence: ResponsesWSSettlementEvidence{Terminal: &ResponsesWSTerminalEvidence{
				Kind: responsesws.ResponsesSuccessTerminal, HasTerminalUsage: true, BillableQuota: 0,
			}}},
			action: ResponsesWSSettlementFinalizeExactUsage,
			basis:  ResponsesWSSettlementBasisTerminalUsage,
			final:  0,
		},
		{
			name: "terminal exact equal floor",
			input: ResponsesWSSettlementInput{FloorQuota: 100, Evidence: ResponsesWSSettlementEvidence{Terminal: &ResponsesWSTerminalEvidence{
				Kind: responsesws.ResponsesSuccessTerminal, HasTerminalUsage: true, BillableQuota: 100,
			}}},
			action: ResponsesWSSettlementFinalizeExactUsage,
			basis:  ResponsesWSSettlementBasisTerminalUsage,
			final:  100,
		},
		{
			name: "terminal exact above floor",
			input: ResponsesWSSettlementInput{FloorQuota: 100, Evidence: ResponsesWSSettlementEvidence{Terminal: &ResponsesWSTerminalEvidence{
				Kind: responsesws.ResponsesSuccessTerminal, HasTerminalUsage: true, BillableQuota: 150,
			}}},
			action: ResponsesWSSettlementFinalizeExactUsage,
			basis:  ResponsesWSSettlementBasisTerminalUsage,
			final:  150,
		},
		{
			name: "terminal without usage",
			input: ResponsesWSSettlementInput{FloorQuota: 100, Evidence: ResponsesWSSettlementEvidence{Terminal: &ResponsesWSTerminalEvidence{
				Kind: responsesws.ResponsesFailedTerminal,
			}}},
			action: ResponsesWSSettlementFinalizeFloor,
			basis:  ResponsesWSSettlementBasisFloor,
			final:  100,
		},
		{
			name:   "observed below floor",
			input:  ResponsesWSSettlementInput{FloorQuota: 100, Evidence: ResponsesWSSettlementEvidence{ObservedBillableQuota: 10}},
			action: ResponsesWSSettlementFinalizeObservedOrFloor,
			basis:  ResponsesWSSettlementBasisObservedOrFloor,
			final:  100,
		},
		{
			name:   "observed equal floor",
			input:  ResponsesWSSettlementInput{FloorQuota: 100, Evidence: ResponsesWSSettlementEvidence{ObservedBillableQuota: 100}},
			action: ResponsesWSSettlementFinalizeObservedOrFloor,
			basis:  ResponsesWSSettlementBasisObservedOrFloor,
			final:  100,
		},
		{
			name:   "observed above floor",
			input:  ResponsesWSSettlementInput{FloorQuota: 100, Evidence: ResponsesWSSettlementEvidence{ObservedBillableQuota: 150}},
			action: ResponsesWSSettlementFinalizeObservedOrFloor,
			basis:  ResponsesWSSettlementBasisObservedOrFloor,
			final:  150,
		},
		{
			name:   "zero charge proof alone",
			input:  ResponsesWSSettlementInput{FloorQuota: 100, ZeroChargeProof: ResponsesWSZeroChargeProof{Kind: ResponsesWSZeroChargeProofTransportNotAttempted}},
			action: ResponsesWSSettlementRollbackReserve,
			basis:  ResponsesWSSettlementBasisZeroChargeProof,
			final:  0,
		},
		{
			name:      "no proof no evidence",
			input:     ResponsesWSSettlementInput{FloorQuota: 100},
			action:    ResponsesWSSettlementFinalizeFloor,
			basis:     ResponsesWSSettlementBasisFloor,
			final:     100,
			wantFlags: []ResponsesWSSettlementFlag{ResponsesWSSettlementFlagNoProviderEvidence},
		},
		{
			name:   "negative floor and observed clamp",
			input:  ResponsesWSSettlementInput{FloorQuota: -100, Evidence: ResponsesWSSettlementEvidence{ObservedBillableQuota: -10}},
			action: ResponsesWSSettlementFinalizeFloor,
			basis:  ResponsesWSSettlementBasisFloor,
			final:  0,
			wantFlags: []ResponsesWSSettlementFlag{
				ResponsesWSSettlementFlagMissingSettlementFloor,
				ResponsesWSSettlementFlagNoProviderEvidence,
			},
		},
		{
			name:      "missing settlement floor emits flag",
			input:     ResponsesWSSettlementInput{},
			action:    ResponsesWSSettlementFinalizeFloor,
			basis:     ResponsesWSSettlementBasisFloor,
			final:     0,
			wantFlags: []ResponsesWSSettlementFlag{ResponsesWSSettlementFlagMissingSettlementFloor},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decideResponsesWSSettlement(tc.input)
			if got.Action != tc.action || got.Basis != tc.basis || got.ExpectedFinalQuota != tc.final {
				t.Fatalf("decision mismatch: got action=%v basis=%v final=%d flags=%v", got.Action, got.Basis, got.ExpectedFinalQuota, got.Flags)
			}
			for _, flag := range tc.wantFlags {
				if !responsesWSSettlementHasFlag(got, flag) {
					t.Fatalf("expected flag %q in %+v", flag, got.Flags)
				}
			}
			replayed := decideResponsesWSSettlement(tc.input)
			if replayed.DecisionKey != got.DecisionKey {
				t.Fatalf("expected stable decision key, got %q then %q", got.DecisionKey, replayed.DecisionKey)
			}
		})
	}
}

func TestResponsesWSSettlementContradictoryInputSuppressesRollback(t *testing.T) {
	tests := []struct {
		name   string
		input  ResponsesWSSettlementInput
		action ResponsesWSSettlementAction
		basis  ResponsesWSSettlementBasis
		final  int64
	}{
		{
			name: "zero charge proof plus provider activity",
			input: ResponsesWSSettlementInput{
				FloorQuota:      100,
				ZeroChargeProof: ResponsesWSZeroChargeProof{Kind: ResponsesWSZeroChargeProofTransportNotAttempted},
				Evidence:        ResponsesWSSettlementEvidence{AnyProviderActivityEvidence: true},
			},
			action: ResponsesWSSettlementFinalizeFloor,
			basis:  ResponsesWSSettlementBasisFloor,
			final:  100,
		},
		{
			name: "zero charge proof plus observed below floor",
			input: ResponsesWSSettlementInput{
				FloorQuota:      100,
				ZeroChargeProof: ResponsesWSZeroChargeProof{Kind: ResponsesWSZeroChargeProofTransportNotAttempted},
				Evidence:        ResponsesWSSettlementEvidence{ObservedBillableQuota: 10},
			},
			action: ResponsesWSSettlementFinalizeObservedOrFloor,
			basis:  ResponsesWSSettlementBasisObservedOrFloor,
			final:  100,
		},
		{
			name: "zero charge proof plus observed above floor",
			input: ResponsesWSSettlementInput{
				FloorQuota:      100,
				ZeroChargeProof: ResponsesWSZeroChargeProof{Kind: ResponsesWSZeroChargeProofTransportNotAttempted},
				Evidence:        ResponsesWSSettlementEvidence{ObservedBillableQuota: 150},
			},
			action: ResponsesWSSettlementFinalizeObservedOrFloor,
			basis:  ResponsesWSSettlementBasisObservedOrFloor,
			final:  150,
		},
		{
			name: "zero charge proof plus terminal usage",
			input: ResponsesWSSettlementInput{
				FloorQuota:      100,
				ZeroChargeProof: ResponsesWSZeroChargeProof{Kind: ResponsesWSZeroChargeProofTransportNotAttempted},
				Evidence: ResponsesWSSettlementEvidence{Terminal: &ResponsesWSTerminalEvidence{
					Kind: responsesws.ResponsesSuccessTerminal, HasTerminalUsage: true, BillableQuota: 10,
				}},
			},
			action: ResponsesWSSettlementFinalizeExactUsage,
			basis:  ResponsesWSSettlementBasisTerminalUsage,
			final:  10,
		},
		{
			name: "zero charge proof plus terminal without usage",
			input: ResponsesWSSettlementInput{
				FloorQuota:      100,
				ZeroChargeProof: ResponsesWSZeroChargeProof{Kind: ResponsesWSZeroChargeProofTransportNotAttempted},
				Evidence:        ResponsesWSSettlementEvidence{Terminal: &ResponsesWSTerminalEvidence{Kind: responsesws.ResponsesFailedTerminal}},
			},
			action: ResponsesWSSettlementFinalizeFloor,
			basis:  ResponsesWSSettlementBasisFloor,
			final:  100,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decideResponsesWSSettlement(tc.input)
			if got.Action == ResponsesWSSettlementRollbackReserve {
				t.Fatalf("contradictory input must not rollback: %+v", got)
			}
			if got.Action != tc.action || got.Basis != tc.basis || got.ExpectedFinalQuota != tc.final {
				t.Fatalf("decision mismatch: got action=%v basis=%v final=%d flags=%v", got.Action, got.Basis, got.ExpectedFinalQuota, got.Flags)
			}
			if !responsesWSSettlementHasFlag(got, ResponsesWSSettlementFlagContradictoryInput) {
				t.Fatalf("expected contradictory flag in %+v", got.Flags)
			}
			if !responsesWSSettlementHasFlag(got, ResponsesWSSettlementFlagZeroChargeProofSuppressed) {
				t.Fatalf("expected zero proof suppressed flag in %+v", got.Flags)
			}
		})
	}
}

func TestResponsesWSSettlementMissingSettlementFloorOnlyOnFloorPath(t *testing.T) {
	tests := []struct {
		name        string
		input       ResponsesWSSettlementInput
		wantMissing bool
	}{
		{
			name: "terminal exact with zero floor does not depend on settlement floor",
			input: ResponsesWSSettlementInput{Evidence: ResponsesWSSettlementEvidence{Terminal: &ResponsesWSTerminalEvidence{
				Kind: responsesws.ResponsesSuccessTerminal, HasTerminalUsage: true, BillableQuota: 0,
			}}},
		},
		{
			name: "zero proof rollback with zero floor does not depend on settlement floor",
			input: ResponsesWSSettlementInput{
				ZeroChargeProof: ResponsesWSZeroChargeProof{Kind: ResponsesWSZeroChargeProofTransportNotAttempted},
			},
		},
		{
			name:        "floor path with zero floor is missing settlement floor",
			input:       ResponsesWSSettlementInput{Evidence: ResponsesWSSettlementEvidence{AnyProviderActivityEvidence: true}},
			wantMissing: true,
		},
		{
			name: "observed-or-floor path with zero floor is missing settlement floor",
			input: ResponsesWSSettlementInput{
				Evidence: ResponsesWSSettlementEvidence{ObservedBillableQuota: 10},
			},
			wantMissing: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decision := decideResponsesWSSettlement(tc.input)
			if got := responsesWSSettlementHasFlag(decision, ResponsesWSSettlementFlagMissingSettlementFloor); got != tc.wantMissing {
				t.Fatalf("missing settlement floor flag mismatch: got %v want %v decision=%+v", got, tc.wantMissing, decision)
			}
		})
	}
}

func TestResponsesWSSettlementProjection(t *testing.T) {
	observed := ProjectResponsesWSProviderEvidence(ResponsesWSProviderEvidenceProjectionInput{ObservedBillableQuota: 1})
	if !observed.Evidence.AnyProviderActivityEvidence {
		t.Fatal("expected observed billable quota to become settlement activity")
	}
	terminal := ProjectResponsesWSProviderEvidence(ResponsesWSProviderEvidenceProjectionInput{
		Terminal: &ResponsesWSTerminalEvidence{Kind: responsesws.ResponsesFailedTerminal},
	})
	if !terminal.Evidence.AnyProviderActivityEvidence {
		t.Fatal("expected terminal evidence to become settlement activity")
	}
	providerFrame := ProjectResponsesWSProviderEvidence(ResponsesWSProviderEvidenceProjectionInput{
		Provider: responsesWSSettlementProviderProjection(responsesws.ProviderObservation{DetailOrigin: responsesws.RecvDetailOriginProviderFrame}),
	})
	if !providerFrame.Evidence.AnyProviderActivityEvidence || !providerFrame.Diagnostics.ProviderFrameSeen {
		t.Fatalf("expected relay adapter to expose provider-frame evidence, got %+v", providerFrame)
	}

	baseEvidence := ProjectResponsesWSProviderEvidence(ResponsesWSProviderEvidenceProjectionInput{ObservedBillableQuota: 10}).Evidence
	diagResult := ProjectResponsesWSProviderEvidence(ResponsesWSProviderEvidenceProjectionInput{
		ObservedBillableQuota: 10,
		Provider:              responsesWSSettlementProviderProjection(responsesws.ProviderObservation{DetailOrigin: responsesws.RecvDetailOriginBridgeStreamError}),
		TransportStatus:       "ambiguous",
		CloseReason:           "close",
		EventKind:             "test",
	})
	if !reflect.DeepEqual(baseEvidence, diagResult.Evidence) {
		t.Fatalf("diagnostics must not alter evidence: base=%+v diag=%+v", baseEvidence, diagResult.Evidence)
	}
	diagnostics := diagResult.Diagnostics
	diagnostics.DetailOrigins[0] = "mutated"
	copied := ProjectResponsesWSProviderEvidence(ResponsesWSProviderEvidenceProjectionInput{
		Provider: responsesWSSettlementProviderProjection(responsesws.ProviderObservation{DetailOrigin: responsesws.RecvDetailOriginProxyLocal}),
	}).Diagnostics
	if copied.DetailOrigins[0] != string(responsesws.RecvDetailOriginProxyLocal) {
		t.Fatalf("expected diagnostics detail origins to be copied")
	}

	rejected := ProjectResponsesWSProviderEvidence(ResponsesWSProviderEvidenceProjectionInput{
		Provider:  responsesWSSettlementProviderProjection(responsesws.ProviderObservation{DetailOrigin: responsesws.RecvDetailOriginBridgeOpenProviderError}),
		EventKind: "provider_rejected_before_stream",
	})
	if rejected.Evidence.AnyProviderActivityEvidence {
		t.Fatal("provider rejected before stream must not be projected as activity evidence")
	}
	if rejected.ZeroChargeProofCandidate != responsesws.ZeroChargeProofCandidateProviderRejectedBeforeStream {
		t.Fatalf("expected provider rejection candidate, got %v", rejected.ZeroChargeProofCandidate)
	}
}

func TestResponsesWSSettlementInputDoesNotPromoteProjectionCandidateToZeroProof(t *testing.T) {
	attempt := &ResponsesWSTurnAttempt{
		AttemptID: "attempt-projection-candidate",
		Usage:     &types.Usage{},
	}
	actor := &ResponsesWSSessionActor{}
	input := actor.buildSettlementInputFromAttempt(
		attempt,
		responsesWSSettlementProviderProjection(responsesws.ProviderObservation{DetailOrigin: responsesws.RecvDetailOriginBridgeOpenProviderError}),
		"bridge_open_provider_error",
		ResponsesWSZeroChargeProof{},
	)

	if input.ZeroChargeProof.Present() {
		t.Fatalf("projection candidate must not auto-populate zero proof, input=%+v", input)
	}
	if decideResponsesWSSettlement(input).Action == ResponsesWSSettlementRollbackReserve {
		t.Fatalf("settlement without explicit zero proof must not rollback, input=%+v", input)
	}
}

func TestBuildResponsesWSSettlementInputProjectsExplicitBeforeStreamProof(t *testing.T) {
	base := ResponsesWSSettlementProjectionInput{
		AttemptID:  "attempt-builder",
		FloorQuota: 100,
		Provider: responsesWSSettlementProviderProjection(
			responsesws.ProviderObservation{DetailOrigin: responsesws.RecvDetailOriginBridgeOpenProviderError},
		),
		TransportResult: responsesws.ResponsesWSTransportSendResult{
			Status: responsesws.ResponsesWSTransportSendRejectedBeforeStream,
		},
		ZeroChargeProofRequest: ResponsesWSZeroChargeProof{
			Kind:   ResponsesWSZeroChargeProofProviderRejectedBeforeStream,
			Reason: "bridge_open_provider_error",
		},
	}

	rollbackInput := BuildResponsesWSSettlementInput(base)
	if !rollbackInput.ZeroChargeProof.Present() {
		t.Fatalf("expected explicit before-stream proof, input=%+v", rollbackInput)
	}
	if decision := decideResponsesWSSettlement(rollbackInput); decision.Action != ResponsesWSSettlementRollbackReserve {
		t.Fatalf("expected before-stream proof to rollback, decision=%+v input=%+v", decision, rollbackInput)
	}

	withActivity := base
	withActivity.Provider = responsesWSSettlementProviderProjection(
		responsesws.ProviderObservation{DetailOrigin: responsesws.RecvDetailOriginBridgeOpenProviderError},
		responsesws.ProviderObservation{DetailOrigin: responsesws.RecvDetailOriginProviderFrame, HasFrame: true},
	)
	activityInput := BuildResponsesWSSettlementInput(withActivity)
	decision := decideResponsesWSSettlement(activityInput)
	if !activityInput.ZeroChargeProof.Present() ||
		decision.Action == ResponsesWSSettlementRollbackReserve ||
		!responsesWSSettlementHasFlag(decision, ResponsesWSSettlementFlagZeroChargeProofSuppressed) {
		t.Fatalf("expected provider activity to suppress rollback in settlement core, decision=%+v input=%+v", decision, activityInput)
	}
}

func TestBuildResponsesWSSettlementInputRequiresTypedBeforeAcceptProof(t *testing.T) {
	base := ResponsesWSSettlementProjectionInput{
		AttemptID:  "attempt-before-accept",
		FloorQuota: 100,
		ZeroChargeProofRequest: ResponsesWSZeroChargeProof{
			Kind:   ResponsesWSZeroChargeProofProviderRejectedBeforeAccept,
			Reason: "misused_before_accept",
		},
	}

	misusedInput := BuildResponsesWSSettlementInput(base)
	if misusedInput.ZeroChargeProof.Present() {
		t.Fatalf("expected untyped before-accept proof to be rejected, input=%+v", misusedInput)
	}
	if decision := decideResponsesWSSettlement(misusedInput); decision.Action == ResponsesWSSettlementRollbackReserve {
		t.Fatalf("expected untyped before-accept proof not to rollback, decision=%+v", decision)
	}

	typed := base
	typed.ZeroChargeProofRequest = responsesWSProviderRejectedBeforeAcceptProof("typed_before_accept")
	typedInput := BuildResponsesWSSettlementInput(typed)
	if !typedInput.ZeroChargeProof.Present() {
		t.Fatalf("expected typed before-accept proof to be accepted, input=%+v", typedInput)
	}
	if decision := decideResponsesWSSettlement(typedInput); decision.Action != ResponsesWSSettlementRollbackReserve {
		t.Fatalf("expected typed before-accept proof to rollback, decision=%+v", decision)
	}
}

func TestResponsesWSPendingProviderReplayBuffersAppendEvidence(t *testing.T) {
	actor := &ResponsesWSSessionActor{
		turns: responsesWSTurnSlots{
			pending: responsesWSPendingTurn{attempt: &ResponsesWSTurnAttempt{
				AttemptID: "attempt-buffer-evidence",
				Usage:     &types.Usage{},
			}},
		},
	}
	frame := responsesws.NewTextFrame([]byte(`{"type":"response.created","response":{"id":"resp_1"}}`))
	downstream := ResponsesWSEventProviderDownstream{
		AttemptID:    actor.turns.pending.attempt.AttemptID,
		Kind:         ProviderDownstreamFrame,
		Frame:        &frame,
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
	}
	if !actor.observeAndBufferPendingProviderEvent(downstream, upstreamEventFromProviderDownstream(downstream)) {
		t.Fatal("expected pending downstream event to be buffered")
	}
	if len(actor.turns.pending.provider.journal.DownstreamEvents()) != 1 || !actor.turns.pending.provider.journal.Project().HasActivity() {
		t.Fatalf("expected buffered downstream to update evidence, events=%d evidence=%+v", len(actor.turns.pending.provider.journal.DownstreamEvents()), actor.turns.pending.provider.journal.Project())
	}

	failure := ResponsesWSEventProviderRecvFailed{
		AttemptID:    actor.turns.pending.attempt.AttemptID,
		DetailOrigin: responsesws.RecvDetailOriginProviderMalformed,
	}
	actor.observeAndBufferPendingProviderFailure(failure, upstreamEventFromProviderRecvFailed(failure))
	if len(actor.turns.pending.provider.journal.Failures()) != 1 ||
		actor.turns.pending.provider.journal.Project().LastActivityOrigin() != responsesws.RecvDetailOriginProviderMalformed {
		t.Fatalf("expected buffered failure to update evidence, failures=%d evidence=%+v", len(actor.turns.pending.provider.journal.Failures()), actor.turns.pending.provider.journal.Project())
	}
}

func TestResponsesWSPendingEvidenceOnlyObservationDoesNotEnterReplayBuffer(t *testing.T) {
	actor := &ResponsesWSSessionActor{
		turns: responsesWSTurnSlots{
			pending: responsesWSPendingTurn{attempt: &ResponsesWSTurnAttempt{
				AttemptID: "attempt-evidence-only",
				Usage:     &types.Usage{},
			}},
		},
	}
	usage := ResponsesWSEventProviderUsageObserved{
		AttemptID:    actor.turns.pending.attempt.AttemptID,
		Usage:        &types.UsageEvent{TotalTokens: 1},
		DetailOrigin: responsesws.RecvDetailOriginProviderFrame,
	}
	actor.appendPendingProviderLifecycle(upstreamEventFromProviderUsage(usage))
	if !actor.turns.pending.provider.journal.Project().HasActivity() {
		t.Fatalf("expected usage-only observation to update evidence, evidence=%+v", actor.turns.pending.provider.journal.Project())
	}
	if len(actor.turns.pending.provider.journal.DownstreamEvents()) != 0 || len(actor.turns.pending.provider.journal.Failures()) != 0 {
		t.Fatalf("evidence-only observation must not enter replay buffers, events=%d failures=%d", len(actor.turns.pending.provider.journal.DownstreamEvents()), len(actor.turns.pending.provider.journal.Failures()))
	}
}

func TestResponsesWSRelayUpstreamLifecycleAndIOHelpers(t *testing.T) {
	tests := []struct {
		name                         string
		event                        responsesws.UpstreamEvent
		wantLifecycle                bool
		wantFailureLifecycle         bool
		wantIdleClose                bool
		wantBridgeEOF                bool
		wantProviderMalformedPayload bool
		wantZeroProof                responsesws.ZeroChargeProofCandidate
	}{
		{
			name:          "bridge open provider error",
			event:         responsesws.UpstreamEvent{DetailOrigin: responsesws.RecvDetailOriginBridgeOpenProviderError},
			wantZeroProof: responsesws.ZeroChargeProofCandidateProviderRejectedBeforeStream,
		},
		{
			name:                 "bridge stream local error",
			event:                responsesws.UpstreamEvent{DetailOrigin: responsesws.RecvDetailOriginBridgeStreamError},
			wantFailureLifecycle: true,
		},
		{
			name:          "bridge stream opened",
			event:         responsesws.UpstreamEvent{DetailOrigin: responsesws.RecvDetailOriginBridgeStreamOpened},
			wantLifecycle: true,
		},
		{
			name:                 "bridge stream eof",
			event:                responsesws.UpstreamEvent{DetailOrigin: responsesws.RecvDetailOriginBridgeStreamEOF},
			wantFailureLifecycle: true,
			wantBridgeEOF:        true,
		},
		{
			name:                 "native provider eof closes idle session",
			event:                responsesws.UpstreamEvent{DetailOrigin: responsesws.RecvDetailOriginNativeProviderEOF},
			wantFailureLifecycle: true,
			wantIdleClose:        true,
		},
		{
			name:                         "provider malformed has client payload",
			event:                        responsesws.UpstreamEvent{DetailOrigin: responsesws.RecvDetailOriginProviderMalformed},
			wantFailureLifecycle:         true,
			wantIdleClose:                true,
			wantProviderMalformedPayload: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lifecycle := responsesWSProviderLifecyclePolicyForEvent(tc.event)
			zeroProof := responsesWSUpstreamZeroChargeProofCandidate(tc.event)
			if lifecycle.DeliverRecvLifecycleEvent != tc.wantLifecycle ||
				lifecycle.DeliverRecvFailureLifecycleEvent != tc.wantFailureLifecycle ||
				lifecycle.IdleRecvFailureClosesSession != tc.wantIdleClose ||
				lifecycle.BridgeStreamEOF != tc.wantBridgeEOF ||
				lifecycle.ProviderMalformedClientPayload != tc.wantProviderMalformedPayload ||
				zeroProof != tc.wantZeroProof {
				t.Fatalf("helper mismatch: lifecycle=%+v zero=%v", lifecycle, zeroProof)
			}
		})
	}
}

func TestResponsesWSLifecycleDetailOriginDoesNotBecomeSettlementEvidence(t *testing.T) {
	event := responsesws.UpstreamEvent{DetailOrigin: responsesws.RecvDetailOriginBridgeStreamError}
	if !responsesWSProviderLifecyclePolicyForEvent(event).DeliverRecvFailureLifecycleEvent {
		t.Fatal("expected bridge stream error to drive recv failure lifecycle behavior")
	}
	projected := ProjectResponsesWSProviderEvidence(ResponsesWSProviderEvidenceProjectionInput{
		Provider: responsesWSSettlementProviderProjection(responsesws.ProviderObservation{
			DetailOrigin: responsesws.RecvDetailOriginBridgeStreamError,
			HasError:     true,
		}),
	})
	if projected.Evidence.AnyProviderActivityEvidence || projected.ZeroChargeProofCandidate.Present() {
		t.Fatalf("lifecycle detail must not become settlement evidence: %+v", projected)
	}
}

func TestResponsesWSSettlementDecisionCanonicalizesFlags(t *testing.T) {
	decision := newResponsesWSSettlementDecision(
		ResponsesWSSettlementFinalizeFloor,
		ResponsesWSSettlementBasisFloor,
		100,
		"canonical",
		[]ResponsesWSSettlementFlag{
			ResponsesWSSettlementFlagZeroChargeProofSuppressed,
			ResponsesWSSettlementFlagContradictoryInput,
			ResponsesWSSettlementFlagZeroChargeProofSuppressed,
			"",
		},
	)
	wantFlags := []ResponsesWSSettlementFlag{
		ResponsesWSSettlementFlagContradictoryInput,
		ResponsesWSSettlementFlagZeroChargeProofSuppressed,
	}
	if !reflect.DeepEqual(decision.Flags, wantFlags) {
		t.Fatalf("expected canonical flags %+v, got %+v", wantFlags, decision.Flags)
	}

	reordered := newResponsesWSSettlementDecision(
		ResponsesWSSettlementFinalizeFloor,
		ResponsesWSSettlementBasisFloor,
		100,
		"canonical",
		[]ResponsesWSSettlementFlag{
			ResponsesWSSettlementFlagZeroChargeProofSuppressed,
			ResponsesWSSettlementFlagContradictoryInput,
		},
	)
	if decision.DecisionKey != reordered.DecisionKey {
		t.Fatalf("expected reordered flags to keep decision key stable: %q != %q", decision.DecisionKey, reordered.DecisionKey)
	}

	differentReason := newResponsesWSSettlementDecision(
		ResponsesWSSettlementFinalizeFloor,
		ResponsesWSSettlementBasisFloor,
		100,
		"different diagnostic reason",
		[]ResponsesWSSettlementFlag{
			ResponsesWSSettlementFlagZeroChargeProofSuppressed,
			ResponsesWSSettlementFlagContradictoryInput,
		},
	)
	if decision.DecisionKey != differentReason.DecisionKey {
		t.Fatalf("expected diagnostic reason not to affect decision key: %q != %q", decision.DecisionKey, differentReason.DecisionKey)
	}
}
