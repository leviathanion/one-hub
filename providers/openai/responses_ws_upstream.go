package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"one-api/common"
	"one-api/common/jsonobject"
	commonresponses "one-api/common/responses"
	"one-api/common/responsesws"
	"one-api/model"
	"one-api/types"
)

// 每个真实 response 独立观察工具用量；观察容量不影响原帧交付。
const openAIResponsesWSMaxTrackedResponses = 64

type openAIResponsesWSAdapter struct {
	mu        sync.Mutex
	responses map[string]*openAIResponsesWSToolObservation
}

type openAIResponsesWSToolObservation struct {
	searchServiceType  string
	searchType         string
	responseModel      string
	serviceTier        string
	toolBillingTracker commonresponses.ToolBillingStreamTracker
}

func (p *OpenAIProvider) OpenResponsesWS(ctx context.Context, req *responsesws.OpenRequest) (responsesws.Upstream, *types.OpenAIErrorWithStatusCode) {
	if p == nil {
		return nil, common.StringErrorWrapperLocal("provider is required", "ws_request_failed", http.StatusInternalServerError)
	}
	if req == nil {
		return nil, common.StringErrorWrapperLocal("responses websocket open request is required", "invalid_request_error", http.StatusBadRequest)
	}
	modelName := req.SelectedModel
	if !p.supportsNativeResponsesWSTransport() {
		return nil, responsesWSUnsupportedForChannel()
	}
	conn, credentials, errWithCode := p.openResponsesWSConnWithHeaders(ctx, modelName, req.InboundHeaders)
	if errWithCode != nil {
		return nil, errWithCode
	}
	return responsesws.NewNativeSession(conn, &openAIResponsesWSAdapter{}, responsesws.NativeSessionOptions{
		Credentials:  credentials,
		Context:      ctx,
		Diagnostics:  req.Diagnostics,
		ProviderName: "openai",
		ChannelID:    req.ChannelID,
		Transport:    "responses-ws",
	}), nil
}

func responsesWSUnsupportedForChannel() *types.OpenAIErrorWithStatusCode {
	return common.StringErrorWrapperLocal("channel does not support Responses websocket transport", "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired)
}

func (p *OpenAIProvider) supportsNativeResponsesWSTransport() bool {
	if p == nil {
		return false
	}
	if p.Channel == nil {
		return false
	}
	if p.IsAzure {
		return true
	}
	if strings.TrimSpace(p.Config.Responses) == "" {
		return false
	}
	if model.IsOfficialOpenAIBaseURL(p.GetBaseURL()) {
		return true
	}
	if p.responsesWSNativeExplicitlyEnabled() {
		return true
	}
	return false
}

func (p *OpenAIProvider) responsesWSNativeExplicitlyEnabled() bool {
	if p == nil || p.Channel == nil {
		return false
	}
	enabled, _ := p.Channel.GetOtherBoolField("responses_ws_native")
	return enabled
}

func (a *openAIResponsesWSAdapter) PrepareClientFrame(_ context.Context, frame responsesws.Frame) (responsesws.Frame, error) {
	if frame.Kind() != responsesws.FrameKindText {
		return responsesws.Frame{}, responsesws.ErrInvalidFrame
	}
	if _, err := responsesws.ParseClientEventEnvelope(frame.Payload()); err != nil {
		return responsesws.Frame{}, err
	}
	return frame, nil
}

func (a *openAIResponsesWSAdapter) HandleProviderFrame(_ context.Context, frame responsesws.Frame) responsesws.ProviderFrameResult {
	if frame.Kind() != responsesws.FrameKindText {
		return responsesws.ProviderFrameResult{
			Origin:         responsesws.RecvDetailOriginProviderMalformed,
			Err:            responsesws.ErrNativeProtocol,
			CloseTransport: true,
		}
	}
	payload := frame.Payload()

	envelope, err := responsesws.ParseProviderEventEnvelope(payload)
	if err != nil {
		return responsesws.ProviderFrameResult{
			Origin:         responsesws.RecvDetailOriginProviderMalformed,
			Err:            err,
			CloseTransport: true,
		}
	}
	if envelope.Type == "session.created" || envelope.Type == "session.updated" {
		payload, err = sanitizeOpenAIResponsesWSSession(payload, envelope.Object["session"])
		if err != nil {
			return responsesws.ProviderFrameResult{Origin: responsesws.RecvDetailOriginProviderMalformed, Err: err, CloseTransport: true}
		}
	}
	if responsesws.IsAuxiliaryControlEvent(envelope.Type) {
		out := responsesws.NewTextFrame(payload)
		return responsesws.ProviderFrameResult{EmitFrame: &out, Origin: responsesws.RecvDetailOriginProviderFrame}
	}
	// 计费观察失败不会改变安全原帧的交付。
	usage, _ := a.acceptedProviderUsage(envelope.EventID, payload)

	out := responsesws.NewTextFrame(append([]byte(nil), payload...))
	return responsesws.ProviderFrameResult{
		EmitFrame: &out,
		Usage:     usage,
		Origin:    responsesws.RecvDetailOriginProviderFrame,
	}
}

// 仅移除会话协议中的上游临时凭据；业务数据和无凭据原帧不改写。
func sanitizeOpenAIResponsesWSSession(payload, sessionRaw []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(sessionRaw, &fields) != nil {
		return payload, nil
	}
	if _, exists := fields["client_secret"]; !exists {
		return payload, nil
	}
	session, err := jsonobject.Parse(sessionRaw)
	if err != nil {
		return nil, responsesws.ErrInvalidProviderEventPayload
	}
	session.Delete("client_secret")
	safe, err := session.MarshalJSON()
	if err != nil {
		return nil, err
	}
	envelope, err := jsonobject.Parse(payload)
	if err != nil {
		return nil, responsesws.ErrInvalidProviderEventPayload
	}
	if err := envelope.SetRaw("session", safe); err != nil {
		return nil, err
	}
	return envelope.MarshalJSON()
}

// acceptedProviderUsage extracts only facts accepted at the provider-frame
// boundary.  Token snapshots keep the existing terminal path.  A completed
// web_search_call is emitted as an independent delta because it is valid
// provider billing evidence even when the overall response has no usage.
func (a *openAIResponsesWSAdapter) acceptedProviderUsage(providerEventID string, payload []byte) (*types.UsageEvent, error) {
	if a == nil {
		return openAIResponsesWSEventUsage(providerEventID, payload), nil
	}
	event, ok := commonresponses.ParseStreamUsageEvent(payload)
	if !ok {
		return nil, nil
	}

	responseID := responsesws.ProviderResponseID(payload)
	if responseID == "" || len(responseID) > 1024 {
		return nil, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.responses[responseID]
	switch event.Type {
	case "response.created":
		if state == nil {
			if len(a.responses) >= openAIResponsesWSMaxTrackedResponses {
				return nil, nil
			}
			if a.responses == nil {
				a.responses = make(map[string]*openAIResponsesWSToolObservation)
			}
			state = &openAIResponsesWSToolObservation{}
			a.responses[responseID] = state
		}
		service, search := commonresponses.ResponsesSearchBilling(event.Response)
		if state.searchServiceType == "" {
			state.searchServiceType = service
		}
		if state.searchType == "" {
			state.searchType = search
		}
		if event.Response != nil {
			if state.responseModel == "" {
				state.responseModel = strings.TrimSpace(event.Response.Model)
			}
			if state.serviceTier == "" {
				state.serviceTier = strings.TrimSpace(event.Response.ServiceTier)
			}
		}
		return nil, nil
	case "response.output_item.done":
		if state == nil || event.Item == nil || event.Item.Type != types.InputTypeWebSearchCall {
			return nil, nil
		}
		observed := &types.Usage{}
		if err := commonresponses.ApplyResponsesStreamOutputItemBillingWithToolTracker(
			observed,
			event.Type,
			event.Item,
			event.ItemID,
			event.OutputIndex,
			state.searchServiceType,
			state.searchType,
			&state.toolBillingTracker,
		); err != nil {
			service := state.searchServiceType
			if service == "" {
				service = types.APIToolTypeWebSearchPreview
			}
			if fatal := commonresponses.ObserveBillingFailure(observed, service, err); fatal != nil {
				observed.AddBillingDiagnostic(commonresponses.ResponsesStreamTrackingFailureCode(fatal))
			}
		}
		if len(observed.ExtraBilling) == 0 && len(observed.BillingDiagnostics) == 0 {
			return nil, nil
		}
		return &types.UsageEvent{
			ProviderEventID:      providerEventID,
			ResponseID:           responseID,
			ResponseModel:        state.responseModel,
			ServiceTier:          state.serviceTier,
			ItemID:               event.ItemID,
			ExtraBilling:         observed.ExtraBilling,
			BillingDiagnostics:   observed.BillingDiagnostics,
			ProviderExtraBilling: observed.ProviderExtraBilling,
		}, nil
	default:
		switch event.Type {
		case "response.completed", "response.failed", "response.incomplete", "response.cancelled":
			delete(a.responses, responseID)
		}
		return openAIResponsesWSEventUsage(providerEventID, payload), nil
	}
}

func openAIResponsesWSEventUsage(providerEventID string, payload []byte) *types.UsageEvent {
	event, ok := commonresponses.ParseStreamUsageEvent(payload)
	if !ok || event.Response == nil || event.Response.Usage == nil {
		return nil
	}
	event.Response.Usage.MarkProviderReported()
	usage := event.Response.Usage.ToOpenAIUsage()
	if usage == nil {
		return nil
	}
	event.Response.ApplyUsageAttribution(usage)
	return &types.UsageEvent{
		InputTokens:           usage.PromptTokens,
		OutputTokens:          usage.CompletionTokens,
		TotalTokens:           usage.TotalTokens,
		InputTokenDetails:     usage.PromptTokensDetails,
		OutputTokenDetails:    usage.CompletionTokensDetails,
		Source:                types.UsageSourceResponsesResponse,
		BillingBasis:          types.UsageBillingBasisTokens,
		ProviderEventID:       providerEventID,
		ResponseID:            event.Response.ID,
		ResponseModel:         strings.TrimSpace(event.Response.Model),
		ServiceTier:           strings.TrimSpace(event.Response.ServiceTier),
		ExtraTokens:           usage.GetExtraTokens(),
		ProviderTokenEvidence: usage.HasProviderUsage(),
		AttributionConflict:   usage.AttributionConflict,
		BillingDiagnostics:    usage.BillingDiagnostics,
	}
}

func (a *openAIResponsesWSAdapter) MapProviderClose(_ context.Context, info responsesws.ProviderCloseInfo) responsesws.ProviderCloseResult {
	if openAIResponsesWSNativeProviderCloseInfo(info) {
		return responsesws.ProviderCloseResult{
			ProviderClose: &responsesws.ProviderClose{Code: info.Code, Reason: info.Reason, Err: info.Err},
			Origin:        responsesws.RecvDetailOriginNativeProviderClose,
		}
	}
	return responsesws.ProviderCloseResult{}
}

func openAIResponsesWSNativeProviderCloseInfo(info responsesws.ProviderCloseInfo) bool {
	return info.Kind == responsesws.ProviderCloseKindPeerClose
}

var _ responsesws.ProviderAdapter = (*openAIResponsesWSAdapter)(nil)
