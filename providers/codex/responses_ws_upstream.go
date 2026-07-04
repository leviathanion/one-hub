package codex

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/common/responsesws"
	"one-api/providers/codex/wire"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/google/uuid"
)

type codexResponsesWSAdapter struct {
	provider                  *CodexProvider
	model                     string
	identity                  wire.Identity
	defaultPreviousResponseID string
	responsesLite             bool

	mu           sync.Mutex
	lastResponse string
	accumulator  *codexTurnUsageAccumulator
}

func (p *CodexProvider) OpenResponsesWS(ctx context.Context, req *responsesws.OpenRequest) (responsesws.Upstream, *types.OpenAIErrorWithStatusCode) {
	if req == nil {
		return nil, common.StringErrorWrapperLocal("responses websocket open request is required", "invalid_request_error", http.StatusBadRequest)
	}
	if req.FirstFrame == nil {
		return nil, common.StringErrorWrapperLocal("first response.create frame is required", "invalid_request_error", http.StatusBadRequest)
	}
	normalizedModel := normalizeCodexModelName(req.SelectedModel)
	if normalizedModel == "" {
		normalizedModel = normalizeCodexModelName(req.FirstFrame.Projection.Model)
	}
	switch req.Transport {
	case "", runtimesession.TransportModeResponsesWS:
	case runtimesession.TransportModeResponsesHTTPBridge:
		return nil, common.StringErrorWrapperLocal("Codex Official ResponsesWS does not support HTTP bridge transport", "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired)
	default:
		return nil, common.StringErrorWrapperLocal("invalid responses websocket transport", "invalid_transport", http.StatusBadRequest)
	}
	if p.getWebsocketMode() == codexWebsocketModeOff {
		return nil, common.StringErrorWrapperLocal("channel does not support Responses websocket transport", "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired)
	}
	sessionID := strings.TrimSpace(req.UpstreamSessionID)
	if sessionID == "" {
		sessionID = "responses-ws:" + uuid.NewString()
	}
	if err := validateCodexRealtimeExecutionSessionID(sessionID); err != nil {
		return nil, codexRealtimeInvalidSessionIDError(err)
	}

	plan, identity, responsesLite, errWithCode := p.prepareResponsesWSOfficialConn(ctx, req, normalizedModel, sessionID)
	if errWithCode != nil {
		return nil, errWithCode
	}
	conn, errWithCode := p.dialChatRealtimeConnWithContext(ctx, plan)
	if errWithCode != nil {
		return nil, errWithCode
	}

	adapter := &codexResponsesWSAdapter{
		provider:                  p,
		model:                     normalizedModel,
		identity:                  identity,
		defaultPreviousResponseID: strings.TrimSpace(req.PreviousResponseID),
		responsesLite:             responsesLite,
	}
	return responsesws.NewNativeSession(conn, adapter, responsesws.NativeSessionOptions{
		Context:      ctx,
		Diagnostics:  req.Diagnostics,
		ProviderName: "codex",
		ChannelID:    req.ChannelID,
		Transport:    string(runtimesession.TransportModeResponsesWS),
	}), nil
}

func (p *CodexProvider) prepareResponsesWSOfficialConn(ctx context.Context, req *responsesws.OpenRequest, normalizedModel, sessionID string) (*codexRealtimeConnPlan, wire.Identity, bool, *types.OpenAIErrorWithStatusCode) {
	urlPath, errWithCode := p.GetSupportedAPIUri(config.RelayModeChatRealtime)
	if errWithCode != nil {
		return nil, wire.Identity{}, false, errWithCode
	}
	httpURL := p.GetFullRequestURL(urlPath, normalizedModel)
	proxyAddr := channelProxyValue(p.codexChannel())
	allowSelfHosted := p.codexResponsesWSSelfHosted()
	wsURL, err := buildCodexRealtimeURLWithPolicy(httpURL, allowSelfHosted, proxyAddr == "")
	if err != nil {
		return nil, wire.Identity{}, false, common.StringErrorWrapperLocal(err.Error(), "ws_request_failed", requester.UpstreamRealtimeURLStatusCode(err))
	}
	metadata, err := wire.MetadataFromResponsesFrame(req.FirstFrame)
	if err != nil {
		return nil, wire.Identity{}, false, codexWireError(err)
	}
	policy, err := p.codexOfficialChannelPolicy()
	if err != nil {
		return nil, wire.Identity{}, false, common.ErrorWrapperLocal(err, "channel_config_error", http.StatusServiceUnavailable)
	}
	token, err := p.GetToken()
	if err != nil {
		return nil, wire.Identity{}, false, p.handleTokenError(err)
	}
	identity, decisions, err := wire.ResolveIdentity(wire.IdentityInput{
		Operation: wire.OpResponsesWSOpen,
		Headers:   req.InboundHeaders,
		Metadata:  metadata,
		Policy:    policy,
		Principal: p.codexPrincipalFingerprint(req.Principal),
		ChannelID: req.ChannelID,
		Clock:     wire.RealClock{},
	})
	if err != nil {
		return nil, wire.Identity{}, false, codexWireError(err)
	}
	plan, err := wire.BuildHeaders(wire.HeaderPlanInput{
		Operation: wire.OpResponsesWSOpen,
		Headers:   req.InboundHeaders,
		Credential: wire.Credential{
			AccessToken: token,
			AccountID:   p.codexAccountID(),
		},
		Policy:   policy,
		Identity: identity,
	})
	if err != nil {
		return nil, wire.Identity{}, false, codexWireError(err)
	}
	plan.Decisions = append(decisions, plan.Decisions...)
	p.auditCodexOfficialHeaderPlan(ctx, wire.OpResponsesWSOpen, req.ChannelID, plan.Decisions)
	return &codexRealtimeConnPlan{
		wsURL:           wsURL,
		headers:         plan.Map(),
		allowSelfHosted: allowSelfHosted,
		proxyAddr:       proxyAddr,
	}, identity, policy.ResponsesLite || identity.ResponsesLite == "true", nil
}

func (a *codexResponsesWSAdapter) PrepareClientFrame(_ context.Context, frame responsesws.Frame) (responsesws.Frame, error) {
	if frame.Kind() != responsesws.FrameKindText {
		return responsesws.Frame{}, newCodexRealtimeClientError("", "unsupported_client_event", "only text websocket events are supported")
	}
	payload := frame.Payload()
	envelope, err := responsesws.ParseClientEventEnvelope(payload)
	if err != nil {
		logCodexRealtimeInternalError("codex responses websocket client event decode failed: " + err.Error())
		return responsesws.Frame{}, newCodexRealtimeClientError("", "invalid_event", codexRealtimeStaticErrorMessage("invalid_event"))
	}

	switch strings.TrimSpace(envelope.Type) {
	case "response.create":
		return a.prepareResponseCreate(payload)
	case "response.cancel":
		return frame, nil
	default:
		return responsesws.Frame{}, newCodexRealtimeClientError(envelope.EventID, "unsupported_client_event", "unsupported responses websocket client event")
	}
}

func (a *codexResponsesWSAdapter) prepareResponseCreate(payload []byte) (responsesws.Frame, error) {
	if a == nil || a.provider == nil {
		return responsesws.Frame{}, responsesws.ErrUpstreamClosed
	}
	parsed, err := responsesws.ParseRawResponsesCreateFrame(payload)
	if err != nil {
		return responsesws.Frame{}, err
	}
	encodedPayload, err := wire.PlanResponsesWSFrame(parsed, wire.FramePatchInput{
		Identity:                  a.identity,
		Model:                     a.model,
		DefaultPreviousResponseID: a.defaultPreviousResponseID,
		ResponsesLite:             a.responsesLite,
		Clock:                     wire.RealClock{},
	})
	if err != nil {
		return responsesws.Frame{}, err
	}
	request := parsed.Projection
	request.Model = a.model
	accumulator := newCodexTurnUsageAccumulator()
	accumulator.SeedPromptFromRequest(&request, a.provider.codexPreCost())

	a.mu.Lock()
	a.lastResponse = ""
	a.accumulator = accumulator
	a.mu.Unlock()

	return responsesws.NewTextFrame(encodedPayload), nil
}

func (a *codexResponsesWSAdapter) HandleProviderFrame(_ context.Context, frame responsesws.Frame) responsesws.ProviderFrameResult {
	if a == nil || a.provider == nil {
		return codexResponsesWSProviderMalformed(responsesws.ErrUpstreamClosed)
	}
	if frame.Kind() != responsesws.FrameKindText {
		return codexResponsesWSProviderMalformed(responsesws.ErrNativeProtocol)
	}
	payload := frame.Payload()
	if isCodexRealtimeBootstrapPayload(payload) {
		return responsesws.ProviderFrameResult{
			Filtered: true,
			Origin:   responsesws.RecvDetailOriginProviderFrame,
		}
	}
	if _, err := responsesws.ParseProviderEventEnvelope(payload); err != nil {
		return codexResponsesWSProviderMalformed(err)
	}
	if classified := responsesws.ClassifyResponsesWSEvent(payload); classified.Malformed {
		return codexResponsesWSProviderMalformed(fmt.Errorf("%w: %s", responsesws.ErrInvalidProviderEventPayload, classified.MalformedError))
	}

	shouldContinue, usage, handlerErr := a.handleProviderPayloadLocked(&payload)

	if handlerErr != nil {
		return codexResponsesWSProviderMalformed(handlerErr)
	}
	if !shouldContinue {
		return responsesws.ProviderFrameResult{
			Filtered: true,
			Origin:   responsesws.RecvDetailOriginProviderFrame,
		}
	}
	out := responsesws.NewTextFrame(payload)
	return responsesws.ProviderFrameResult{
		EmitFrame: &out,
		Usage:     usage,
		Origin:    responsesws.RecvDetailOriginProviderFrame,
	}
}

func (a *codexResponsesWSAdapter) handleProviderPayloadLocked(payload *[]byte) (bool, *types.UsageEvent, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	accumulator := a.accumulator
	modelName := a.model
	shouldContinue, usage, rewritten, handlerErr := a.provider.handleRealtimeSupplierPayload(*payload, accumulator, modelName)
	if len(rewritten) > 0 {
		*payload = rewritten
	}
	terminal, lastResponseID, _ := inspectCodexRealtimeSupplierPayload(*payload)
	if lastResponseID != "" {
		a.lastResponse = lastResponseID
	}
	if terminal {
		a.accumulator = nil
	}
	return shouldContinue, usage, handlerErr
}

func codexResponsesWSProviderMalformed(err error) responsesws.ProviderFrameResult {
	if err == nil {
		err = responsesws.ErrNativeProtocol
	}
	return responsesws.ProviderFrameResult{
		Origin:         responsesws.RecvDetailOriginProviderMalformed,
		Err:            err,
		CloseTransport: true,
	}
}

func (a *codexResponsesWSAdapter) MapProviderClose(_ context.Context, info responsesws.ProviderCloseInfo) responsesws.ProviderCloseResult {
	if a != nil {
		a.mu.Lock()
		a.accumulator = nil
		a.mu.Unlock()
	}
	if codexResponsesWSNativeProviderCloseInfo(info) {
		return responsesws.ProviderCloseResult{
			ProviderClose: &responsesws.ProviderClose{Code: info.Code, Reason: info.Reason, Err: info.Err},
			Origin:        responsesws.RecvDetailOriginNativeProviderClose,
		}
	}
	return responsesws.ProviderCloseResult{}
}

func codexResponsesWSNativeProviderCloseInfo(info responsesws.ProviderCloseInfo) bool {
	return info.Kind == responsesws.ProviderCloseKindPeerClose
}

var _ responsesws.ProviderAdapter = (*codexResponsesWSAdapter)(nil)
