package relay

import (
	"fmt"
	"time"

	"one-api/common/logger"
	"one-api/common/responsesws"
)

func (a *ResponsesWSSessionActor) buildPendingSettlementInput(reason string, proof ResponsesWSZeroChargeProof) ResponsesWSSettlementInput {
	if a == nil || a.turns.pending.attempt == nil {
		return ResponsesWSSettlementInput{}
	}
	return a.buildSettlementInputFromAttempt(a.turns.pending.attempt, a.turns.pending.provider.journal.Project(), reason, proof)
}

func (a *ResponsesWSSessionActor) buildActiveSettlementInput(reason string, proof ResponsesWSZeroChargeProof) ResponsesWSSettlementInput {
	if a == nil || a.turns.active.attempt == nil {
		return ResponsesWSSettlementInput{}
	}
	return a.buildSettlementInputFromAttempt(a.turns.active.attempt, a.turns.active.evidence, reason, proof)
}

func (a *ResponsesWSSessionActor) applyPendingSettlement(input ResponsesWSSettlementInput) (ResponsesWSSettlementDecision, ResponsesWSAppliedSettlement, error) {
	if a == nil || a.turns.pending.attempt == nil {
		return ResponsesWSSettlementDecision{}, ResponsesWSAppliedSettlement{}, fmt.Errorf("responses websocket pending attempt is required for settlement")
	}
	decision := decideResponsesWSSettlement(input)
	applied, err := a.turns.pending.attempt.ApplyResponsesWSSettlementDecision(a.Context(), decision)
	if err == nil {
		a.emitSettlementTrace(input, decision, applied)
	}
	return decision, applied, err
}

func (a *ResponsesWSSessionActor) applyActiveSettlement(input ResponsesWSSettlementInput) (ResponsesWSSettlementDecision, ResponsesWSAppliedSettlement, error) {
	if a == nil || a.turns.active.attempt == nil {
		return ResponsesWSSettlementDecision{}, ResponsesWSAppliedSettlement{}, fmt.Errorf("responses websocket active attempt is required for settlement")
	}
	decision := decideResponsesWSSettlement(input)
	applied, err := a.turns.active.attempt.ApplyResponsesWSSettlementDecision(a.Context(), decision)
	if err == nil {
		a.emitSettlementTrace(input, decision, applied)
	}
	return decision, applied, err
}

func (a *ResponsesWSSessionActor) buildSettlementInputFromAttempt(attempt *ResponsesWSTurnAttempt, provider responsesws.ProviderSettlementLogProjection, reason string, proof ResponsesWSZeroChargeProof) ResponsesWSSettlementInput {
	if attempt == nil {
		return ResponsesWSSettlementInput{}
	}
	observed := int64(0)
	if attempt.Quota != nil && responsesWSUsageHasBillableEvidence(attempt.Usage) {
		observed = int64(attempt.Quota.GetTotalQuotaByUsage(attempt.Usage))
	}
	return BuildResponsesWSSettlementInput(ResponsesWSSettlementProjectionInput{
		AttemptID:              attempt.AttemptID,
		OpeningID:              attempt.OpeningID,
		FloorQuota:             responsesWSAttemptFloorQuota(attempt),
		Terminal:               attempt.TerminalEvidence,
		ObservedBillableQuota:  observed,
		Provider:               provider,
		TransportResult:        attempt.TransportResult,
		CloseReason:            reason,
		EventKind:              reason,
		ZeroChargeProofRequest: proof,
	})
}

func (a *ResponsesWSSessionActor) emitSettlementTrace(input ResponsesWSSettlementInput, decision ResponsesWSSettlementDecision, applied ResponsesWSAppliedSettlement) {
	if a == nil {
		return
	}
	trace := ResponsesWSSettlementTrace{
		AttemptID: input.AttemptID,
		OpeningID: input.OpeningID,
		ChannelID: appliedSettlementChannelID(a, input.AttemptID),
		Input:     input,
		Decision:  decision,
		Applied:   applied,
		CreatedAt: time.Now(),
	}
	if replayed := decideResponsesWSSettlement(trace.Input); replayed.DecisionKey != trace.Decision.DecisionKey {
		logger.LogError(a.logContext(), fmt.Sprintf("responses websocket settlement trace replay mismatch: attempt_id=%s decision=%s replayed=%s", trace.AttemptID, trace.Decision.DecisionKey, replayed.DecisionKey))
	}
	if trace.Decision.ExpectedFinalQuota != trace.Applied.AppliedFinalQuota {
		logger.LogError(a.logContext(), fmt.Sprintf("responses websocket settlement applied mismatch: attempt_id=%s expected=%d applied=%d", trace.AttemptID, trace.Decision.ExpectedFinalQuota, trace.Applied.AppliedFinalQuota))
	}
	if responsesWSSettlementTraceHook != nil {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					logger.LogError(a.logContext(), fmt.Sprintf("responses websocket settlement trace hook panic: %v", recovered))
				}
			}()
			responsesWSSettlementTraceHook(trace)
		}()
	}
}

func appliedSettlementChannelID(a *ResponsesWSSessionActor, attemptID string) int {
	if a == nil {
		return 0
	}
	if a.turns.pending.attempt != nil && a.turns.pending.attempt.AttemptID == attemptID {
		return a.turns.pending.attempt.SelectedChannelID
	}
	if a.turns.active.attempt != nil && a.turns.active.attempt.AttemptID == attemptID {
		return a.turns.active.attempt.SelectedChannelID
	}
	return a.turns.active.channelID
}

func responsesWSAttemptFloorQuota(attempt *ResponsesWSTurnAttempt) int64 {
	if attempt == nil || attempt.Quota == nil {
		return 0
	}
	return int64(attempt.Quota.PreConsumedQuota())
}

func responsesWSZeroChargeProof(kind ResponsesWSZeroChargeProofKind, reason string) ResponsesWSZeroChargeProof {
	return ResponsesWSZeroChargeProof{Kind: kind, Reason: reason}
}

func responsesWSProviderRejectedBeforeAcceptProof(reason string) ResponsesWSZeroChargeProof {
	return ResponsesWSZeroChargeProof{
		Kind:                                 ResponsesWSZeroChargeProofProviderRejectedBeforeAccept,
		Reason:                               reason,
		providerRejectedBeforeAcceptEvidence: true,
	}
}

func responsesWSTransportSendStatusLabel(result responsesws.ResponsesWSTransportSendResult) string {
	if result.Status != "" {
		if err := responsesws.ValidateResponsesWSTransportSendResult(result); err != nil {
			return "invalid"
		}
	}
	switch responsesWSTransportSendStatus(result) {
	case responsesws.ResponsesWSTransportSendNotAttempted:
		return "not_attempted"
	case responsesws.ResponsesWSTransportSendRejectedBeforeStream:
		return "rejected_before_stream"
	case responsesws.ResponsesWSTransportSendAttempted:
		return "attempted"
	case responsesws.ResponsesWSTransportSendAmbiguous:
		return "ambiguous"
	default:
		return "unknown"
	}
}
