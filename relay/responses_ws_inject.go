package relay

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"one-api/common"
	commonresponses "one-api/common/responses"
	"one-api/common/responsesws"
	"one-api/model"
	"one-api/types"
)

func (a *ResponsesWSSessionActor) hasConnectionLocalResponseProof(c *gin.Context, id string) bool {
	if a.hasHeldSteeringParent(c, id) {
		return true
	}
	for _, current := range a.turns.history.localEphemeralResponseIDs {
		if current == id {
			return true
		}
	}
	return false
}

func (a *ResponsesWSSessionActor) authorizeInjectTarget(id string, channelID int) *types.OpenAIErrorWithStatusCode {
	c := a.Context()
	if c == nil || c.GetInt("id") <= 0 {
		return responsesWSInjectOwnerError()
	}
	if attempt := a.currentTurnAttempt(); attempt != nil && attempt.SeenProviderResponseID == id && attempt.SelectedChannelID == channelID {
		return nil
	}
	if channelID == a.upstream.channelID && a.hasConnectionLocalResponseProof(c, id) {
		return nil
	}
	ctx, cancel := boundedResponsesLifecycleContext(responseOwnerRequestContext(c))
	defer cancel()
	owner, err := model.GetResponseOwner(ctx, id, c.GetInt("id"))
	if err != nil && !errors.Is(err, model.ErrResponseOwnerNotFound) {
		return common.StringErrorWrapperLocal("stored response ownership is temporarily unavailable", "responses_owner_store_unavailable", http.StatusServiceUnavailable)
	}
	if owner == nil || owner.UserID != c.GetInt("id") || owner.ChannelID != channelID || owner.State != model.ResponseOwnerStateActive {
		return responsesWSInjectOwnerError()
	}
	return nil
}

func responsesWSInjectOwnerError() *types.OpenAIErrorWithStatusCode {
	return &types.OpenAIErrorWithStatusCode{OpenAIError: types.OpenAIError{Message: "response target is not authorized", Type: "invalid_request_error", Code: "response_not_found", Param: "response_id"}, StatusCode: http.StatusBadRequest, LocalError: true}
}

func (a *ResponsesWSSessionActor) handleClientInject(event ResponsesWSEventClientFrame) {
	if !a.allowNewWork() {
		return
	}
	envelope, err := responsesws.ParseClientEventEnvelope(event.Frame.Payload())
	var target string
	if err != nil || json.Unmarshal(envelope.Object["response_id"], &target) != nil || strings.TrimSpace(target) == "" {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(responsesWSInjectOwnerError()))
		return
	}
	if field, found := commonresponses.FindAccountScopedResourceReferenceJSON(envelope.Object["input"]); found {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadRequest, "unsupported_resource_reference", "inject input requires an account-scoped resource owner: "+field))
		return
	}
	channelID := a.upstream.channelID
	if channelID == 0 && a.currentTurnAttempt() != nil {
		channelID = a.currentTurnAttempt().SelectedChannelID
	}
	if err := a.authorizeInjectTarget(strings.TrimSpace(target), channelID); err != nil {
		a.writeProxyLocal(responsesWSErrorFromOpenAI(err))
		return
	}
	if a.upstream.session == nil || a.turns.pending.attempt != nil {
		if a.currentTurnAttempt() == nil {
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusConflict, "session_closed", "responses websocket session is not open"))
			return
		}
		queue := &a.turns.deferredInjects
		if len(queue.deferred) >= responsesWSInjectMaxPending || event.Frame.PayloadLen() > responsesWSInjectMaxDeferredBytes-queue.deferredBytes {
			a.writeProxyLocal(responsesWSErrorPayload(http.StatusTooManyRequests, "responses_ws_inject_queue_full", "response.inject send queue is full"))
			return
		}
		queue.deferred = append(queue.deferred, event.Frame)
		queue.deferredBytes += event.Frame.PayloadLen()
		return
	}
	if !a.SendProviderAuxiliaryFrame(uuid.NewString(), channelID, a.upstream.session, event.Frame, ResponsesWSSendPurposeResponseInject) {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusServiceUnavailable, "responses_ws_send_queue_full", responsesWSStaticErrorMessage("responses_ws_send_queue_full")))
		a.close("responses_ws_inject_send_queue_full")
	}
}

func (a *ResponsesWSSessionActor) consumeInjectSend(event ResponsesWSEventSendResult) {
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
	if event.TransportResult.Status == responsesws.ResponsesWSTransportSendAmbiguous {
		a.writeProxyLocal(responsesWSErrorPayload(http.StatusBadGateway, "responses_ws_inject_send_failed", "inject delivery is not confirmed"))
		a.close("responses_ws_inject_send_failed")
	} else if event.TransportResult.Err != nil {
		a.writeProxyLocal(responsesWSErrorFromErr(event.TransportResult.Err))
	}
}
