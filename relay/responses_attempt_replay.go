package relay

import (
	"net/http"

	"one-api/common"
	"one-api/common/config"
	"one-api/types"
)

type ResponsesAttemptUpstreamDisposition int

const (
	ResponsesAttemptUpstreamUnknown ResponsesAttemptUpstreamDisposition = iota
	ResponsesAttemptUpstreamNotAttempted
	ResponsesAttemptUpstreamRejectedBeforeAccept
	ResponsesAttemptUpstreamAccepted
	ResponsesAttemptUpstreamFailedAfterAccept
	ResponsesAttemptUpstreamAmbiguous
)

type ResponsesAttemptDownstreamDisposition int

const (
	ResponsesAttemptDownstreamUncommitted ResponsesAttemptDownstreamDisposition = iota
	ResponsesAttemptDownstreamCommitted
)

type ResponsesAttemptAccountingDisposition int

const (
	ResponsesAttemptAccountingNoEvidence ResponsesAttemptAccountingDisposition = iota
	ResponsesAttemptAccountingZeroChargeProofAvailable
	ResponsesAttemptAccountingAcceptanceEvidenceSeen
	ResponsesAttemptAccountingUsageSeen
	ResponsesAttemptAccountingFinalized
)

type ResponsesAttemptReplayCapability int

const (
	ResponsesAttemptReplayNone ResponsesAttemptReplayCapability = iota
	ResponsesAttemptReplayRawCreateFirstTurn
)

type ResponsesAttemptAffinityMode int

const (
	ResponsesAttemptAffinityFree ResponsesAttemptAffinityMode = iota
	ResponsesAttemptAffinityPreferred
	ResponsesAttemptAffinityStrict
	ResponsesAttemptAffinityExplicitPin
)

type ResponsesAttemptTurnKind int

const (
	ResponsesAttemptTurnFirst ResponsesAttemptTurnKind = iota
	ResponsesAttemptTurnContinuation
)

type ResponsesAttemptFailureOrigin int

const (
	ResponsesAttemptFailureOriginUnknown ResponsesAttemptFailureOrigin = iota
	ResponsesAttemptFailureOriginHTTPStatus
	ResponsesAttemptFailureOriginWSProviderRequestError
	ResponsesAttemptFailureOriginBridgeOpenProviderError
	ResponsesAttemptFailureOriginTransportNotAttempted
	ResponsesAttemptFailureOriginTransportAmbiguous
	ResponsesAttemptFailureOriginNoFirstProviderPayload
)

type ResponsesContinuationAnchor struct {
	PreviousResponseID string
	BoundChannelID     int
	BoundConnID        string
	HasFullContext     bool
	HasToolOutput      bool
	Strict             bool
}

type ResponsesDownstreamCommitKind int

const (
	DownstreamCommitNone ResponsesDownstreamCommitKind = iota
	DownstreamCommitProviderFrame
	DownstreamCommitProxyError
	DownstreamCommitSyntheticFrame
	DownstreamCommitKeepalive
	DownstreamCommitClosePayload
)

type ResponsesDownstreamWatermark struct {
	AttemptID string
	Seq       uint64
	Committed bool
	Kind      ResponsesDownstreamCommitKind
}

type ResponsesAttemptSnapshot struct {
	AttemptID string
	OpeningID string

	Upstream   ResponsesAttemptUpstreamDisposition
	Downstream ResponsesAttemptDownstreamDisposition
	Accounting ResponsesAttemptAccountingDisposition

	Failure *types.OpenAIErrorWithStatusCode
	Origin  ResponsesAttemptFailureOrigin

	Replay   ResponsesAttemptReplayCapability
	Affinity ResponsesAttemptAffinityMode
	Turn     ResponsesAttemptTurnKind
	// Preferred affinity is not a correctness barrier by itself. This flag
	// carries the explicit operator choice to surface rather than retry after a
	// preferred channel failure.
	SkipRetryAfterPreferredFailure bool
	PendingCreateCancel            bool

	Continuation ResponsesContinuationAnchor
	Watermark    ResponsesDownstreamWatermark
}

type ResponsesAttemptReplayDecision int

const (
	ResponsesAttemptDecisionSurface ResponsesAttemptReplayDecision = iota
	ResponsesAttemptDecisionRollbackAndRetryNextChannel
	ResponsesAttemptDecisionRollbackAndSurface
	ResponsesAttemptDecisionNoRetryAmbiguous
	ResponsesAttemptDecisionClose
)

type ResponsesChannelFailure int

const (
	ChannelFailureNone ResponsesChannelFailure = iota
	ChannelFailureRateLimited
	ChannelFailureQuotaExhausted
	ChannelFailureTransient5xx
	ChannelFailureAuthRejected
	ChannelFailureTransportNotAttempted
	ChannelFailureAmbiguous
)

type ResponsesReplayBlockingBarrier int

const (
	ReplayBarrierNone ResponsesReplayBlockingBarrier = iota
	ReplayBarrierDownstreamCommitted
	ReplayBarrierProviderAccepted
	ReplayBarrierAccounting
	ReplayBarrierContinuation
	ReplayBarrierAffinity
	ReplayBarrierReplayCapability
	ReplayBarrierAmbiguous
	ReplayBarrierNonRetryableStatus
	ReplayBarrierPreferredSkipRetry
	ReplayBarrierClientCancel
)

type ResponsesReplayCommand struct {
	Decision ResponsesAttemptReplayDecision
	Failure  ResponsesChannelFailure
	APIError *types.OpenAIErrorWithStatusCode
	Barrier  ResponsesReplayBlockingBarrier
}

func DecideResponsesAttemptReplay(snapshot ResponsesAttemptSnapshot) ResponsesReplayCommand {
	if snapshot.Downstream == ResponsesAttemptDownstreamCommitted || snapshot.Watermark.Committed {
		return responsesAttemptSurfaceBlocked(snapshot, ReplayBarrierDownstreamCommitted)
	}

	if snapshot.Upstream == ResponsesAttemptUpstreamAmbiguous {
		return ResponsesReplayCommand{
			Decision: ResponsesAttemptDecisionNoRetryAmbiguous,
			Failure:  ChannelFailureAmbiguous,
			APIError: snapshot.Failure,
			Barrier:  ReplayBarrierAmbiguous,
		}
	}

	if snapshot.Upstream == ResponsesAttemptUpstreamAccepted ||
		snapshot.Upstream == ResponsesAttemptUpstreamFailedAfterAccept {
		return responsesAttemptSurfaceBlocked(snapshot, ReplayBarrierProviderAccepted)
	}

	if snapshot.Accounting == ResponsesAttemptAccountingUsageSeen ||
		snapshot.Accounting == ResponsesAttemptAccountingFinalized ||
		snapshot.Accounting == ResponsesAttemptAccountingAcceptanceEvidenceSeen {
		return responsesAttemptSurfaceBlocked(snapshot, ReplayBarrierAccounting)
	}

	if snapshot.PendingCreateCancel && responsesAttemptRollbackableUpstream(snapshot.Upstream) {
		return responsesAttemptRollbackSurfaceBlocked(snapshot, ReplayBarrierClientCancel)
	}

	// Trade-off: non-strict continuation is allowed to retry on another channel
	// for availability. The new provider may reject the previous_response_id as
	// unknown, but strict affinity still preserves the old same-channel contract.
	if snapshot.Continuation.Strict {
		if responsesAttemptRollbackableUpstream(snapshot.Upstream) {
			return responsesAttemptRollbackSurfaceBlocked(snapshot, ReplayBarrierContinuation)
		}
		return responsesAttemptSurfaceBlocked(snapshot, ReplayBarrierContinuation)
	}

	if snapshot.Affinity == ResponsesAttemptAffinityStrict ||
		snapshot.Affinity == ResponsesAttemptAffinityExplicitPin {
		if responsesAttemptRollbackableUpstream(snapshot.Upstream) {
			return responsesAttemptRollbackSurfaceBlocked(snapshot, ReplayBarrierAffinity)
		}
		return responsesAttemptSurfaceBlocked(snapshot, ReplayBarrierAffinity)
	}

	if snapshot.Affinity == ResponsesAttemptAffinityPreferred && snapshot.SkipRetryAfterPreferredFailure {
		if responsesAttemptRollbackableUpstream(snapshot.Upstream) {
			return responsesAttemptRollbackSurfaceBlocked(snapshot, ReplayBarrierPreferredSkipRetry)
		}
		return responsesAttemptSurfaceBlocked(snapshot, ReplayBarrierPreferredSkipRetry)
	}

	if snapshot.Replay != ResponsesAttemptReplayRawCreateFirstTurn ||
		snapshot.Turn != ResponsesAttemptTurnFirst {
		if responsesAttemptRollbackableUpstream(snapshot.Upstream) {
			return responsesAttemptRollbackSurfaceBlocked(snapshot, ReplayBarrierReplayCapability)
		}
		return responsesAttemptSurfaceBlocked(snapshot, ReplayBarrierReplayCapability)
	}

	switch snapshot.Upstream {
	case ResponsesAttemptUpstreamNotAttempted:
		return ResponsesReplayCommand{
			Decision: ResponsesAttemptDecisionRollbackAndRetryNextChannel,
			Failure:  ChannelFailureTransportNotAttempted,
			APIError: snapshot.Failure,
		}
	case ResponsesAttemptUpstreamRejectedBeforeAccept:
		if snapshot.Accounting != ResponsesAttemptAccountingZeroChargeProofAvailable {
			return responsesAttemptSurfaceBlocked(snapshot, ReplayBarrierAccounting)
		}
		if !responsesAttemptRetryableFailure(snapshot.Failure) {
			return responsesAttemptRollbackSurfaceBlocked(snapshot, ReplayBarrierNonRetryableStatus)
		}
		return ResponsesReplayCommand{
			Decision: ResponsesAttemptDecisionRollbackAndRetryNextChannel,
			Failure:  classifyResponsesChannelFailure(snapshot.Failure),
			APIError: snapshot.Failure,
		}
	default:
		return responsesAttemptSurfaceBlocked(snapshot, ReplayBarrierNone)
	}
}

func responsesAttemptRollbackableUpstream(upstream ResponsesAttemptUpstreamDisposition) bool {
	return upstream == ResponsesAttemptUpstreamRejectedBeforeAccept ||
		upstream == ResponsesAttemptUpstreamNotAttempted
}

func responsesAttemptSurfaceBlocked(snapshot ResponsesAttemptSnapshot, barrier ResponsesReplayBlockingBarrier) ResponsesReplayCommand {
	return ResponsesReplayCommand{
		Decision: ResponsesAttemptDecisionSurface,
		Failure:  classifyResponsesChannelFailure(snapshot.Failure),
		APIError: snapshot.Failure,
		Barrier:  barrier,
	}
}

func responsesAttemptRollbackSurfaceBlocked(snapshot ResponsesAttemptSnapshot, barrier ResponsesReplayBlockingBarrier) ResponsesReplayCommand {
	return ResponsesReplayCommand{
		Decision: ResponsesAttemptDecisionRollbackAndSurface,
		Failure:  classifyResponsesChannelFailure(snapshot.Failure),
		APIError: snapshot.Failure,
		Barrier:  barrier,
	}
}

func responsesAttemptRetryableFailure(apiErr *types.OpenAIErrorWithStatusCode) bool {
	if apiErr == nil || apiErr.LocalError {
		return false
	}
	// Status policy is operator-owned, but ResponsesWS still gates replay with
	// upstream, downstream, accounting, affinity, and continuation barriers
	// before this function runs. Including timeout statuses here only takes
	// effect after the attempt is classified as not accepted/committed.
	return config.RetryStatusCodeIsRetryable(apiErr.StatusCode)
}

func classifyResponsesChannelFailure(apiErr *types.OpenAIErrorWithStatusCode) ResponsesChannelFailure {
	if apiErr == nil {
		return ChannelFailureNone
	}
	if common.ProviderErrorIsQuotaExhausted(apiErr.OpenAIError) {
		return ChannelFailureQuotaExhausted
	}
	switch apiErr.StatusCode {
	case http.StatusTooManyRequests:
		return ChannelFailureRateLimited
	case http.StatusUnauthorized, http.StatusForbidden:
		return ChannelFailureAuthRejected
	case http.StatusPaymentRequired:
		return ChannelFailureQuotaExhausted
	}
	if apiErr.StatusCode/100 == 5 {
		return ChannelFailureTransient5xx
	}
	if common.ProviderErrorIsAuthRejected(apiErr.OpenAIError) {
		return ChannelFailureAuthRejected
	}
	if common.ProviderErrorIsRateLimited(apiErr.OpenAIError) {
		return ChannelFailureRateLimited
	}
	return ChannelFailureNone
}

func responsesAttemptStatusCode(apiErr *types.OpenAIErrorWithStatusCode) int {
	if apiErr == nil {
		return 0
	}
	return apiErr.StatusCode
}

func responsesAttemptDecisionLabel(decision ResponsesAttemptReplayDecision) string {
	switch decision {
	case ResponsesAttemptDecisionSurface:
		return "surface"
	case ResponsesAttemptDecisionRollbackAndRetryNextChannel:
		return "rollback_retry_next_channel"
	case ResponsesAttemptDecisionRollbackAndSurface:
		return "rollback_surface"
	case ResponsesAttemptDecisionNoRetryAmbiguous:
		return "no_retry_ambiguous"
	case ResponsesAttemptDecisionClose:
		return "close"
	default:
		return "unknown"
	}
}

func responsesAttemptFailureLabel(failure ResponsesChannelFailure) string {
	switch failure {
	case ChannelFailureNone:
		return "none"
	case ChannelFailureRateLimited:
		return "rate_limited"
	case ChannelFailureQuotaExhausted:
		return "quota_exhausted"
	case ChannelFailureTransient5xx:
		return "transient_5xx"
	case ChannelFailureAuthRejected:
		return "auth_rejected"
	case ChannelFailureTransportNotAttempted:
		return "transport_not_attempted"
	case ChannelFailureAmbiguous:
		return "ambiguous"
	default:
		return "unknown"
	}
}

func responsesAttemptBarrierLabel(barrier ResponsesReplayBlockingBarrier) string {
	switch barrier {
	case ReplayBarrierNone:
		return "none"
	case ReplayBarrierDownstreamCommitted:
		return "downstream_committed"
	case ReplayBarrierProviderAccepted:
		return "provider_accepted"
	case ReplayBarrierAccounting:
		return "accounting"
	case ReplayBarrierContinuation:
		return "continuation"
	case ReplayBarrierAffinity:
		return "affinity"
	case ReplayBarrierReplayCapability:
		return "replay_capability"
	case ReplayBarrierAmbiguous:
		return "ambiguous"
	case ReplayBarrierNonRetryableStatus:
		return "non_retryable_status"
	case ReplayBarrierPreferredSkipRetry:
		return "preferred_skip_retry"
	case ReplayBarrierClientCancel:
		return "client_cancel"
	default:
		return "unknown"
	}
}

func responsesAttemptOriginLabel(origin ResponsesAttemptFailureOrigin) string {
	switch origin {
	case ResponsesAttemptFailureOriginHTTPStatus:
		return "http_status"
	case ResponsesAttemptFailureOriginWSProviderRequestError:
		return "ws_provider_request_error"
	case ResponsesAttemptFailureOriginBridgeOpenProviderError:
		return "bridge_open_provider_error"
	case ResponsesAttemptFailureOriginTransportNotAttempted:
		return "transport_not_attempted"
	case ResponsesAttemptFailureOriginTransportAmbiguous:
		return "transport_ambiguous"
	case ResponsesAttemptFailureOriginNoFirstProviderPayload:
		return "no_first_provider_payload"
	default:
		return "unknown"
	}
}
