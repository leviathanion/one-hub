package relay

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"one-api/common/responsesws"
)

const responsesWSObservationLimit = 64
const responsesWSObservationBytes = 32 << 20

// Actor-owned evidence only. No lane scheduling or upstream lifecycle state.
// The fixed filters may produce false positives (foregoing billing), never a
// false negative that could attach an old response to a newer reservation.
type responsesWSObservationFilter [256]uint64

func (f *responsesWSObservationFilter) contains(key string) bool {
	digest := sha256.Sum256([]byte(key))
	for i := 0; i < 3; i++ {
		bit := binary.LittleEndian.Uint16(digest[i*2:]) % (256 * 64)
		if f[bit/64]&(uint64(1)<<(bit%64)) == 0 {
			return false
		}
	}
	return true
}
func (f *responsesWSObservationFilter) add(key string) {
	digest := sha256.Sum256([]byte(key))
	for i := 0; i < 3; i++ {
		bit := binary.LittleEndian.Uint16(digest[i*2:]) % (256 * 64)
		f[bit/64] |= uint64(1) << (bit % 64)
	}
}

type responsesWSObservedWork struct {
	steerAwaiting   int
	steerAccepted   map[string]bool
	attempt         *ResponsesWSTurnAttempt
	lane            string
	successorParent string
	bytes           int
}
type responsesWSObservations struct {
	works  []*responsesWSObservedWork
	bytes  int
	seen   responsesWSObservationFilter
	unsafe responsesWSObservationFilter
	// 精确、有限的终结身份区分真实迟到回执与 seen 过滤器碰撞。
	retired         [responsesWSObservationLimit]string
	retiredNext     int
	controls        responsesWSObservationFilter
	unscopedControl bool
}

func responsesWSFrameLane(frame *responsesws.RawResponsesCreateFrame) string {
	if frame == nil {
		return ""
	}
	return responsesWSLane(frame.Object)
}
func responsesWSLane(object map[string]json.RawMessage) string {
	var lane string
	// Invalid shapes remain untouched on the wire. They cannot provide an
	// observation identity, and must not alias the default lane.
	raw, ok := object["stream_id"]
	if !ok {
		return ""
	}
	if json.Unmarshal(raw, &lane) != nil {
		return "\x00" + string(raw)
	}
	return lane
}
func (o *responsesWSObservations) add(attempt *ResponsesWSTurnAttempt, lane, parent string) error {
	size := len(lane) + len(parent)
	if attempt.RequestFrame != nil {
		size += len(attempt.RequestFrame.Raw)
	}
	if len(o.works) >= responsesWSObservationLimit || size > responsesWSObservationBytes-o.bytes {
		return errors.New("responses websocket work observation capacity exceeded")
	}
	o.works = append(o.works, &responsesWSObservedWork{attempt: attempt, lane: lane, successorParent: parent, bytes: size})
	o.bytes += size
	return nil
}
func (o *responsesWSObservations) byAttempt(id string) *responsesWSObservedWork {
	if id == "" {
		return nil
	}
	for _, w := range o.works {
		if w.attempt.AttemptID == id {
			return w
		}
	}
	return nil
}
func (o *responsesWSObservations) byResponse(id string) *responsesWSObservedWork {
	if id == "" {
		return nil
	}
	for _, w := range o.works {
		if w.attempt.SeenProviderResponseID == id {
			return w
		}
	}
	return nil
}
func (o *responsesWSObservations) remove(id string) {
	for i, w := range o.works {
		if w.attempt.AttemptID != id {
			continue
		}
		o.bytes -= w.bytes
		copy(o.works[i:], o.works[i+1:])
		o.works[len(o.works)-1] = nil
		o.works = o.works[:len(o.works)-1]
		return
	}
}
func (o *responsesWSObservations) unsafeLane(lane string) bool { return o.unsafe.contains(lane) }

func (o *responsesWSObservations) retiredResponse(id string) bool {
	if id == "" {
		return false
	}
	for _, old := range o.retired {
		if old == id {
			return true
		}
	}
	return false
}
func (o *responsesWSObservations) rememberRetiredResponse(id string) {
	if id == "" || len(id) > responsesWSSteerMaxIDBytes || o.retiredResponse(id) {
		return
	}
	o.retired[o.retiredNext] = id
	o.retiredNext = (o.retiredNext + 1) % len(o.retired)
}

// 没有命令身份的控制回执不能拿来证明另一条 create 被拒绝。
// 这些有界事实仅影响错误关联，不阻挡发送，也不镜像上游业务状态。
func (a *ResponsesWSSessionActor) noteAuxiliaryCommand(attemptID string, frame responsesws.Frame, purpose ResponsesWSSendPurpose) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(frame.Payload(), &envelope) != nil {
		a.observation.unscopedControl = true
		return
	}
	// 错误可能使用命令携带的 lane，也可能使用目标 Response 的 lane。
	a.observation.controls.add(responsesWSLane(envelope))
	if purpose == ResponsesWSSendPurposeResponseSteer {
		if work := a.observation.byAttempt(attemptID); work != nil {
			a.observation.controls.add(work.lane)
			return
		}
	}
	var command struct {
		ResponseID string `json:"response_id"`
	}
	if json.Unmarshal(frame.Payload(), &command) == nil && command.ResponseID != "" {
		if work := a.observation.byResponse(command.ResponseID); work != nil {
			a.observation.controls.add(work.lane)
			return
		}
		if proof, ok := a.heldSteeringParents[command.ResponseID]; ok {
			a.observation.controls.add(proof.lane)
			return
		}
	}
	a.observation.unscopedControl = true
}

// 原生 request-level rejection 不会创建 Response。只有该 lane 的唯一
// 普通 create 能解释此回执时，才将失败限制在该候选；其他情形保持不猜配。
func (a *ResponsesWSSessionActor) finishRejectedCreate(object map[string]json.RawMessage) bool {
	lane := responsesWSLane(object)
	if a.observation.unsafeLane(lane) || a.observation.unscopedControl || a.observation.controls.contains(lane) {
		return false
	}
	var rejection map[string]json.RawMessage
	if json.Unmarshal(object["error"], &rejection) != nil || rejection == nil {
		return false
	}
	var rejectionType, rejectionCode string
	_ = json.Unmarshal(rejection["type"], &rejectionType)
	_ = json.Unmarshal(rejection["code"], &rejectionCode)
	var status int
	_ = json.Unmarshal(object["status"], &status)
	if rejectionType != "invalid_request_error" && rejectionCode != "invalid_request_error" && !(status >= 400 && status < 500 && status != 408) {
		return false
	}
	if rejectionCode == "websocket_connection_limit_reached" {
		return false
	}
	var candidate *responsesWSObservedWork
	for _, work := range a.observation.works {
		if work.lane != lane {
			continue
		}
		if candidate != nil || work.successorParent != "" || work.attempt.SeenProviderResponseID != "" {
			return false
		}
		candidate = work
	}
	if candidate == nil {
		return false
	}
	return a.finishObservedWork(candidate)
}

func (a *ResponsesWSSessionActor) finishObservedWork(work *responsesWSObservedWork) bool {
	if work == nil {
		return true
	}
	attempt := work.attempt
	attempt.MarkCompleted(time.Now())
	ctx, cancel := boundedResponsesLifecycleContext(context.WithoutCancel(a.logContext()))
	_, err := attempt.ApplyResponsesWSSettlementDecisionWithContext(ctx, attempt.Context(), projectResponsesWSSharedDecision(attempt))
	cancel()
	if err != nil {
		a.logErrorf("responses websocket observation settlement failed: %v", err)
		return false
	}
	if attempt.SeenProviderResponseID != "" {
		a.observation.seen.add(attempt.SeenProviderResponseID)
		a.observation.rememberRetiredResponse(attempt.SeenProviderResponseID)
	}
	a.observation.remove(attempt.AttemptID)
	if a.turns.pending.attempt == attempt {
		a.turns.pending = responsesWSPendingTurn{}
	}
	return true
}
func (a *ResponsesWSSessionActor) finishAllObservedWorks() {
	for _, work := range append([]*responsesWSObservedWork(nil), a.observation.works...) {
		a.finishObservedWork(work)
	}
}
func (a *ResponsesWSSessionActor) abandonLaneObservation(lane string) {
	// Only unbound candidates depend on the lost FIFO correspondence. Already
	// identified responses retain their independent component evidence.
	for _, work := range append([]*responsesWSObservedWork(nil), a.observation.works...) {
		if work.lane == lane && work.attempt.SeenProviderResponseID == "" {
			a.observation.unsafe.add(lane)
			a.finishObservedWork(work)
		}
	}
}
func (a *ResponsesWSSessionActor) parallelProviderSource(generation string, channel int) bool {
	if generation != a.upstream.sessionGeneration {
		return false
	}
	if channel > 0 && channel != a.upstream.channelID {
		a.failClosed("responses_ws_provider_channel_mismatch")
		return false
	}
	return true
}

func (a *ResponsesWSSessionActor) bindObservedResponse(object map[string]json.RawMessage, id string) *responsesWSObservedWork {
	if id == "" || len(id) > responsesWSSteerMaxIDBytes {
		a.abandonLaneObservation(responsesWSLane(object))
		return nil
	}
	if work := a.observation.byResponse(id); work != nil {
		return work
	}
	if a.observation.retiredResponse(id) {
		return nil
	}
	if a.observation.seen.contains(id) {
		// This may be a duplicate or a filter collision. In either case the
		// next created event must not shift into this candidate's FIFO slot.
		a.abandonLaneObservation(responsesWSLane(object))
		return nil
	}
	a.observation.seen.add(id)
	lane := responsesWSLane(object)
	if a.observation.unsafeLane(lane) {
		a.abandonLaneObservation(lane)
		return nil
	}
	var creates, successors []*responsesWSObservedWork
	for _, w := range a.observation.works {
		if w.lane != lane || w.attempt.SeenProviderResponseID != "" {
			continue
		}
		if w.successorParent == "" {
			creates = append(creates, w)
		} else {
			successors = append(successors, w)
		}
	}
	var selected *responsesWSObservedWork
	switch {
	case len(successors) == 0 && len(creates) > 0:
		selected = creates[0] // The native protocol guarantees ordinary create FIFO per lane.
	case len(creates) == 0 && len(successors) == 1:
		var response struct {
			PreviousResponseID string `json:"previous_response_id"`
		}
		if json.Unmarshal(object["response"], &response) == nil && response.PreviousResponseID == successors[0].successorParent {
			selected = successors[0]
		}
	}
	if selected == nil {
		a.abandonLaneObservation(lane)
		return nil
	}
	selected.attempt.RememberProviderResponseID(id)
	return selected
}

func (a *ResponsesWSSessionActor) observeParallelDownstream(event ResponsesWSEventProviderDownstream) {
	if !a.parallelProviderSource(event.UpstreamSessionGeneration, event.ChannelID) {
		return
	}
	if event.Kind == ProviderDownstreamClose {
		a.closeProviderDownstream(ResponsesWSEventProviderClosed{Code: event.CloseCode, Reason: event.CloseReason})
		a.close("provider_closed")
		return
	}
	if event.Frame == nil {
		return
	}
	if event.DetailOrigin != responsesws.RecvDetailOriginProviderFrame {
		if responsesWSProviderPayloadPolicyForEvent(upstreamEventFromProviderDownstream(event)).PayloadOrigin != responsesws.PayloadOriginProvider {
			a.writeProxyLocal(event.Frame.Payload())
			return
		}
	}
	payload := event.Frame.Payload()
	var object map[string]json.RawMessage
	var eventType string
	if event.Frame.Kind() == responsesws.FrameKindText && json.Unmarshal(payload, &object) == nil {
		_ = json.Unmarshal(object["type"], &eventType)
	}
	id := responsesWSPayloadResponseID(payload)
	work := a.observation.byResponse(id)
	if eventType == "response.created" {
		work = a.bindObservedResponse(object, id)
	}
	a.observeSteeringReceipt(eventType, object)
	rejectedCreate := eventType == "error" && id == "" && a.finishRejectedCreate(object)
	var attempt *ResponsesWSTurnAttempt
	if work != nil {
		attempt = work.attempt
	}
	classified := responsesws.ClassifyResponsesWSEvent(payload)
	terminal := eventType == "response.completed" || eventType == "response.failed" || eventType == "response.incomplete"
	if work == nil && (terminal || eventType == "error") && !rejectedCreate && !a.observation.retiredResponse(id) {
		a.abandonLaneObservation(responsesWSLane(object))
		if id != "" {
			a.observation.seen.add(id)
		}
	}
	if attempt != nil {
		attempt.MarkFirstProviderResponse(event.ReceivedAt)
		_ = attempt.ObserveResponsesStreamPayload(payload) // component diagnostics do not govern raw delivery
		if responsesWSProviderResponseIDsAgree(id, event.ResponseID) && (event.Usage == nil || responsesWSProviderResponseIDsAgree(id, event.Usage.ResponseID)) {
			mergeResponsesWSAttachedFrameUsage(attempt.Usage, classified, event.Usage)
		}
		if classified.Response != nil {
			mergeResponsesWSTerminalResponse(attempt.Usage, classified.Response, &attempt.imageGenerationTracker)
		}
		if terminal && !classified.Malformed {
			attempt.MarkCompleted(event.ReceivedAt)
			attempt.MarkProviderTerminalEvidence(classified)
		}
	}
	// Even unattributable billing has an authenticated connection owner. Persist
	// the resource before its first delivery; do not infer a work or price from it.
	var resource struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(object["response"], &resource)
	if (eventType == "response.created" || terminal) && resource.ID != "" {
		if attempt != nil && attempt.SeenProviderResponseID == resource.ID {
			if !a.ensureProviderResponseDelivery(attempt, event) {
				return
			}
		} else if apiErr := persistStoredResponseOwner(a.Context(), resource.ID, a.upstream.channelID); apiErr != nil {
			a.writeProxyLocal(responsesWSErrorFromOpenAI(apiErr))
			a.close("responses_owner_persist_failed")
			return
		}
	}
	if err := a.emitProviderFrameForAttempt(attempt, *event.Frame, "provider_frame"); err != nil {
		a.close("client_write_failed")
		return
	}
	a.processProviderPayloadAPIError(payload, event.ChannelID, "responses_ws_provider_frame")
	if terminal && work != nil {
		if a.finishObservedWork(work) && classified.Kind == responsesws.ResponsesSuccessTerminal {
			RecordResponsesTurnSuccess(attempt.Context(), CommitResponsesTurnAffinity(attempt.Candidate, attempt.SelectedChannelID), classified.Response)
		}
	}
}

func (a *ResponsesWSSessionActor) observationCapacityError() {
	a.writeProxyLocal(responsesWSErrorPayload(http.StatusTooManyRequests, "responses_ws_work_capacity", "responses websocket work observation capacity exceeded"))
}

// Receipts only retire reservation evidence; they never gate client commands.
func (a *ResponsesWSSessionActor) observeSteeringReceipt(eventType string, object map[string]json.RawMessage) {
	if eventType != "response.steer.accepted" && eventType != "response.steer.failed" {
		return
	}
	var receipt struct {
		ID     string `json:"id"`
		Parent string `json:"previous_response_id"`
	}
	if json.Unmarshal(object["steer"], &receipt) != nil || receipt.Parent == "" {
		return
	}
	for _, work := range a.observation.works {
		if work.successorParent != receipt.Parent || work.attempt.SeenProviderResponseID != "" {
			continue
		}
		if eventType == "response.steer.accepted" {
			if receipt.ID == "" || work.steerAccepted[receipt.ID] || work.steerAwaiting == 0 {
				return
			}
			if len(work.steerAccepted) >= responsesWSObservationLimit || len(receipt.ID) > responsesWSSteerMaxIDBytes {
				a.abandonLaneObservation(work.lane)
				return
			}
			if work.steerAccepted == nil {
				work.steerAccepted = make(map[string]bool)
			}
			work.steerAccepted[receipt.ID] = true
			work.steerAwaiting--
			return
		}
		if receipt.ID == "" {
			a.abandonLaneObservation(work.lane)
			return
		}
		if !work.steerAccepted[receipt.ID] {
			return
		}
		delete(work.steerAccepted, receipt.ID)
		if work.steerAwaiting == 0 && len(work.steerAccepted) == 0 {
			a.abandonLaneObservation(work.lane)
		}
		return
	}
}
