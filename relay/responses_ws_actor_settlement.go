package relay

import (
	"context"
	"fmt"
)

func (a *ResponsesWSSessionActor) applyPendingSettlement() (ResponsesWSSettlementDecision, ResponsesWSAppliedSettlement, error) {
	if a == nil || a.turns.pending.attempt == nil {
		return ResponsesWSSettlementDecision{}, ResponsesWSAppliedSettlement{}, fmt.Errorf("responses websocket pending attempt is required for settlement")
	}
	decision := projectResponsesWSSharedDecision(a.turns.pending.attempt)
	operationCtx, cancel := boundedResponsesLifecycleContext(context.WithoutCancel(a.logContext()))
	defer cancel()
	applied, err := a.turns.pending.attempt.ApplyResponsesWSSettlementDecisionWithContext(operationCtx, a.Context(), decision)
	return decision, applied, err
}

func (a *ResponsesWSSessionActor) applyActiveSettlement() (ResponsesWSSettlementDecision, ResponsesWSAppliedSettlement, error) {
	if a == nil || a.turns.active.attempt == nil {
		return ResponsesWSSettlementDecision{}, ResponsesWSAppliedSettlement{}, fmt.Errorf("responses websocket active attempt is required for settlement")
	}
	decision := projectResponsesWSSharedDecision(a.turns.active.attempt)
	operationCtx, cancel := boundedResponsesLifecycleContext(context.WithoutCancel(a.logContext()))
	defer cancel()
	applied, err := a.turns.active.attempt.ApplyResponsesWSSettlementDecisionWithContext(operationCtx, a.Context(), decision)
	return decision, applied, err
}

func projectResponsesWSSharedDecision(attempt *ResponsesWSTurnAttempt) ResponsesWSSettlementDecision {
	if attempt == nil || attempt.Billing == nil {
		return newResponsesWSSettlementDecision(ResponsesWSSettlementNoop)
	}
	if !attempt.Billing.SubmissionClaimed() {
		return newResponsesWSSettlementDecision(ResponsesWSSettlementRollbackReserve)
	}
	if attempt.TerminalUsage != nil {
		return newResponsesWSSettlementDecision(ResponsesWSSettlementFinalizeExactUsage)
	}
	// A submission may have independently priceable provider components even
	// when its token partition is absent or incomplete. The shared component
	// reducer is the sole owner of deciding whether anything can be confirmed.
	return newResponsesWSSettlementDecision(ResponsesWSSettlementFinalizeProviderUsage)
}
