package relay

type ResponsesWSSettlementAction int

const (
	ResponsesWSSettlementNoop ResponsesWSSettlementAction = iota
	ResponsesWSSettlementRollbackReserve
	ResponsesWSSettlementFinalizeExactUsage
	ResponsesWSSettlementFinalizeProviderUsage
)

type ResponsesWSSettlementDecision struct {
	Action ResponsesWSSettlementAction
}

type ResponsesWSAppliedSettlement struct {
	AttemptID string

	Action ResponsesWSSettlementAction

	AppliedFinalQuota int64
}

func newResponsesWSSettlementDecision(action ResponsesWSSettlementAction) ResponsesWSSettlementDecision {
	return ResponsesWSSettlementDecision{Action: action}
}
