package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/providerresponse"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/common/responsesws"
	"one-api/providers/codex/wire"
	"one-api/types"

	"github.com/google/uuid"
)

type codexResponsesWSAdapter struct {
	credentials          []string
	provider             *CodexProvider
	model                string
	identity             wire.Identity
	responsesLite        bool
	autoStampStreamStart bool

	mu        sync.Mutex
	responses map[string]*codexTurnUsageAccumulator
}

const codexResponsesWSMaxTrackedResponses = 64

type codexResponsesWSOfficialOpenPlan struct {
	conn                 *codexRealtimeConnPlan
	identity             wire.Identity
	responsesLite        bool
	autoStampStreamStart bool
}

func (p *CodexProvider) OpenResponsesWS(ctx context.Context, req *responsesws.OpenRequest) (responsesws.Upstream, *types.OpenAIErrorWithStatusCode) {
	if req == nil {
		return nil, common.StringErrorWrapperLocal("responses websocket open request is required", "invalid_request_error", http.StatusBadRequest)
	}
	if req.FirstFrame == nil {
		return nil, common.StringErrorWrapperLocal("first response.create frame is required", "invalid_request_error", http.StatusBadRequest)
	}
	normalizedModel := strings.TrimSpace(req.SelectedModel)
	if normalizedModel == "" {
		normalizedModel = strings.TrimSpace(req.FirstFrame.Projection.Model)
	}
	sessionID := strings.TrimSpace(req.UpstreamSessionID)
	if sessionID == "" {
		sessionID = "responses-ws:" + uuid.NewString()
	}
	if err := validateCodexRealtimeExecutionSessionID(sessionID); err != nil {
		return nil, codexRealtimeInvalidSessionIDError(err)
	}

	openPlan, errWithCode := p.prepareResponsesWSOfficialConn(ctx, req, normalizedModel, sessionID)
	if errWithCode != nil {
		return nil, errWithCode
	}
	conn, errWithCode := p.dialChatRealtimeConnWithContext(ctx, openPlan.conn)
	if errWithCode != nil {
		return nil, errWithCode
	}

	adapter := &codexResponsesWSAdapter{
		credentials:          providerresponse.ConnectionCredentials(openPlan.conn.wsURL, codexRealtimeHTTPHeader(openPlan.conn.headers)),
		provider:             p,
		model:                normalizedModel,
		identity:             openPlan.identity,
		responsesLite:        openPlan.responsesLite,
		autoStampStreamStart: openPlan.autoStampStreamStart,
	}
	return responsesws.NewNativeSession(conn, adapter, responsesws.NativeSessionOptions{
		Credentials:  providerresponse.ConnectionCredentials(openPlan.conn.wsURL, codexRealtimeHTTPHeader(openPlan.conn.headers)),
		Context:      ctx,
		Diagnostics:  req.Diagnostics,
		ProviderName: "codex",
		ChannelID:    req.ChannelID,
		Transport:    "responses-ws",
	}), nil
}

func (p *CodexProvider) prepareResponsesWSOfficialConn(ctx context.Context, req *responsesws.OpenRequest, normalizedModel, sessionID string) (*codexResponsesWSOfficialOpenPlan, *types.OpenAIErrorWithStatusCode) {
	safeRouteRetry := !p.codexOpenMayMutateCredentials()
	urlPath, errWithCode := p.GetSupportedAPIUri(config.RelayModeChatRealtime)
	if errWithCode != nil {
		return nil, errWithCode
	}
	httpURL := p.GetFullRequestURL(urlPath, normalizedModel)
	proxyAddr := channelProxyValue(p.codexChannel())
	allowSelfHosted := p.codexResponsesWSSelfHosted()
	wsURL, err := buildCodexRealtimeURLWithPolicy(httpURL, allowSelfHosted, proxyAddr == "")
	if err != nil {
		return nil, common.StringErrorWrapperLocal(err.Error(), "ws_request_failed", requester.UpstreamRealtimeURLStatusCode(err))
	}
	metadata, err := wire.MetadataFromResponsesFrame(req.FirstFrame)
	if err != nil {
		return nil, codexWireError(err)
	}
	policy, err := p.codexOfficialChannelPolicy()
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "channel_config_error", http.StatusServiceUnavailable)
	}
	principal := wire.PrincipalFingerprint{}
	if policy.AutoGenerate.InstallationID {
		principal, err = p.codexPrincipalFingerprint(req.Principal)
		if err != nil {
			return nil, common.ErrorWrapperLocal(err, "channel_config_error", http.StatusServiceUnavailable)
		}
	}
	identity, decisions, err := wire.ResolveIdentity(wire.IdentityInput{
		Operation: wire.OpResponsesWSOpen,
		Headers:   req.InboundHeaders,
		Metadata:  metadata,
		Policy:    policy,
		Principal: principal,
		ChannelID: req.ChannelID,
		Clock:     wire.RealClock{},
	})
	if err != nil {
		return nil, codexWireError(err)
	}
	token, err := p.GetToken()
	if err != nil {
		return nil, p.handleTokenError(err)
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
		return nil, codexWireError(err)
	}
	plan.Decisions = append(decisions, plan.Decisions...)
	p.auditCodexOfficialHeaderPlan(ctx, wire.OpResponsesWSOpen, req.ChannelID, plan.Decisions)
	return &codexResponsesWSOfficialOpenPlan{
		conn: &codexRealtimeConnPlan{
			wsURL:           wsURL,
			headers:         plan.Map(),
			allowSelfHosted: allowSelfHosted,
			proxyAddr:       proxyAddr,
			safeRouteRetry:  safeRouteRetry,
		},
		identity:             identity,
		responsesLite:        identity.ResponsesLite == "true",
		autoStampStreamStart: policy.AutoGenerate.WSStreamRequestStartMS,
	}, nil
}

func (a *codexResponsesWSAdapter) PrepareClientFrame(ctx context.Context, frame responsesws.Frame) (responsesws.Frame, error) {
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
		return a.prepareResponseCreate(ctx, payload)
	case "response.inject", "response.steer":
		return frame, nil
	default:
		return responsesws.Frame{}, newCodexRealtimeClientError(envelope.EventID, "unsupported_client_event", "unsupported responses websocket client event")
	}
}

func (a *codexResponsesWSAdapter) prepareResponseCreate(ctx context.Context, payload []byte) (responsesws.Frame, error) {
	if a == nil || a.provider == nil {
		return responsesws.Frame{}, responsesws.ErrUpstreamClosed
	}
	parsed, err := responsesws.ParseRawResponsesCreateFrame(payload)
	if err != nil {
		return responsesws.Frame{}, err
	}
	request := parsed.Projection
	turnModel := strings.TrimSpace(request.Model)
	if turnModel == "" {
		turnModel = a.model
	}
	encodedPayload, err := wire.PlanResponsesWSFrame(parsed, wire.FramePatchInput{
		Identity:                           a.identity,
		Model:                              turnModel,
		ResponsesLite:                      a.responsesLite,
		AutoGenerateWSStreamRequestStartMS: a.autoStampStreamStart,
		Clock:                              wire.RealClock{},
	})
	if err != nil {
		return responsesws.Frame{}, err
	}

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
	if isCodexSupplierBootstrapPayload(payload) {
		return responsesws.ProviderFrameResult{
			Filtered: true,
			Origin:   responsesws.RecvDetailOriginProviderFrame,
		}
	}
	envelope, err := responsesws.ParseProviderEventEnvelope(payload)
	if err != nil {
		return codexResponsesWSProviderMalformed(err)
	}

	shouldContinue, normalized, usage, handlerErr := a.handleProviderPayloadLocked(payload, envelope)

	if handlerErr != nil {
		return codexResponsesWSProviderMalformedWithUsage(handlerErr, usage)
	}
	if !shouldContinue {
		return responsesws.ProviderFrameResult{
			Filtered: true,
			Origin:   responsesws.RecvDetailOriginProviderFrame,
		}
	}
	out := responsesws.NewTextFrame(normalized)
	return responsesws.ProviderFrameResult{
		EmitFrame: &out,
		Usage:     usage,
		Origin:    responsesws.RecvDetailOriginProviderFrame,
	}
}

func (a *codexResponsesWSAdapter) handleProviderPayloadLocked(payload []byte, envelope *responsesws.ProviderEventEnvelope) (bool, []byte, *types.UsageEvent, error) {
	if responsesws.IsAuxiliaryControlEvent(envelope.Type) {
		return true, payload, nil, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	normalized, err := a.normalizeProviderPayloadLocked(payload, envelope)
	if err != nil {
		return false, nil, a.supplierTerminalUsageLocked(payload, envelope), err
	}
	event, tracked := commonresponses.ParseStreamUsageEvent(normalized)
	if !tracked {
		return true, normalized, nil, nil
	}
	responseID := responsesws.ProviderResponseID(normalized)
	if responseID == "" || len(responseID) > 1024 {
		return true, normalized, nil, nil
	}
	accumulator := a.responses[responseID]
	terminal := commonresponses.IsTerminalEventType(event.Type)
	if accumulator == nil {
		switch {
		case event.Type == "response.created":
			if len(a.responses) >= codexResponsesWSMaxTrackedResponses {
				return true, normalized, nil, nil
			}
			if a.responses == nil {
				a.responses = make(map[string]*codexTurnUsageAccumulator)
			}
			accumulator = newCodexTurnUsageAccumulator()
			a.responses[responseID] = accumulator
		case terminal:
			// 终态自身的有身份用量可独立提取；去重和 work 归属由 relay 负责。
			accumulator = newCodexTurnUsageAccumulator()
		default:
			return true, normalized, nil, nil
		}
	}
	// 观察器只影响对应计费证据，不改变原始交付。
	accumulator.observeEventWithoutDeliveryGate(&types.OpenAIResponsesStreamResponses{Type: event.Type, Item: event.Item, ItemID: event.ItemID, OutputIndex: event.OutputIndex, PartialImageIndex: event.PartialImageIndex, Response: event.Response})
	var usage *types.UsageEvent
	if terminal {
		usage = accumulator.ResolveUsageEvent(event.Response)
		delete(a.responses, responseID)
	} else {
		usage = accumulator.BillingUsageEvent()
	}
	if usage != nil {
		usage.ResponseID = responseID
		usage.ProviderEventID = envelope.EventID
	}
	return true, normalized, usage, nil
}

// Codex's websocket supplier dialect has several private terminal aliases.
// Interpret them before the public classifier, then render the equivalent
// Responses lifecycle event without changing already-valid public terminals.
func (a *codexResponsesWSAdapter) normalizeProviderPayloadLocked(payload []byte, envelope *responsesws.ProviderEventEnvelope) ([]byte, error) {
	if envelope == nil || strings.TrimSpace(envelope.Type) == types.EventTypeError {
		return append([]byte(nil), payload...), nil
	}

	if isCodexPublicResponsesTerminal(envelope.Type) {
		return payload, nil
	}
	switch envelope.Type {
	case types.EventTypeResponseDone, "response.updated", "response.cancelled", "response.canceled":
		// 仅转换已知 supplier 方言；未知扩展不由 status 推导事件类型。
	default:
		return payload, nil
	}
	object := envelope.Object
	rawResponse, exists := object["response"]
	var responseObject map[string]json.RawMessage
	var response types.OpenAIResponsesResponses
	if exists {
		if json.Unmarshal(rawResponse, &responseObject) != nil || responseObject == nil || response.DecodeCapturedProviderJSON(rawResponse) != nil {
			if _, terminal := interpretCodexSupplierTerminal(envelope.Type, nil); terminal {
				return nil, fmt.Errorf("%w: supplier terminal response is invalid", responsesws.ErrInvalidProviderEventPayload)
			}
			return append([]byte(nil), payload...), nil
		}
	}
	evidence, terminal := interpretCodexSupplierTerminal(envelope.Type, &response)
	if !terminal {
		return append([]byte(nil), payload...), nil
	}
	if !exists || responseObject == nil || strings.TrimSpace(response.ID) == "" {
		// 缺少身份的私有回执不能借用最近响应建立关联。
		return payload, nil
	}
	if evidence.kind == codexSupplierCancelledTerminal {
		return nil, fmt.Errorf("%w: supplier cancellation cannot be represented as a public Responses terminal", responsesws.ErrInvalidProviderEventPayload)
	}
	encodedID, err := json.Marshal(response.ID)
	if err != nil {
		return nil, err
	}
	encodedStatus, err := json.Marshal(evidence.responseStatus)
	if err != nil {
		return nil, err
	}
	responseObject["id"] = encodedID
	responseObject["status"] = encodedStatus
	encodedResponse, err := json.Marshal(responseObject)
	if err != nil {
		return nil, err
	}
	object["response"] = encodedResponse
	encodedType, err := json.Marshal(evidence.publicEventType)
	if err != nil {
		return nil, err
	}
	object["type"] = encodedType

	return json.Marshal(object)
}

func isCodexPublicResponsesTerminal(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.completed", "response.failed", "response.incomplete":
		return true
	default:
		return false
	}
}

func (a *codexResponsesWSAdapter) supplierTerminalUsageLocked(payload []byte, envelope *responsesws.ProviderEventEnvelope) *types.UsageEvent {
	if a == nil || envelope == nil || responsesws.ProviderResponseID(payload) == "" {
		return nil
	}
	var response types.OpenAIResponsesResponses
	if response.DecodeCapturedProviderJSON(envelope.Object["response"]) != nil || strings.TrimSpace(response.ID) == "" {
		return nil
	}
	eventType := strings.TrimSpace(envelope.Type)
	if !classifyCodexSupplierTerminal(eventType, &response, eventType == types.EventTypeError).isTerminal() && eventType != types.EventTypeResponseDone {
		return nil
	}
	accumulator := a.responses[response.ID]
	if accumulator == nil {
		accumulator = newCodexTurnUsageAccumulator()
	}
	accumulator.observeEventWithoutDeliveryGate(&types.OpenAIResponsesStreamResponses{Type: eventType, Response: &response})
	usage := accumulator.ResolveUsageEvent(&response)
	delete(a.responses, response.ID)
	if usage != nil {
		usage.ResponseID = response.ID
		usage.ProviderEventID = envelope.EventID
	}
	return usage
}

func codexResponsesWSProviderMalformed(err error) responsesws.ProviderFrameResult {
	return codexResponsesWSProviderMalformedWithUsage(err, nil)
}

func codexResponsesWSProviderMalformedWithUsage(err error, usage *types.UsageEvent) responsesws.ProviderFrameResult {
	if err == nil {
		err = responsesws.ErrNativeProtocol
	}
	return responsesws.ProviderFrameResult{
		Origin:         responsesws.RecvDetailOriginProviderMalformed,
		Usage:          usage,
		Err:            err,
		CloseTransport: true,
	}
}

func (a *codexResponsesWSAdapter) MapProviderClose(_ context.Context, info responsesws.ProviderCloseInfo) responsesws.ProviderCloseResult {
	if a != nil {
		a.mu.Lock()
		a.responses = nil
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
