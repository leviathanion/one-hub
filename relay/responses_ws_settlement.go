package relay

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"one-api/common/responsesws"
)

type ResponsesWSTerminalKind = responsesws.ResponsesTerminalKind

type ResponsesWSZeroChargeProofKind int

const (
	ResponsesWSZeroChargeProofNone ResponsesWSZeroChargeProofKind = iota
	ResponsesWSZeroChargeProofPrepareFailed
	ResponsesWSZeroChargeProofRewriteFailed
	ResponsesWSZeroChargeProofQuotaRejected
	ResponsesWSZeroChargeProofClientClosedBeforeSend
	ResponsesWSZeroChargeProofTransportNotAttempted
	ResponsesWSZeroChargeProofProviderRejectedBeforeStream
	ResponsesWSZeroChargeProofProviderRejectedBeforeAccept
)

type ResponsesWSZeroChargeProof struct {
	Kind   ResponsesWSZeroChargeProofKind
	Reason string

	providerRejectedBeforeAcceptEvidence bool
}

func (p ResponsesWSZeroChargeProof) Present() bool {
	return p.Kind != ResponsesWSZeroChargeProofNone
}

type ResponsesWSSettlementEvidence struct {
	Terminal                    *ResponsesWSTerminalEvidence
	ObservedBillableQuota       int64
	AnyProviderActivityEvidence bool
}

type ResponsesWSTerminalEvidence struct {
	Kind       ResponsesWSTerminalKind
	ResponseID string
	// Presence of provider terminal usage is authoritative even when its
	// computed billable quota is zero.
	HasTerminalUsage bool
	BillableQuota    int64
}

type ResponsesWSSettlementDiagnostics struct {
	ProviderStreamOpened  bool
	ProviderFrameSeen     bool
	ProviderUsageSeen     bool
	ProviderPeerCloseSeen bool

	DetailOrigins   []string
	TransportStatus string
	CloseReason     string
	EventKind       string
}

type ResponsesWSSettlementInput struct {
	AttemptID  string
	OpeningID  string
	FloorQuota int64

	ZeroChargeProof ResponsesWSZeroChargeProof
	Evidence        ResponsesWSSettlementEvidence

	Diagnostics ResponsesWSSettlementDiagnostics
}

type ResponsesWSSettlementAction int

const (
	ResponsesWSSettlementNoop ResponsesWSSettlementAction = iota
	ResponsesWSSettlementRollbackReserve
	ResponsesWSSettlementFinalizeExactUsage
	ResponsesWSSettlementFinalizeFloor
	ResponsesWSSettlementFinalizeObservedOrFloor
)

type ResponsesWSSettlementBasis int

const (
	ResponsesWSSettlementBasisNone ResponsesWSSettlementBasis = iota
	ResponsesWSSettlementBasisZeroChargeProof
	ResponsesWSSettlementBasisTerminalUsage
	ResponsesWSSettlementBasisFloor
	ResponsesWSSettlementBasisObservedOrFloor
)

type ResponsesWSSettlementFlag string

const (
	ResponsesWSSettlementFlagTerminalUsage             ResponsesWSSettlementFlag = "terminal_usage"
	ResponsesWSSettlementFlagProviderActivityEvidence  ResponsesWSSettlementFlag = "provider_activity_evidence"
	ResponsesWSSettlementFlagObservedBillableUsage     ResponsesWSSettlementFlag = "observed_billable_usage"
	ResponsesWSSettlementFlagNoProviderEvidence        ResponsesWSSettlementFlag = "no_provider_evidence"
	ResponsesWSSettlementFlagZeroChargeProof           ResponsesWSSettlementFlag = "zero_charge_proof"
	ResponsesWSSettlementFlagZeroChargeProofSuppressed ResponsesWSSettlementFlag = "zero_charge_proof_suppressed"
	ResponsesWSSettlementFlagContradictoryInput        ResponsesWSSettlementFlag = "contradictory_input"
	// MissingSettlementFloor means the chosen settlement path depends on a
	// floor/fallback amount, but the projected floor is zero.
	ResponsesWSSettlementFlagMissingSettlementFloor ResponsesWSSettlementFlag = "missing_settlement_floor"
)

type ResponsesWSSettlementDecision struct {
	Action ResponsesWSSettlementAction
	Basis  ResponsesWSSettlementBasis

	ExpectedFinalQuota int64

	// Reason is diagnostic-only. Actor control flow must not branch on it.
	Reason string

	// Flags are accounting-only and intentionally exclude transport detail.
	Flags []ResponsesWSSettlementFlag

	DecisionKey string
}

type ResponsesWSAppliedSettlement struct {
	AttemptID string

	Action ResponsesWSSettlementAction
	Basis  ResponsesWSSettlementBasis

	ExpectedFinalQuota int64
	AppliedFinalQuota  int64

	// SettlementIdentity is the quota-ledger idempotency identity returned by
	// ConsumeFixedFinalQuotaWithUsageIdentity. It is not a response/turn ID.
	SettlementIdentity string
}

type ResponsesWSSettlementTrace struct {
	AttemptID string
	OpeningID string
	ChannelID int

	Input    ResponsesWSSettlementInput
	Decision ResponsesWSSettlementDecision
	Applied  ResponsesWSAppliedSettlement

	CreatedAt time.Time
}

var responsesWSSettlementTraceHook func(ResponsesWSSettlementTrace)

func decideResponsesWSSettlement(in ResponsesWSSettlementInput) ResponsesWSSettlementDecision {
	floor := clampNonNegativeInt64(in.FloorQuota)
	observed := clampNonNegativeInt64(in.Evidence.ObservedBillableQuota)

	flags := make([]ResponsesWSSettlementFlag, 0, 4)

	terminal := in.Evidence.Terminal
	hasTerminal := terminal != nil
	hasTerminalUsage := terminal != nil && terminal.HasTerminalUsage
	hasObservedUsage := observed > 0
	hasProviderActivity := in.Evidence.AnyProviderActivityEvidence || hasObservedUsage || hasTerminal
	hasZeroChargeProof := in.ZeroChargeProof.Present()

	if hasZeroChargeProof && hasProviderActivity {
		flags = append(flags,
			ResponsesWSSettlementFlagContradictoryInput,
			ResponsesWSSettlementFlagZeroChargeProofSuppressed,
		)
	}

	if hasTerminalUsage {
		flags = append(flags, ResponsesWSSettlementFlagTerminalUsage)
		return newResponsesWSSettlementDecision(
			ResponsesWSSettlementFinalizeExactUsage,
			ResponsesWSSettlementBasisTerminalUsage,
			terminal.BillableQuota,
			"terminal_billable_usage",
			flags,
		)
	}

	if hasProviderActivity {
		flags = append(flags, ResponsesWSSettlementFlagProviderActivityEvidence)
		if hasObservedUsage {
			flags = append(flags, ResponsesWSSettlementFlagObservedBillableUsage)
			flags = responsesWSSettlementFloorPathFlags(flags, floor)
			return newResponsesWSSettlementDecision(
				ResponsesWSSettlementFinalizeObservedOrFloor,
				ResponsesWSSettlementBasisObservedOrFloor,
				maxInt64(observed, floor),
				"provider_evidence_observed_or_floor",
				flags,
			)
		}
		flags = responsesWSSettlementFloorPathFlags(flags, floor)
		return newResponsesWSSettlementDecision(
			ResponsesWSSettlementFinalizeFloor,
			ResponsesWSSettlementBasisFloor,
			floor,
			"provider_evidence_floor",
			flags,
		)
	}

	if hasZeroChargeProof {
		flags = append(flags, ResponsesWSSettlementFlagZeroChargeProof)
		return newResponsesWSSettlementDecision(
			ResponsesWSSettlementRollbackReserve,
			ResponsesWSSettlementBasisZeroChargeProof,
			0,
			"zero_charge_proof",
			flags,
		)
	}

	flags = append(flags, ResponsesWSSettlementFlagNoProviderEvidence)
	flags = responsesWSSettlementFloorPathFlags(flags, floor)
	return newResponsesWSSettlementDecision(
		ResponsesWSSettlementFinalizeFloor,
		ResponsesWSSettlementBasisFloor,
		floor,
		"uncertain_no_provider_evidence_floor",
		flags,
	)
}

func responsesWSSettlementFloorPathFlags(flags []ResponsesWSSettlementFlag, floor int64) []ResponsesWSSettlementFlag {
	if floor == 0 {
		flags = append(flags, ResponsesWSSettlementFlagMissingSettlementFloor)
	}
	return flags
}

func newResponsesWSSettlementDecision(action ResponsesWSSettlementAction, basis ResponsesWSSettlementBasis, expectedFinalQuota int64, reason string, flags []ResponsesWSSettlementFlag) ResponsesWSSettlementDecision {
	expectedFinalQuota = clampNonNegativeInt64(expectedFinalQuota)
	copiedFlags := canonicalResponsesWSSettlementFlags(flags)
	decision := ResponsesWSSettlementDecision{
		Action:             action,
		Basis:              basis,
		ExpectedFinalQuota: expectedFinalQuota,
		Reason:             strings.TrimSpace(reason),
		Flags:              copiedFlags,
	}
	decision.DecisionKey = responsesWSSettlementDecisionKey(decision)
	return decision
}

func canonicalResponsesWSSettlementFlags(flags []ResponsesWSSettlementFlag) []ResponsesWSSettlementFlag {
	if len(flags) == 0 {
		return nil
	}
	seen := make(map[ResponsesWSSettlementFlag]struct{}, len(flags))
	copied := make([]ResponsesWSSettlementFlag, 0, len(flags))
	for _, flag := range flags {
		if flag == "" {
			continue
		}
		if _, ok := seen[flag]; ok {
			continue
		}
		seen[flag] = struct{}{}
		copied = append(copied, flag)
	}
	sort.Slice(copied, func(i, j int) bool {
		return string(copied[i]) < string(copied[j])
	})
	return copied
}

func responsesWSSettlementHasFlag(decision ResponsesWSSettlementDecision, want ResponsesWSSettlementFlag) bool {
	for _, flag := range decision.Flags {
		if flag == want {
			return true
		}
	}
	return false
}

func responsesWSSettlementDecisionKey(d ResponsesWSSettlementDecision) string {
	flags := make([]string, 0, len(d.Flags))
	for _, flag := range d.Flags {
		flags = append(flags, string(flag))
	}
	return fmt.Sprintf("%d:%d:%d:%s", d.Action, d.Basis, d.ExpectedFinalQuota, strings.Join(flags, ","))
}

func cloneResponsesWSTerminalEvidence(in *ResponsesWSTerminalEvidence) *ResponsesWSTerminalEvidence {
	if in == nil {
		return nil
	}
	cloned := *in
	cloned.ResponseID = strings.TrimSpace(cloned.ResponseID)
	cloned.BillableQuota = clampNonNegativeInt64(cloned.BillableQuota)
	return &cloned
}

func clampNonNegativeInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
