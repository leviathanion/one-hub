package openai

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"one-api/common"
	commonresponses "one-api/common/responses"
	"one-api/common/responsesws"
	"one-api/model"
	"one-api/types"
)

// openAIResponsesWSAdapter is scoped to one native Responses websocket.  The
// provider's output_item.done event carries the completed item but does not
// repeat the response tool declaration, so the accepted response.created
// snapshot must remain available until that item is observed.  The tracker is
// bounded by commonresponses and is reset at each new response lifecycle.
type openAIResponsesWSAdapter struct {
	mu                 sync.Mutex
	searchServiceType  string
	searchType         string
	responseID         string
	responseModel      string
	serviceTier        string
	responseCreated    bool
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
	conn, errWithCode := p.openResponsesWSConnWithHeaders(ctx, modelName, req.InboundHeaders)
	if errWithCode != nil {
		return nil, errWithCode
	}
	return responsesws.NewNativeSession(conn, &openAIResponsesWSAdapter{}, responsesws.NativeSessionOptions{
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
	envelope, err := responsesws.ParseClientEventEnvelope(frame.Payload())
	if err != nil {
		return responsesws.Frame{}, err
	}
	// A native session may carry more than one Responses turn.  Resetting on
	// an accepted turn-start frame prevents an item identity from suppressing a
	// same-named item in a later turn.  response.created below also resets the
	// state at the provider acceptance boundary, covering callers that inject a
	// first frame through the transport fixture.
	switch strings.TrimSpace(envelope.Type) {
	case "response.create":
		a.resetSearchState()
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
	eventType := strings.TrimSpace(envelope.Type)
	if eventType == types.EventTypeSessionCreated {
		return responsesws.ProviderFrameResult{
			Filtered: true,
			Origin:   responsesws.RecvDetailOriginProviderFrame,
		}
	}
	if responsesws.IsAuxiliaryControlEvent(envelope.Type) {
		out := responsesws.NewTextFrame(payload)
		return responsesws.ProviderFrameResult{EmitFrame: &out, Origin: responsesws.RecvDetailOriginProviderFrame}
	}
	usage, usageErr := a.acceptedProviderUsage(envelope.EventID, payload)
	if usageErr != nil {
		return responsesws.ProviderFrameResult{
			Origin:         responsesws.RecvDetailOriginProviderMalformed,
			Err:            usageErr,
			CloseTransport: true,
		}
	}

	out := responsesws.NewTextFrame(append([]byte(nil), payload...))
	return responsesws.ProviderFrameResult{
		EmitFrame: &out,
		Usage:     usage,
		Origin:    responsesws.RecvDetailOriginProviderFrame,
	}
}

func (a *openAIResponsesWSAdapter) resetSearchState() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.resetSearchStateLocked()
	a.mu.Unlock()
}

func (a *openAIResponsesWSAdapter) resetSearchStateLocked() {
	if a == nil {
		return
	}
	a.searchServiceType = ""
	a.searchType = ""
	a.responseID = ""
	a.responseModel = ""
	a.serviceTier = ""
	a.responseCreated = false
	a.toolBillingTracker = commonresponses.ToolBillingStreamTracker{}
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

	a.mu.Lock()
	defer a.mu.Unlock()
	if event.Type != "response.created" && event.Response != nil && event.Response.ID != "" && a.responseID != "" && event.Response.ID != a.responseID {
		return nil, nil
	}

	switch event.Type {
	case "response.created":
		// The tool declaration is attribution metadata, not execution evidence.
		// Only a later completed output item can create a billable unit.
		incomingServiceType, incomingSearchType := commonresponses.ResponsesSearchBilling(event.Response)
		incomingResponseID := ""
		incomingResponseModel := ""
		incomingServiceTier := ""
		if event.Response != nil {
			incomingResponseID = strings.TrimSpace(event.Response.ID)
			incomingResponseModel = strings.TrimSpace(event.Response.Model)
			incomingServiceTier = strings.TrimSpace(event.Response.ServiceTier)
		}
		if a.responseCreated && incomingResponseID != "" && a.responseID != "" && incomingResponseID != a.responseID {
			// A different provider response is a new lifecycle.  It may reuse an
			// item ID, so its evidence must not inherit the prior turn's tracker.
			a.resetSearchStateLocked()
		} else if a.responseCreated {
			// Repeated declarations for the current response, including an
			// ownerless declaration, must not clear accepted item evidence.  Fill
			// metadata that the first declaration omitted, but keep the original
			// owner dimensions and tracker decisions authoritative.
			if a.searchServiceType == "" {
				a.searchServiceType = incomingServiceType
			}
			if a.searchType == "" {
				a.searchType = incomingSearchType
			}
			if a.responseID == "" {
				a.responseID = incomingResponseID
			}
			if a.responseModel == "" {
				a.responseModel = incomingResponseModel
			}
			if a.serviceTier == "" {
				a.serviceTier = incomingServiceTier
			}
			return nil, nil
		}
		a.searchServiceType = incomingServiceType
		a.searchType = incomingSearchType
		a.responseID = incomingResponseID
		a.responseModel = incomingResponseModel
		a.serviceTier = incomingServiceTier
		a.responseCreated = true
		a.toolBillingTracker = commonresponses.ToolBillingStreamTracker{}
		return nil, nil
	case "response.output_item.done":
		if event.Item == nil || event.Item.Type != types.InputTypeWebSearchCall {
			return nil, nil
		}
		observed := &types.Usage{}
		if err := commonresponses.ApplyResponsesStreamOutputItemBillingWithToolTracker(
			observed,
			event.Type,
			event.Item,
			event.ItemID,
			event.OutputIndex,
			a.searchServiceType,
			a.searchType,
			&a.toolBillingTracker,
		); err != nil {
			service := a.searchServiceType
			if service == "" {
				service = types.APIToolTypeWebSearchPreview
			}
			if fatal := commonresponses.ObserveBillingFailure(observed, service, err); fatal != nil {
				return nil, common.ErrorWrapperLocal(fatal, commonresponses.ResponsesStreamTrackingFailureCode(fatal), http.StatusBadGateway)
			}
		}
		if len(observed.ExtraBilling) == 0 && len(observed.BillingDiagnostics) == 0 {
			return nil, nil
		}
		return &types.UsageEvent{
			ProviderEventID:      strings.TrimSpace(providerEventID),
			ResponseID:           a.responseID,
			ResponseModel:        a.responseModel,
			ServiceTier:          a.serviceTier,
			ItemID:               strings.TrimSpace(event.ItemID),
			ExtraBilling:         observed.ExtraBilling,
			BillingDiagnostics:   observed.BillingDiagnostics,
			ProviderExtraBilling: observed.ProviderExtraBilling,
		}, nil
	default:
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
		ProviderEventID:       strings.TrimSpace(providerEventID),
		ResponseID:            strings.TrimSpace(event.Response.ID),
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
