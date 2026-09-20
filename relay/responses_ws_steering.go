package relay

import (
	"encoding/json"
	"net/http"
	"strings"

	"one-api/common/responsesws"
	"one-api/model"

	"github.com/gin-gonic/gin"
)

const (
	responsesWSSteerMaxIDBytes     = 1024
	responsesWSSteerMaxSubmissions = 64
	responsesWSHeldParentLimit     = 64
	responsesWSHeldParentMaxBytes  = 4 << 20
)

// 每条发送独立消费结果，生命周期不依赖计费候选；仅由 actor 修改。
type responsesWSSendCompletion struct{ consumed bool }

// 临时资源授权事实，不保存输入或已结束的计费对象。
type responsesWSParentProof struct {
	userID, tokenID, channelID int
	generation                 string
	frame                      *responsesws.RawResponsesCreateFrame
	lane                       string
}

func (a *ResponsesWSSessionActor) holdSteeringParent(parent *ResponsesWSTurnAttempt) bool {
	if parent == nil || parent.RequestFrame == nil || parent.SeenProviderResponseID == "" || !parent.StoredOwnerPersisted {
		return false
	}
	id := parent.SeenProviderResponseID
	if _, ok := a.heldSteeringParents[id]; ok {
		return true
	}
	ctx := a.Context()
	if ctx == nil {
		return false
	}
	raw, _ := json.Marshal(map[string]any{"type": "response.create", "model": parent.RequestFrame.Projection.Model, "store": parent.RequireStoredOwner})
	frame, err := responsesws.ParseRawResponsesCreateFrame(raw)
	if err != nil {
		return false
	}
	frame.MultiAgentEnabled = parent.MultiAgentEnabled
	lane := responsesWSFrameLane(parent.RequestFrame)
	if work := a.observation.byResponse(id); work != nil {
		lane = work.lane
	}
	size := len(id) + len(raw) + len(lane)
	if len(a.heldSteeringParents) >= responsesWSHeldParentLimit || size > responsesWSHeldParentMaxBytes-a.heldSteeringParentBytes {
		return false
	}
	if a.heldSteeringParents == nil {
		a.heldSteeringParents = make(map[string]responsesWSParentProof)
	}
	a.heldSteeringParents[id] = responsesWSParentProof{userID: ctx.GetInt("id"), tokenID: ctx.GetInt("token_id"), channelID: a.upstream.channelID, generation: a.upstream.sessionGeneration, frame: frame, lane: lane}
	a.heldSteeringParentBytes += size
	return true
}

func (a *ResponsesWSSessionActor) hasHeldSteeringParent(c *gin.Context, id string) bool {
	proof, ok := a.heldSteeringParents[id]
	return ok && c != nil && proof.userID == c.GetInt("id") && proof.tokenID == c.GetInt("token_id") && proof.channelID == a.upstream.channelID && proof.generation == a.upstream.sessionGeneration
}

func (a *ResponsesWSSessionActor) handleClientSteer(event ResponsesWSEventClientFrame) {

	if !a.allowNewWork() {
		return
	}
	envelope, err := responsesws.ParseClientEventEnvelope(event.Frame.Payload())
	var target string
	if err != nil || json.Unmarshal(envelope.Object["previous_response_id"], &target) != nil || strings.TrimSpace(target) == "" {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(responsesWSInjectOwnerError()))
		return
	}
	if apiErr := a.authorizeInjectTarget(target, a.upstream.channelID); apiErr != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
		return
	}
	if err := prepareResourceRequest(a.Context(), event.Frame.Payload(), "responses"); err != nil {
		a.writeProxyLocal(responsesWSErrorFromErr(err))
		return
	}
	if a.upstream.session == nil {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "session_closed", "responses websocket session is not open"))
		return
	}
	var candidate *responsesWSObservedWork
	for _, work := range a.observation.works {
		if work.successorParent == target && work.attempt.SeenProviderResponseID == "" {
			candidate = work
			break
		}
	}
	if candidate == nil {
		if len(a.observation.works) >= responsesWSObservationLimit {
			a.observationCapacityError()
			return
		}
		frame := a.turns.opening.firstFrame
		lane := responsesWSLane(envelope.Object)
		knownSettings := false
		if proof, ok := a.heldSteeringParents[target]; ok && a.hasHeldSteeringParent(a.Context(), target) {
			frame = proof.frame
			lane = proof.lane
			knownSettings = true
		}
		if parent := a.observation.byResponse(target); parent != nil {
			frame = parent.attempt.RequestFrame
			lane = parent.lane
			knownSettings = true
		}
		if frame == nil {
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusServiceUnavailable, "responses_ws_context_missing", "responses websocket model admission context is unavailable"))
			return
		}
		request := frame.Projection
		request.PreviousResponseID = target
		request.Input = nil
		next := a.prepareResponsesWork(frame, request, event.ReceivedAt, false)
		if next == nil {
			return
		}
		next.initializeIdentity()
		if !knownSettings {
			// 资源归属不能替代当前 token 的模型权限。没有父模型事实时，
			// 受限 token 不能借首帧模型获得另一模型的执行许可。
			settings, _ := next.Context().Get("token_setting")
			if setting, ok := settings.(*model.TokenSetting); ok && setting != nil && setting.Limits.LimitModelSetting.Enabled {
				a.writeProxyLocal(responsesWSErrorPayload(http.StatusForbidden, "responses_ws_parent_model_unknown", "response model authorization is unavailable"))
				return
			}
			next.Usage.AttributionConflict = true
		}
		if err := a.observation.add(next, lane, target); err != nil {
			a.observationCapacityError()
			return
		}
		candidate = a.observation.byAttempt(next.AttemptID)
		if apiErr := a.reserveAndClaimResponsesWork(next); apiErr != nil {
			a.finishObservedWork(candidate)
			a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
			return
		}
	}
	candidate.steerAwaiting++
	if !a.SendProviderAuxiliaryFrame(candidate.attempt.AttemptID, a.upstream.channelID, a.upstream.session, event.Frame, ResponsesWSSendPurposeResponseSteer) {
		// This submission was not queued; other submissions may still produce a
		// successor, so abandon observation without altering the upstream workflow.
		a.abandonLaneObservation(candidate.lane)
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusServiceUnavailable, "responses_ws_send_queue_full", "responses websocket send queue is full"))
	}

}

// 交付只依赖已验证的连接；缺失、过期和未来回执不参与账务观察。
