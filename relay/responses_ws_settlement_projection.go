package relay

import (
	"strings"

	"one-api/common/responsesws"
)

type ResponsesWSProviderEvidenceProjectionInput struct {
	Provider              responsesws.ProviderSettlementLogProjection
	Terminal              *ResponsesWSTerminalEvidence
	ObservedBillableQuota int64

	TransportStatus string
	CloseReason     string
	EventKind       string
}

type ResponsesWSSettlementProjectionInput struct {
	AttemptID  string
	OpeningID  string
	FloorQuota int64

	Terminal              *ResponsesWSTerminalEvidence
	ObservedBillableQuota int64
	Provider              responsesws.ProviderSettlementLogProjection

	TransportResult responsesws.ResponsesWSTransportSendResult
	CloseReason     string
	EventKind       string

	ZeroChargeProofRequest ResponsesWSZeroChargeProof
}

type ResponsesWSProviderEvidenceProjectionResult struct {
	Evidence                 ResponsesWSSettlementEvidence
	Diagnostics              ResponsesWSSettlementDiagnostics
	ZeroChargeProofCandidate responsesws.ZeroChargeProofCandidate
}

type responsesWSProviderAccountingEventProjection struct {
	UpstreamEvent               responsesws.UpstreamEvent
	Diagnostic                  responsesws.DiagnosticDetail
	HasProviderActivityEvidence bool
	ZeroChargeProofCandidate    responsesws.ZeroChargeProofCandidate
}

func ProjectResponsesWSProviderEvidence(in ResponsesWSProviderEvidenceProjectionInput) ResponsesWSProviderEvidenceProjectionResult {
	observations := in.Provider
	observed := clampNonNegativeInt64(in.ObservedBillableQuota)
	terminal := cloneResponsesWSTerminalEvidence(in.Terminal)
	detailOrigins := make([]string, 0, len(observations.DetailOrigins))
	for _, origin := range observations.DetailOrigins {
		if origin != "" {
			detailOrigins = append(detailOrigins, string(origin))
		}
	}
	candidate := observations.FirstZeroChargeProofCandidate()
	return ResponsesWSProviderEvidenceProjectionResult{
		Evidence: ResponsesWSSettlementEvidence{
			Terminal:              terminal,
			ObservedBillableQuota: observed,
			AnyProviderActivityEvidence: observations.Activity.HasActivity() ||
				observed > 0 ||
				terminal != nil,
		},
		Diagnostics: ResponsesWSSettlementDiagnostics{
			ProviderStreamOpened:  observations.Activity.ProviderStreamOpened,
			ProviderFrameSeen:     observations.Activity.ProviderFrameSeen,
			ProviderUsageSeen:     observations.Activity.ProviderUsageSeen,
			ProviderPeerCloseSeen: observations.Activity.ProviderPeerCloseSeen,
			DetailOrigins:         detailOrigins,
			TransportStatus:       strings.TrimSpace(in.TransportStatus),
			CloseReason:           strings.TrimSpace(in.CloseReason),
			EventKind:             strings.TrimSpace(in.EventKind),
		},
		ZeroChargeProofCandidate: candidate,
	}
}

func BuildResponsesWSSettlementInput(in ResponsesWSSettlementProjectionInput) ResponsesWSSettlementInput {
	projected := ProjectResponsesWSProviderEvidence(ResponsesWSProviderEvidenceProjectionInput{
		Terminal:              in.Terminal,
		ObservedBillableQuota: in.ObservedBillableQuota,
		Provider:              in.Provider,
		TransportStatus:       responsesWSTransportSendStatusLabel(in.TransportResult),
		CloseReason:           in.CloseReason,
		EventKind:             in.EventKind,
	})
	return ResponsesWSSettlementInput{
		AttemptID:       strings.TrimSpace(in.AttemptID),
		OpeningID:       strings.TrimSpace(in.OpeningID),
		FloorQuota:      clampNonNegativeInt64(in.FloorQuota),
		ZeroChargeProof: responsesWSSettlementZeroChargeProof(in, projected),
		Evidence:        projected.Evidence,
		Diagnostics:     projected.Diagnostics,
	}
}

func responsesWSSettlementZeroChargeProof(in ResponsesWSSettlementProjectionInput, projected ResponsesWSProviderEvidenceProjectionResult) ResponsesWSZeroChargeProof {
	request := in.ZeroChargeProofRequest
	if !request.Present() {
		return ResponsesWSZeroChargeProof{}
	}
	switch request.Kind {
	case ResponsesWSZeroChargeProofProviderRejectedBeforeStream:
		if projected.ZeroChargeProofCandidate != responsesws.ZeroChargeProofCandidateProviderRejectedBeforeStream &&
			responsesWSTransportSendStatus(in.TransportResult) != responsesws.ResponsesWSTransportSendRejectedBeforeStream {
			return ResponsesWSZeroChargeProof{}
		}
	case ResponsesWSZeroChargeProofProviderRejectedBeforeAccept:
		// Provider request-level rejection is a typed attempt event, not generic
		// provider activity. It may have arrived as a native WS frame that is
		// intentionally kept out of the settlement projection. Accept this proof
		// only when it was constructed by the request-level rejection normalizer;
		// independent acceptance/usage evidence still suppresses it later.
		if !request.providerRejectedBeforeAcceptEvidence {
			return ResponsesWSZeroChargeProof{}
		}
	case ResponsesWSZeroChargeProofTransportNotAttempted:
		status := responsesWSTransportSendStatus(in.TransportResult)
		if status != "" && status != responsesws.ResponsesWSTransportSendNotAttempted {
			return ResponsesWSZeroChargeProof{}
		}
	}
	request.Reason = strings.TrimSpace(request.Reason)
	if request.Reason == "" {
		request.Reason = strings.TrimSpace(in.EventKind)
	}
	return request
}

func projectResponsesWSProviderDownstreamAccountingEvent(event ResponsesWSEventProviderDownstream) responsesWSProviderAccountingEventProjection {
	return projectResponsesWSUpstreamAccountingEvent(upstreamEventFromProviderDownstream(event))
}

func projectResponsesWSProviderUsageAccountingEvent(event ResponsesWSEventProviderUsageObserved) responsesWSProviderAccountingEventProjection {
	return projectResponsesWSUpstreamAccountingEvent(upstreamEventFromProviderUsage(event))
}

func projectResponsesWSUpstreamAccountingEvent(event responsesws.UpstreamEvent) responsesWSProviderAccountingEventProjection {
	upstream := event
	projected := responsesws.ProjectProviderObservationForSettlement(responsesws.NewProviderObservation(upstream))
	return responsesWSProviderAccountingEventProjection{
		UpstreamEvent:               upstream,
		Diagnostic:                  projected.Diagnostic,
		HasProviderActivityEvidence: projected.HasProviderActivity,
		ZeroChargeProofCandidate:    projected.ZeroChargeProofCandidate,
	}
}

func responsesWSUpstreamZeroChargeProofCandidate(event responsesws.UpstreamEvent) responsesws.ZeroChargeProofCandidate {
	return projectResponsesWSUpstreamAccountingEvent(event).ZeroChargeProofCandidate
}
