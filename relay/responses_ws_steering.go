package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"one-api/common"
	commonresponses "one-api/common/responses"
	"one-api/common/responsesws"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

const (
	responsesWSSteerMaxIDBytes     = 1024
	responsesWSSteerMaxSubmissions = 64
	responsesWSHeldParentLimit     = 64
	responsesWSHeldParentMaxBytes  = 4 << 20
)

// 仅在自动续接预扣尚未绑定时观察回执。父计费对象可先完成并释放。
type responsesWSSteerState struct {
	parentID        string
	next            *ResponsesWSTurnAttempt
	awaiting        int
	accepted        map[string]bool
	waiting         bool
	parentTerminal  bool
	parentCompleted bool
	lastSequence    int64
	hasSequence     bool
}

// 每条发送独立消费结果，生命周期不依赖计费候选；仅由 actor 修改。
type responsesWSSendCompletion struct{ consumed bool }

// 临时资源授权事实，不保存输入或已结束的计费对象。
type responsesWSParentProof struct {
	userID, tokenID, channelID int
	generation                 string
}

func (a *ResponsesWSSessionActor) holdSteeringParent(parent *ResponsesWSTurnAttempt) bool {
	if parent.RequireStoredOwner {
		return true
	}
	id := parent.SeenProviderResponseID
	if _, ok := a.heldSteeringParents[id]; ok {
		return true
	}
	if len(a.heldSteeringParents) >= responsesWSHeldParentLimit || len(id) > responsesWSHeldParentMaxBytes-a.heldSteeringParentBytes {
		return false
	}
	ctx := a.Context()
	if ctx == nil || id == "" || !parent.StoredOwnerPersisted {
		return false
	}
	if a.heldSteeringParents == nil {
		a.heldSteeringParents = make(map[string]responsesWSParentProof)
	}
	a.heldSteeringParents[id] = responsesWSParentProof{ctx.GetInt("id"), ctx.GetInt("token_id"), a.upstream.channelID, a.upstream.sessionGeneration}
	a.heldSteeringParentBytes += len(id)
	return true
}

func (a *ResponsesWSSessionActor) hasHeldSteeringParent(c *gin.Context, id string) bool {
	proof, ok := a.heldSteeringParents[id]
	return ok && c != nil && proof.userID == c.GetInt("id") && proof.tokenID == c.GetInt("token_id") && proof.channelID == a.upstream.channelID && proof.generation == a.upstream.sessionGeneration
}

func (a *ResponsesWSSessionActor) releaseSteeringParent(id string) {
	if _, ok := a.heldSteeringParents[id]; ok {
		delete(a.heldSteeringParents, id)
		a.heldSteeringParentBytes -= len(id)
	}
}

func (a *ResponsesWSSessionActor) handleClientSteer(event ResponsesWSEventClientFrame) {
	if !a.allowNewWork() {
		return
	}
	parent := a.turns.active.attempt
	if parent == nil || parent.TerminalObserved || parent.RequestFrame == nil || a.upstream.session == nil {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "response_not_active", "response.steer requires an active response"))
		return
	}
	envelope, err := responsesws.ParseClientEventEnvelope(event.Frame.Payload())
	if err != nil {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "invalid_event", "invalid response.steer envelope"))
		return
	}
	var target string
	if json.Unmarshal(envelope.Object["previous_response_id"], &target) != nil || strings.TrimSpace(target) == "" || target != parent.SeenProviderResponseID {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "response_not_found", "response was not found on this connection"))
		return
	}
	if field, found := commonresponses.FindAccountScopedResourceReferenceJSON(envelope.Object["input"]); found {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "unsupported_resource_reference", "steering input requires an account-scoped resource owner: "+field))
		return
	}
	s := &a.steering
	if s.next != nil && s.parentID != target {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "responses_ws_steer_pending", "previous steering is still pending"))
		return
	}
	if s.awaiting+len(s.accepted) >= responsesWSSteerMaxSubmissions {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusTooManyRequests, "responses_ws_steer_queue_full", "too much steering input is pending"))
		return
	}
	if s.next == nil {
		// 只复用父设置的准入事实，不构造或发送 response.create，也不解释 input 联合类型。
		held := a.hasHeldSteeringParent(a.Context(), target)
		if !a.holdSteeringParent(parent) {
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusTooManyRequests, "responses_ws_parent_capacity", "too many continuation resources are retained"))
			return
		}
		request := parent.RequestFrame.Projection
		request.PreviousResponseID = target
		request.Input = nil // 不保留或估算上游自行组合的后继输入；沿用小额 Try。
		next := a.prepareResponsesWork(parent.RequestFrame, request, event.ReceivedAt, false)
		if next == nil {
			if !held {
				a.releaseSteeringParent(target)
			}
			return
		}
		next.initializeIdentity()
		next.TransportAttemptID = parent.transportAttemptID()
		*s = responsesWSSteerState{parentID: target, next: next, lastSequence: a.turns.active.lastProviderSequence, hasSequence: a.turns.active.hasLastProviderSequence}
		if apiErr := a.reserveAndClaimResponsesWork(next); apiErr != nil {
			a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
			if a.releaseSteeringReservation() && !held {
				a.releaseSteeringParent(target)
			}
			return
		}
	}
	// 发布命令前登记聚合义务。只有原 command 的一次结果能消解未发送义务。
	s.awaiting++
	if !a.SendProviderAuxiliaryFrame(parent.transportAttemptID(), a.upstream.channelID, a.upstream.session, event.Frame, ResponsesWSSendPurposeResponseSteer) {
		s.awaiting--
		a.resolveSteeringReservation()
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusServiceUnavailable, "responses_ws_send_queue_full", "responses websocket send queue is full"))
		a.close("responses_ws_steer_send_failed")
	}
}

func (a *ResponsesWSSessionActor) releaseSteeringReservation() bool {
	if a.steering.next == nil {
		return true
	}
	next := a.steering.next
	ctx, cancel := boundedResponsesLifecycleContext(context.WithoutCancel(a.logContext()))
	_, err := next.ApplyResponsesWSSettlementDecisionWithContext(ctx, next.Context(), projectResponsesWSSharedDecision(next))
	cancel()
	if err != nil {
		a.logErrorf("responses websocket steering settlement failed: %v", err)
		a.close("quota_settlement_failed")
		return false
	}
	a.steering = responsesWSSteerState{}
	a.refreshWorkWatchdog()
	return true
}

func (a *ResponsesWSSessionActor) resolveSteeringReservation() {
	s := &a.steering
	if s.next == nil || s.awaiting != 0 {
		return
	}
	allFailed := len(s.accepted) == 0
	if !allFailed && !(s.waiting && s.parentCompleted) {
		return
	}
	parentID := s.parentID
	if !a.releaseSteeringReservation() {
		return
	}
	if allFailed {
		a.releaseSteeringParent(parentID)
	}
	a.startQueuedResponseCreates()
}

// 交付只依赖已验证的连接；缺失、过期和未来回执不参与账务观察。
func (a *ResponsesWSSessionActor) handleProviderAuxiliaryControl(event ResponsesWSEventProviderDownstream) bool {
	if event.Frame == nil || event.Frame.Kind() != responsesws.FrameKindText || event.DetailOrigin != responsesws.RecvDetailOriginProviderFrame {
		return false
	}
	envelope, err := responsesws.ParseProviderEventEnvelope(event.Frame.Payload())
	if err != nil || !responsesws.IsAuxiliaryControlEvent(envelope.Type) {
		return false
	}
	stop := responsesWSWorkflowStop(event)
	if stop {
		a.stopNewWork()
	}
	if err := a.emitProviderFrameForAttempt(nil, responsesws.NewTextFrame(sanitizeProviderJSONPayload(event.Frame.Payload())), "provider_auxiliary_control"); err != nil {
		a.close("client_write_failed")
		return true
	}
	if stop {
		a.close("provider_workflow_stopped")
		return true
	}
	if !responsesws.IsSteeringControlEvent(envelope.Type) {
		return true
	}
	var steer struct {
		ID                 string `json:"id"`
		PreviousResponseID string `json:"previous_response_id"`
	}
	s := &a.steering
	if s.next == nil || json.Unmarshal(envelope.Object["steer"], &steer) != nil || steer.PreviousResponseID != s.parentID || len(steer.ID) > responsesWSSteerMaxIDBytes {
		return true
	}
	// 只有会消解本候选义务的事实才参与水位。未知诊断值不能推进父序号，
	// 否则一条无账务意义的迟到回执就可能使合法 terminal 被误判为倒序。
	switch envelope.Type {
	case "response.steer.accepted":
		if s.awaiting == 0 || steer.ID == "" || s.accepted[steer.ID] {
			return true
		}
	case "response.steer.failed":
		if !s.accepted[steer.ID] && (steer.ID != "" || s.awaiting == 0) {
			return true
		}
	case "response.steer.pending":
		var reason string
		if !s.parentCompleted || s.waiting || !s.accepted[steer.ID] || json.Unmarshal(envelope.Object["reason"], &reason) != nil || reason != "waiting_for_required_input" {
			return true
		}
	}
	var sequenceValue *int64
	if json.Unmarshal(envelope.Object["sequence_number"], &sequenceValue) != nil || sequenceValue == nil || *sequenceValue < 0 {
		return true
	}
	sequence := *sequenceValue
	if parent := a.turns.active.attempt; parent != nil && parent.SeenProviderResponseID == s.parentID {
		if a.turns.active.hasLastProviderSequence && sequence <= a.turns.active.lastProviderSequence {
			return true
		}
		a.turns.active.lastProviderSequence, a.turns.active.hasLastProviderSequence = sequence, true
	}
	if s.hasSequence && sequence <= s.lastSequence {
		return true
	}
	s.lastSequence, s.hasSequence = sequence, true
	switch envelope.Type {
	case "response.steer.accepted":
		s.awaiting--
		if s.accepted == nil {
			s.accepted = make(map[string]bool)
		}
		s.accepted[steer.ID] = true
	case "response.steer.failed":
		if s.accepted[steer.ID] {
			delete(s.accepted, steer.ID)
		} else if steer.ID == "" && s.awaiting > 0 {
			s.awaiting--
		}
	case "response.steer.pending":
		s.waiting = true
	}
	a.resolveSteeringReservation()
	return true
}

func (a *ResponsesWSSessionActor) consumeSteeringSend(event ResponsesWSEventSendResult) {
	if event.UpstreamSessionGeneration != a.upstream.sessionGeneration || event.SelectedChannelID != a.upstream.channelID {
		return
	}
	if event.Completion == nil {
		a.failClosed("responses_ws_missing_send_completion")
		return
	}
	if event.Completion.consumed {
		return
	}
	event.Completion.consumed = true
	if err := responsesws.ValidateResponsesWSTransportSendResult(event.TransportResult); err != nil {
		a.failClosed("responses_ws_transport_contract_violation")
		return
	}
	switch event.TransportResult.Status {
	case responsesws.ResponsesWSTransportSendAttempted:
		return
	case responsesws.ResponsesWSTransportSendNotAttempted:
		s := &a.steering
		if s.next == nil || s.next.AttemptID != event.AttemptID || s.parentID != event.ResponseID {
			return
		}
		if s.awaiting == 0 {
			a.failClosed("responses_ws_contradictory_steering_send")
			return
		}
		s.awaiting--
		a.resolveSteeringReservation()
	default:
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadGateway, "responses_ws_steer_send_failed", "steering delivery is not confirmed; automatic replay is unavailable"))
		a.close("responses_ws_steer_send_failed")
	}
}

func (a *ResponsesWSSessionActor) activateSteeringSuccessor(event ResponsesWSEventProviderDownstream) bool {
	s := &a.steering
	if s.next == nil || event.Frame == nil || event.DetailOrigin != responsesws.RecvDetailOriginProviderFrame {
		return true
	}
	envelope, err := responsesws.ParseProviderEventEnvelope(event.Frame.Payload())
	if err != nil || envelope.Type != "response.created" {
		return true
	}
	var response struct {
		ID                 string `json:"id"`
		PreviousResponseID string `json:"previous_response_id"`
	}
	if json.Unmarshal(envelope.Object["response"], &response) != nil || response.ID == "" || response.ID == s.parentID ||
		(response.PreviousResponseID != "" && response.PreviousResponseID != s.parentID) || !s.parentTerminal || a.turns.pending.attempt != nil || a.turns.active.attempt != nil || !s.next.Billing.SubmissionClaimed() || s.next.RolledBack || s.next.QuotaFinalized {
		a.failClosed("responses_ws_invalid_steering_successor")
		return false
	}
	next := s.next
	if s.awaiting == 0 {
		a.releaseSteeringParent(s.parentID)
	}
	a.steering = responsesWSSteerState{}
	next.MarkProviderAccepted("steering_successor", response.ID)
	a.turns.active = responsesWSActiveTurn{attempt: next, channelID: next.SelectedChannelID, affinity: CommitResponsesTurnAffinity(next.Candidate, next.SelectedChannelID)}
	a.state = responsesWSStateInFlight
	a.armActiveTurnWatchdog()
	return true
}

func responsesWSWorkflowStop(event ResponsesWSEventProviderDownstream) bool {
	if event.Frame == nil || event.Frame.Kind() != responsesws.FrameKindText || event.DetailOrigin != responsesws.RecvDetailOriginProviderFrame {
		return false
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Response struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(event.Frame.Payload(), &envelope) != nil {
		return false
	}
	return common.ProviderErrorStopsWorkflow(types.OpenAIError{Code: envelope.Error.Code}) || common.ProviderErrorStopsWorkflow(types.OpenAIError{Code: envelope.Response.Error.Code})
}
