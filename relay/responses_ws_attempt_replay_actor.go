package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/responsesws"
	runtimesession "one-api/runtime/session"
	"one-api/types"
)

func responsesWSProviderRequestRejectionFromPayload(payload []byte) (*types.OpenAIErrorWithStatusCode, bool) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil, false
	}
	var object map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil {
		return nil, false
	}
	if jsonStringField(object, "type") != "error" {
		return nil, false
	}
	if responsesWSPayloadResponseID(payload) != "" {
		return nil, false
	}
	if rawResponse, ok := object["response"]; ok && !bytes.Equal(bytes.TrimSpace(rawResponse), []byte("null")) {
		return nil, false
	}
	apiErr := runtimesession.ProviderAPIErrorFromPayload(payload)
	if apiErr == nil {
		return nil, false
	}
	return apiErr, true
}

func (a *ResponsesWSSessionActor) tryHandleProviderRejectedBeforeAccept(event ResponsesWSEventProviderDownstream, payloadPolicy responsesWSProviderPayloadPolicy) bool {
	if a == nil || event.Kind != ProviderDownstreamFrame || event.Frame == nil || event.Frame.Kind() != responsesws.FrameKindText {
		return false
	}
	if payloadPolicy.PayloadOrigin != responsesws.PayloadOriginProvider || event.Usage != nil {
		return false
	}
	attempt := a.currentTurnAttempt()
	if attempt == nil || attempt.ProviderAccepted || attempt.DownstreamCommitted {
		return false
	}
	if responsesWSUsageHasBillableEvidence(attempt.Usage) ||
		responsesWSUsageHasBillableEvidence(attempt.TerminalUsage) ||
		attempt.TerminalEvidence != nil ||
		attempt.QuotaFinalized ||
		attempt.AppliedSettlement != nil {
		return false
	}
	apiErr, ok := responsesWSProviderRequestRejectionFromPayload(event.Frame.Payload())
	if !ok {
		return false
	}
	if a.turns.pending.attempt == attempt && responsesWSTransportSendStatus(attempt.TransportResult) == "" {
		_, overLimit := a.turns.pending.provider.journal.AppendReplayableRequestRejection(event, config.ResponsesWSPendingProviderEventsMaxBytes())
		if overLimit {
			a.failClosed("responses_ws_pending_provider_buffer_full")
		}
		return true
	}
	command := DecideResponsesAttemptReplay(a.responsesAttemptSnapshot(
		attempt,
		ResponsesAttemptUpstreamRejectedBeforeAccept,
		apiErr,
		ResponsesAttemptFailureOriginWSProviderRequestError,
		true,
	))
	payload := event.Frame.Payload()
	missTarget := a.continuationMissTargetForAttempt(attempt, responsesWSProviderPayloadContinuationMiss(payload))
	return a.executeResponsesAttemptReplayCommandWithContinuationMiss(
		attempt,
		command,
		payload,
		apiErr,
		ResponsesAttemptFailureOriginWSProviderRequestError,
		false,
		missTarget,
	)
}

func (a *ResponsesWSSessionActor) tryReplayPendingProviderRejectionBeforeCommit(attempt *ResponsesWSTurnAttempt) bool {
	if a == nil || attempt == nil || a.turns.pending.attempt != attempt {
		return false
	}
	event, apiErr, ok := a.pendingReplayableProviderRejection()
	if !ok {
		return false
	}
	command := DecideResponsesAttemptReplay(a.responsesAttemptSnapshot(
		attempt,
		ResponsesAttemptUpstreamRejectedBeforeAccept,
		apiErr,
		ResponsesAttemptFailureOriginWSProviderRequestError,
		true,
	))
	payload := responsesWSProviderDownstreamPayload(event)
	missTarget := a.continuationMissTargetForAttempt(attempt, responsesWSProviderPayloadContinuationMiss(payload))
	return a.executeResponsesAttemptReplayCommandWithContinuationMiss(
		attempt,
		command,
		payload,
		apiErr,
		ResponsesAttemptFailureOriginWSProviderRequestError,
		false,
		missTarget,
	)
}

func (a *ResponsesWSSessionActor) pendingReplayableProviderRejection() (ResponsesWSEventProviderDownstream, *types.OpenAIErrorWithStatusCode, bool) {
	if a == nil || a.turns.pending.attempt == nil {
		return ResponsesWSEventProviderDownstream{}, nil, false
	}
	replay := a.turns.pending.provider.journal.Replay()
	if len(replay) != 1 || replay[0].Downstream == nil || !replay[0].Observation.IsZero() || replay[0].Failure != nil {
		return ResponsesWSEventProviderDownstream{}, nil, false
	}
	event := *replay[0].Downstream
	if event.Usage != nil || event.Kind != ProviderDownstreamFrame || event.Frame == nil || event.Frame.Kind() != responsesws.FrameKindText {
		return ResponsesWSEventProviderDownstream{}, nil, false
	}
	apiErr, ok := responsesWSProviderRequestRejectionFromPayload(event.Frame.Payload())
	if !ok {
		return ResponsesWSEventProviderDownstream{}, nil, false
	}
	return event, apiErr, true
}

func (a *ResponsesWSSessionActor) responsesAttemptSnapshot(attempt *ResponsesWSTurnAttempt, upstream ResponsesAttemptUpstreamDisposition, failure *types.OpenAIErrorWithStatusCode, origin ResponsesAttemptFailureOrigin, zeroChargeProof bool) ResponsesAttemptSnapshot {
	if attempt == nil {
		return ResponsesAttemptSnapshot{Upstream: upstream, Failure: failure, Origin: origin}
	}
	downstream := ResponsesAttemptDownstreamUncommitted
	if attempt.DownstreamCommitted {
		downstream = ResponsesAttemptDownstreamCommitted
	}
	return ResponsesAttemptSnapshot{
		AttemptID:                      strings.TrimSpace(attempt.AttemptID),
		OpeningID:                      strings.TrimSpace(attempt.OpeningID),
		Upstream:                       upstream,
		Downstream:                     downstream,
		Accounting:                     a.responsesAttemptAccounting(attempt, zeroChargeProof),
		Failure:                        failure,
		Origin:                         origin,
		Replay:                         a.responsesAttemptReplayCapability(attempt),
		Affinity:                       a.responsesAttemptAffinity(attempt),
		Turn:                           responsesAttemptTurnKind(attempt),
		SkipRetryAfterPreferredFailure: shouldSkipRetryAfterAffinityFailure(attempt.Context()),
		PendingCreateCancel:            a.hasPendingCreateCancel(attempt.AttemptID),
		Continuation: ResponsesContinuationAnchor{
			PreviousResponseID: strings.TrimSpace(attempt.AttemptedPreviousResponseID),
			Strict:             currentChannelAffinityStrict(attempt.Context()),
		},
		Watermark: ResponsesDownstreamWatermark{
			AttemptID: strings.TrimSpace(attempt.AttemptID),
			Seq:       attempt.DownstreamCommitSeq,
			Committed: attempt.DownstreamCommitted,
			Kind:      attempt.DownstreamCommitKind,
		},
	}
}

func (a *ResponsesWSSessionActor) responsesAttemptAccounting(attempt *ResponsesWSTurnAttempt, zeroChargeProof bool) ResponsesAttemptAccountingDisposition {
	if attempt == nil {
		return ResponsesAttemptAccountingNoEvidence
	}
	if attempt.QuotaFinalized || attempt.AppliedSettlement != nil {
		return ResponsesAttemptAccountingFinalized
	}
	if responsesWSUsageHasBillableEvidence(attempt.Usage) || responsesWSUsageHasBillableEvidence(attempt.TerminalUsage) {
		return ResponsesAttemptAccountingUsageSeen
	}
	if a != nil {
		if a.turns.pending.attempt == attempt && a.turns.pending.provider.journal.Project().HasActivity() {
			return ResponsesAttemptAccountingAcceptanceEvidenceSeen
		}
		if a.turns.active.attempt == attempt && a.turns.active.evidence.HasActivity() {
			return ResponsesAttemptAccountingAcceptanceEvidenceSeen
		}
	}
	if attempt.ProviderAccepted || attempt.SeenProviderResponseID != "" || attempt.TerminalEvidence != nil {
		return ResponsesAttemptAccountingAcceptanceEvidenceSeen
	}
	if zeroChargeProof {
		return ResponsesAttemptAccountingZeroChargeProofAvailable
	}
	return ResponsesAttemptAccountingNoEvidence
}

func (a *ResponsesWSSessionActor) responsesAttemptReplayCapability(attempt *ResponsesWSTurnAttempt) ResponsesAttemptReplayCapability {
	if a == nil || attempt == nil {
		return ResponsesAttemptReplayNone
	}
	if strings.TrimSpace(attempt.OpeningID) != "" &&
		a.turns.opening.firstFrame != nil &&
		strings.TrimSpace(a.turns.opening.openingID) == strings.TrimSpace(attempt.OpeningID) {
		return ResponsesAttemptReplayRawCreateFirstTurn
	}
	return ResponsesAttemptReplayNone
}

func (a *ResponsesWSSessionActor) responsesAttemptAffinity(attempt *ResponsesWSTurnAttempt) ResponsesAttemptAffinityMode {
	if attempt == nil {
		return ResponsesAttemptAffinityFree
	}
	ctx := attempt.Context()
	if attempt.Candidate != nil && attempt.Candidate.ExplicitPinID > 0 || explicitChannelPinID(ctx) > 0 {
		return ResponsesAttemptAffinityExplicitPin
	}
	if currentChannelAffinityStrict(ctx) {
		return ResponsesAttemptAffinityStrict
	}
	if attempt.Candidate != nil && attempt.Candidate.State != nil && attempt.Candidate.State.Hit {
		return ResponsesAttemptAffinityPreferred
	}
	if currentPreferredChannelID(ctx) > 0 {
		return ResponsesAttemptAffinityPreferred
	}
	return ResponsesAttemptAffinityFree
}

func responsesAttemptTurnKind(attempt *ResponsesWSTurnAttempt) ResponsesAttemptTurnKind {
	if attempt == nil {
		return ResponsesAttemptTurnContinuation
	}
	if strings.TrimSpace(attempt.OpeningID) != "" && strings.TrimSpace(attempt.AttemptedPreviousResponseID) == "" {
		return ResponsesAttemptTurnFirst
	}
	return ResponsesAttemptTurnContinuation
}

func (a *ResponsesWSSessionActor) executeResponsesAttemptReplayCommand(attempt *ResponsesWSTurnAttempt, command ResponsesReplayCommand, payload []byte, apiErr *types.OpenAIErrorWithStatusCode, origin ResponsesAttemptFailureOrigin, closeAfterSurface bool) bool {
	return a.executeResponsesAttemptReplayCommandWithContinuationMiss(attempt, command, payload, apiErr, origin, closeAfterSurface, nil)
}

type responsesWSContinuationMissTarget struct {
	turn                        *ResponsesTurnAffinity
	ownerChannelID              int
	attemptedPreviousResponseID string
}

func (a *ResponsesWSSessionActor) continuationMissTargetForAttempt(attempt *ResponsesWSTurnAttempt, providerReportedMiss bool) *responsesWSContinuationMissTarget {
	if a == nil || attempt == nil || !providerReportedMiss {
		return nil
	}
	attemptedPreviousResponseID := strings.TrimSpace(attempt.AttemptedPreviousResponseID)
	if attemptedPreviousResponseID == "" && attempt.Candidate != nil {
		attemptedPreviousResponseID = strings.TrimSpace(attempt.Candidate.PreviousResponseID)
	}
	if attemptedPreviousResponseID == "" && a.turns.active.attempt == attempt && a.turns.active.affinity != nil {
		attemptedPreviousResponseID = strings.TrimSpace(a.turns.active.affinity.PreviousResponseID)
	}
	if attemptedPreviousResponseID == "" {
		return nil
	}
	target := &responsesWSContinuationMissTarget{
		attemptedPreviousResponseID: attemptedPreviousResponseID,
	}
	switch {
	case a.turns.pending.attempt == attempt:
		target.turn = attempt.Candidate
		target.ownerChannelID = attempt.SelectedChannelID
	case a.turns.active.attempt == attempt:
		target.turn = a.turns.active.affinity
		target.ownerChannelID = a.turns.active.channelID
	default:
		target.turn = attempt.Candidate
		target.ownerChannelID = attempt.SelectedChannelID
	}
	return target
}

func responsesWSProviderPayloadContinuationMiss(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	return responsesws.ClassifyResponsesWSEvent(payload).ContinuationMiss
}

func (a *ResponsesWSSessionActor) applyContinuationMissTargetAfterSettlement(attempt *ResponsesWSTurnAttempt, target *responsesWSContinuationMissTarget) {
	if a == nil || attempt == nil || target == nil {
		return
	}
	if !responsesWSReplayAttemptSettled(attempt) {
		return
	}
	a.applyContinuationMissSideEffects(target.turn, target.ownerChannelID, target.attemptedPreviousResponseID)
}

func responsesWSReplayAttemptSettled(attempt *ResponsesWSTurnAttempt) bool {
	return attempt != nil && (attempt.RolledBack || attempt.QuotaFinalized || attempt.AppliedSettlement != nil)
}

func (a *ResponsesWSSessionActor) executeResponsesAttemptReplayCommandWithContinuationMiss(
	attempt *ResponsesWSTurnAttempt,
	command ResponsesReplayCommand,
	payload []byte,
	apiErr *types.OpenAIErrorWithStatusCode,
	origin ResponsesAttemptFailureOrigin,
	closeAfterSurface bool,
	missTarget *responsesWSContinuationMissTarget,
) bool {
	if a == nil || attempt == nil {
		return false
	}
	attempt.ReplayFailure = apiErr
	attempt.ReplayFailureOrigin = origin
	a.observeResponsesAttemptReplayDecision(attempt, command, origin)
	switch command.Decision {
	case ResponsesAttemptDecisionRollbackAndRetryNextChannel:
		if !a.applyReplayRollbackOrClose(attempt, origin, "attempt_replay_retry") {
			return true
		}
		a.applyContinuationMissTargetAfterSettlement(attempt, missTarget)
		a.processReplayProviderAPIError(attempt, apiErr, origin)
		replayed := a.retryFirstTurnAfterReplayableFailure(attempt, command)
		if replayed {
			a.observeResponsesAttemptReplayExecuted(command, origin)
		}
		return replayed
	case ResponsesAttemptDecisionRollbackAndSurface:
		if !a.applyReplayRollbackOrClose(attempt, origin, "attempt_replay_surface") {
			return true
		}
		a.applyContinuationMissTargetAfterSettlement(attempt, missTarget)
		a.processReplayProviderAPIError(attempt, apiErr, origin)
		a.emitReplaySurface(attempt, payload, "attempt_replay_surface")
		a.clearReplayAttemptState(attempt, "attempt_replay_surface")
		if closeAfterSurface {
			a.close("provider_rejected_before_accept")
		}
		return true
	case ResponsesAttemptDecisionNoRetryAmbiguous:
		if !a.applyReplayFloorOrClose(attempt, "attempt_replay_ambiguous") {
			return true
		}
		a.applyContinuationMissTargetAfterSettlement(attempt, missTarget)
		a.processReplayProviderAPIError(attempt, apiErr, origin)
		a.emitReplaySurface(attempt, payload, "attempt_replay_ambiguous")
		a.clearReplayAttemptState(attempt, "attempt_replay_ambiguous")
		if closeAfterSurface {
			a.close("provider_rejected_before_accept_ambiguous")
		}
		return true
	case ResponsesAttemptDecisionSurface:
		if responsesAttemptSurfaceBarrierRequiresSettlement(command.Barrier) && !attempt.RolledBack && !attempt.QuotaFinalized {
			if !a.applyReplayFloorOrClose(attempt, "attempt_replay_blocked") {
				return true
			}
		}
		a.applyContinuationMissTargetAfterSettlement(attempt, missTarget)
		a.processReplayProviderAPIError(attempt, apiErr, origin)
		a.emitReplaySurface(attempt, payload, "attempt_replay_blocked")
		a.clearReplayAttemptState(attempt, "attempt_replay_blocked")
		if closeAfterSurface {
			a.close("provider_rejected_before_accept")
		}
		return true
	case ResponsesAttemptDecisionClose:
		a.close("attempt_replay_close")
		return true
	default:
		return false
	}
}

func responsesAttemptSurfaceBarrierRequiresSettlement(barrier ResponsesReplayBlockingBarrier) bool {
	switch barrier {
	case ReplayBarrierAccounting, ReplayBarrierProviderAccepted, ReplayBarrierDownstreamCommitted:
		return true
	default:
		return false
	}
}

func (a *ResponsesWSSessionActor) observeResponsesAttemptReplayDecision(attempt *ResponsesWSTurnAttempt, command ResponsesReplayCommand, origin ResponsesAttemptFailureOrigin) {
	if a == nil {
		return
	}
	decision := responsesAttemptDecisionLabel(command.Decision)
	originLabel := responsesAttemptOriginLabel(origin)
	barrier := responsesAttemptBarrierLabel(command.Barrier)
	failure := responsesAttemptFailureLabel(command.Failure)
	status := responsesAttemptStatusCode(command.APIError)
	recordResponsesWSAttemptReplayDecision(decision, originLabel, status, barrier, failure)
	if command.Barrier != ReplayBarrierNone {
		recordResponsesWSAttemptReplayBlocked(barrier, originLabel, status)
	}
	attemptID := ""
	openingID := ""
	channelID := 0
	if attempt != nil {
		attemptID = attempt.AttemptID
		openingID = attempt.OpeningID
		channelID = attempt.SelectedChannelID
	}
	code := ""
	if command.APIError != nil {
		code = common.OpenAIErrorCodeText(command.APIError.Code)
	}
	a.logDebugf(
		"responses websocket attempt replay decision: attempt_id=%s opening_id=%s channel_id=%d decision=%s origin=%s status=%d code=%s barrier=%s failure=%s",
		responsesWSSafeDiagnosticValue(attemptID),
		responsesWSSafeDiagnosticValue(openingID),
		channelID,
		decision,
		originLabel,
		status,
		responsesWSSafeDiagnosticValue(code),
		barrier,
		failure,
	)
}

func (a *ResponsesWSSessionActor) observeResponsesAttemptReplayExecuted(command ResponsesReplayCommand, origin ResponsesAttemptFailureOrigin) {
	recordResponsesWSAttemptReplayExecuted(
		responsesAttemptOriginLabel(origin),
		responsesAttemptStatusCode(command.APIError),
		responsesAttemptFailureLabel(command.Failure),
	)
}

func (a *ResponsesWSSessionActor) tryExecuteBridgeOpenProviderReplay(event ResponsesWSEventBridgeOpenProviderError, attempt *ResponsesWSTurnAttempt, continuationMiss bool, closeAfterSurface bool) bool {
	if a == nil || attempt == nil {
		return false
	}
	if a.turns.active.attempt == attempt {
		return false
	}
	apiErr := event.ProviderAPIError
	if apiErr == nil {
		apiErr = runtimesession.ProviderAPIErrorFromPayload(event.Payload)
	}
	if apiErr == nil {
		apiErr = responsesReplayLocalError("upstream rejected response before stream", "provider_rejected_before_stream", http.StatusBadGateway)
	}
	payload := event.Payload
	if len(payload) == 0 {
		payload = responsesWSErrorFromReplayAPIError(apiErr)
	}
	command := DecideResponsesAttemptReplay(a.responsesAttemptSnapshot(
		attempt,
		ResponsesAttemptUpstreamRejectedBeforeAccept,
		apiErr,
		ResponsesAttemptFailureOriginBridgeOpenProviderError,
		true,
	))
	if command.Decision == ResponsesAttemptDecisionSurface &&
		(command.Barrier == ReplayBarrierAccounting ||
			command.Barrier == ReplayBarrierProviderAccepted ||
			command.Barrier == ReplayBarrierDownstreamCommitted) {
		return false
	}
	missTarget := a.continuationMissTargetForAttempt(attempt, continuationMiss)
	return a.executeResponsesAttemptReplayCommandWithContinuationMiss(
		attempt,
		command,
		payload,
		apiErr,
		ResponsesAttemptFailureOriginBridgeOpenProviderError,
		closeAfterSurface,
		missTarget,
	)
}

func (a *ResponsesWSSessionActor) applyReplayRollbackOrClose(attempt *ResponsesWSTurnAttempt, origin ResponsesAttemptFailureOrigin, reason string) bool {
	var proof ResponsesWSZeroChargeProof
	switch origin {
	case ResponsesAttemptFailureOriginWSProviderRequestError:
		proof = responsesWSProviderRejectedBeforeAcceptProof(reason)
	case ResponsesAttemptFailureOriginBridgeOpenProviderError:
		proof = responsesWSZeroChargeProof(ResponsesWSZeroChargeProofProviderRejectedBeforeStream, reason)
	case ResponsesAttemptFailureOriginTransportNotAttempted:
		proof = responsesWSZeroChargeProof(ResponsesWSZeroChargeProofTransportNotAttempted, reason)
	default:
		a.logErrorf("responses websocket replay rollback requires typed origin: attempt_id=%s origin=%s reason=%s",
			responsesWSSafeDiagnosticValue(attempt.AttemptID),
			responsesAttemptOriginLabel(origin),
			responsesWSSafeDiagnosticValue(reason),
		)
		recordResponsesWSSettlementConflict("attempt_replay_state_conflict")
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_settlement_failed", responsesWSStaticErrorMessage("quota_settlement_failed")))
		a.close("attempt_replay_state_conflict")
		return false
	}
	input := a.buildReplaySettlementInput(attempt, reason, proof)
	decision, _, err := a.applyReplaySettlement(attempt, input)
	if err != nil {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_rollback_failed", responsesWSStaticErrorMessage("quota_rollback_failed")))
		a.close("quota_rollback_failed")
		return false
	}
	if decision.Action != ResponsesWSSettlementRollbackReserve {
		a.observeSettlementConflict("replay_rollback_not_applied", decision)
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_settlement_failed", responsesWSStaticErrorMessage("quota_settlement_failed")))
		a.close("quota_settlement_failed")
		return false
	}
	if responsesWSSettlementHasFlag(decision, ResponsesWSSettlementFlagContradictoryInput) {
		a.observeSettlementConflict(string(ResponsesWSSettlementFlagContradictoryInput), decision)
	}
	return true
}

func (a *ResponsesWSSessionActor) applyReplayFloorOrClose(attempt *ResponsesWSTurnAttempt, reason string) bool {
	input := a.buildReplaySettlementInput(attempt, reason, ResponsesWSZeroChargeProof{})
	_, _, err := a.applyReplaySettlement(attempt, input)
	if err != nil {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusInternalServerError, "quota_settlement_failed", responsesWSStaticErrorMessage("quota_settlement_failed")))
		a.close("quota_settlement_failed")
		return false
	}
	return true
}

func (a *ResponsesWSSessionActor) buildReplaySettlementInput(attempt *ResponsesWSTurnAttempt, reason string, proof ResponsesWSZeroChargeProof) ResponsesWSSettlementInput {
	if a == nil || attempt == nil {
		return ResponsesWSSettlementInput{}
	}
	if a.turns.pending.attempt == attempt {
		return a.buildPendingSettlementInput(reason, proof)
	}
	if a.turns.active.attempt == attempt {
		return a.buildActiveSettlementInput(reason, proof)
	}
	return a.buildSettlementInputFromAttempt(attempt, responsesws.ProviderSettlementLogProjection{}, reason, proof)
}

func (a *ResponsesWSSessionActor) applyReplaySettlement(attempt *ResponsesWSTurnAttempt, input ResponsesWSSettlementInput) (ResponsesWSSettlementDecision, ResponsesWSAppliedSettlement, error) {
	if a == nil || attempt == nil {
		return ResponsesWSSettlementDecision{}, ResponsesWSAppliedSettlement{}, errors.New("responses websocket replay attempt is required")
	}
	if a.turns.pending.attempt == attempt {
		return a.applyPendingSettlement(input)
	}
	if a.turns.active.attempt == attempt {
		return a.applyActiveSettlement(input)
	}
	decision := decideResponsesWSSettlement(input)
	applied, err := attempt.ApplyResponsesWSSettlementDecision(a.Context(), decision)
	if err == nil {
		a.emitSettlementTrace(input, decision, applied)
	}
	return decision, applied, err
}

func (a *ResponsesWSSessionActor) processReplayProviderAPIError(attempt *ResponsesWSTurnAttempt, apiErr *types.OpenAIErrorWithStatusCode, origin ResponsesAttemptFailureOrigin) {
	if a == nil || attempt == nil || apiErr == nil {
		return
	}
	source := responsesAttemptFailureOriginSource(origin)
	if source == "" {
		source = "responses_ws_attempt_replay"
	}
	if !a.markProviderAPIErrorSeen(apiErr, source) {
		return
	}
	processProviderAPIError(a.Context(), a.providerPayloadChannel(attempt.SelectedChannelID), apiErr, source)
}

func responsesAttemptFailureOriginSource(origin ResponsesAttemptFailureOrigin) string {
	switch origin {
	case ResponsesAttemptFailureOriginWSProviderRequestError:
		return "responses_ws_provider_request_error"
	case ResponsesAttemptFailureOriginBridgeOpenProviderError:
		return "responses_ws_bridge_open_provider_error"
	case ResponsesAttemptFailureOriginTransportNotAttempted:
		return "responses_ws_transport_not_attempted"
	default:
		return ""
	}
}

func (a *ResponsesWSSessionActor) emitReplaySurface(attempt *ResponsesWSTurnAttempt, payload []byte, reason string) {
	if a == nil || len(payload) == 0 {
		return
	}
	a.writeProxyLocalForAttempt(attempt, payload, reason)
}

func (a *ResponsesWSSessionActor) clearReplayAttemptState(attempt *ResponsesWSTurnAttempt, reason string) {
	if a == nil || attempt == nil {
		return
	}
	switch {
	case a.turns.pending.attempt == attempt:
		a.clearPendingTurn(reason)
	case a.turns.active.attempt == attempt:
		if err := a.finishActiveTurn(reason, attempt.AttemptID); err != nil {
			a.logErrorf("responses websocket active finish transition failed: %v", err)
		}
	}
	if !a.closing.closed.Load() {
		a.state = responsesWSStateIdle
	}
}

func (a *ResponsesWSSessionActor) retryFirstTurnAfterReplayableFailure(previous *ResponsesWSTurnAttempt, command ResponsesReplayCommand) bool {
	if a == nil || previous == nil || previous.OpeningID == "" || a.turns.opening.firstFrame == nil || previous.Admission == nil {
		return false
	}
	if command.Decision != ResponsesAttemptDecisionRollbackAndRetryNextChannel {
		return false
	}
	if !a.canRetryFirstTurnAfterReplayableFailure(previous) {
		a.logErrorf("responses websocket replay state conflict: attempt_id=%s opening_id=%s pending_attempt=%s active_attempt=%s pending_phase=%d state=%d",
			responsesWSSafeDiagnosticValue(previous.AttemptID),
			responsesWSSafeDiagnosticValue(previous.OpeningID),
			responsesWSTestableAttemptID(a.turns.pending.attempt),
			responsesWSTestableAttemptID(a.turns.active.attempt),
			a.turns.pending.phase,
			a.state,
		)
		recordResponsesWSSettlementConflict("attempt_replay_state_conflict")
		a.close("attempt_replay_state_conflict")
		return false
	}
	failedChannelID := previous.SelectedChannelID
	if failedChannelID > 0 {
		ctx := a.Context()
		(&relayBase{c: ctx}).skipChannelID(failedChannelID)
		a.RefreshContext(ctx)
	}
	if a.upstream.session != nil && a.io.bridge != nil {
		a.io.bridge.AbortSession(a.upstream.session, "attempt_replay_retry")
	}
	a.clearReplayAttemptState(previous, "attempt_replay_retry")
	a.upstream.session = nil
	a.upstream.sessionGeneration = ""
	a.upstream.channelID = 0
	a.upstream.recvArmed = false
	a.clearPendingProviderState("attempt_replay_retry")
	a.turns.opening.admission = previous.Admission
	a.mutateSnapshot(clearResponsesWSSelectedChannelSnapshot)
	if a.isClientGone() {
		a.close("client_closed_before_first_turn_retry")
		return true
	}
	a.turns.pending.phase = responsesWSPendingTurnOpening
	a.state = responsesWSStateOpening
	a.startFirstTurnOpenWorker(a.turns.opening.openingID, a.turns.opening.firstFrame)
	return true
}

func (a *ResponsesWSSessionActor) canRetryFirstTurnAfterReplayableFailure(previous *ResponsesWSTurnAttempt) bool {
	if a == nil || previous == nil {
		return false
	}
	if a.turns.active.attempt == previous {
		return a.turns.pending.attempt == nil &&
			a.turns.pending.phase == responsesWSPendingTurnNone &&
			(a.state == responsesWSStateInFlight || a.state == responsesWSStateClosed || a.state == responsesWSStateIdle)
	}
	if a.turns.pending.attempt == previous {
		if a.turns.active.attempt != nil {
			return false
		}
		switch a.turns.pending.phase {
		case responsesWSPendingTurnSend, responsesWSPendingTurnPrepare:
			return a.state == responsesWSStatePendingSend || a.state == responsesWSStatePendingPrepare
		default:
			return false
		}
	}
	return false
}

func responsesWSTestableAttemptID(attempt *ResponsesWSTurnAttempt) string {
	if attempt == nil {
		return ""
	}
	return responsesWSSafeDiagnosticValue(attempt.AttemptID)
}

func responsesHTTPAttemptShouldRetry(relay *relayResponses, apiErr *types.OpenAIErrorWithStatusCode, channelType int) bool {
	if relay == nil || relay.c == nil || apiErr == nil {
		return false
	}
	if apiErr.LocalError {
		return false
	}
	downstream := ResponsesAttemptDownstreamUncommitted
	if relay.c.Writer != nil && relay.c.Writer.Written() {
		downstream = ResponsesAttemptDownstreamCommitted
	}
	affinity := ResponsesAttemptAffinityFree
	if explicitChannelPinID(relay.c) > 0 {
		affinity = ResponsesAttemptAffinityExplicitPin
	} else if currentChannelAffinityStrict(relay.c) {
		affinity = ResponsesAttemptAffinityStrict
	} else if currentPreferredChannelID(relay.c) > 0 {
		affinity = ResponsesAttemptAffinityPreferred
	}
	previousResponseID := strings.TrimSpace(relay.responsesRequest.PreviousResponseID)
	turn := ResponsesAttemptTurnFirst
	if previousResponseID != "" {
		turn = ResponsesAttemptTurnContinuation
	}
	command := DecideResponsesAttemptReplay(ResponsesAttemptSnapshot{
		Upstream:                       ResponsesAttemptUpstreamRejectedBeforeAccept,
		Downstream:                     downstream,
		Accounting:                     ResponsesAttemptAccountingZeroChargeProofAvailable,
		Failure:                        apiErr,
		Origin:                         ResponsesAttemptFailureOriginHTTPStatus,
		Replay:                         ResponsesAttemptReplayRawCreateFirstTurn,
		Affinity:                       affinity,
		Turn:                           turn,
		SkipRetryAfterPreferredFailure: shouldSkipRetryAfterAffinityFailure(relay.c),
		Continuation: ResponsesContinuationAnchor{
			PreviousResponseID: previousResponseID,
			Strict:             currentChannelAffinityStrict(relay.c),
		},
	})
	if command.Decision == ResponsesAttemptDecisionRollbackAndRetryNextChannel {
		return true
	}
	if apiErr.StatusCode == http.StatusBadRequest &&
		command.Decision == ResponsesAttemptDecisionRollbackAndSurface &&
		command.Barrier == ReplayBarrierNonRetryableStatus {
		return shouldRetryBadRequest(channelType, apiErr)
	}
	return false
}

func responsesWSErrorFromReplayAPIError(apiErr *types.OpenAIErrorWithStatusCode) []byte {
	if apiErr == nil {
		return responsesWSErrorPayload(http.StatusBadGateway, "upstream_error", responsesWSStaticErrorMessage("upstream_error"))
	}
	if payload := responsesWSErrorFromOpenAI(apiErr); len(payload) > 0 {
		return payload
	}
	return responsesWSErrorPayload(http.StatusBadGateway, "upstream_error", responsesWSStaticErrorMessage("upstream_error"))
}

func responsesReplayLocalError(message, code string, status int) *types.OpenAIErrorWithStatusCode {
	return common.StringErrorWrapperLocal(message, code, status)
}
