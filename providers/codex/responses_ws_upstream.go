package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/common/responsesws"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/google/uuid"
)

type codexResponsesWSAdapter struct {
	provider *CodexProvider
	model    string

	mu           sync.Mutex
	lastResponse string
	accumulator  *codexTurnUsageAccumulator
}

var codexResponsesWSBridgeBlockedHeaders = []string{
	"session_id",
	"session-id",
	"x-session-id",
	"x-codex-turn-metadata",
	"x-codex-turn-state",
}

func (p *CodexProvider) OpenResponsesWS(ctx context.Context, modelName string, options responsesws.OpenOptions) (responsesws.Upstream, *types.OpenAIErrorWithStatusCode) {
	normalizedModel := normalizeCodexModelName(modelName)
	switch options.Transport {
	case "", runtimesession.TransportModeResponsesWS:
	case runtimesession.TransportModeResponsesHTTPBridge:
		if !p.supportsHTTPBridgeResponsesWSTransport() {
			return nil, common.StringErrorWrapperLocal("channel does not support Responses websocket transport", "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired)
		}
		return responsesws.NewBridgeSession(codexResponsesWSBridgeOpener{
			provider: p,
			model:    normalizedModel,
		}, responsesws.BridgeSessionOptions{
			Context:                   ctx,
			Diagnostics:               options.Diagnostics,
			ProviderName:              "codex",
			ChannelID:                 options.ChannelID,
			Transport:                 string(runtimesession.TransportModeResponsesHTTPBridge),
			InitialPreviousResponseID: options.PreviousResponseID,
			OpenTimeout:               config.ResponsesWSBridgeOpenTimeout(),
			MaxStreamEventBytes:       config.RealtimeWebsocketReadLimit(),
		}), nil
	default:
		return nil, common.StringErrorWrapperLocal("invalid responses websocket transport", "invalid_transport", http.StatusBadRequest)
	}
	if p.getWebsocketMode() == codexWebsocketModeOff {
		return nil, common.StringErrorWrapperLocal("channel does not support Responses websocket transport", "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired)
	}
	sessionID := strings.TrimSpace(options.UpstreamSessionID)
	if sessionID == "" {
		sessionID = "responses-ws:" + uuid.NewString()
	}
	if err := validateCodexRealtimeExecutionSessionID(sessionID); err != nil {
		return nil, codexRealtimeInvalidSessionIDError(err)
	}

	plan, errWithCode := p.prepareChatRealtimeConnWithSelfHosted(normalizedModel, sessionID, p.codexResponsesWSSelfHosted())
	if errWithCode != nil {
		return nil, errWithCode
	}
	conn, errWithCode := p.dialChatRealtimeConnWithContext(ctx, plan)
	if errWithCode != nil {
		return nil, errWithCode
	}

	adapter := &codexResponsesWSAdapter{provider: p, model: normalizedModel}
	return responsesws.NewNativeSession(conn, adapter, responsesws.NativeSessionOptions{
		Diagnostics:  options.Diagnostics,
		ProviderName: "codex",
		ChannelID:    options.ChannelID,
		Transport:    string(runtimesession.TransportModeResponsesWS),
	}), nil
}

func (p *CodexProvider) supportsHTTPBridgeResponsesWSTransport() bool {
	return p != nil && strings.TrimSpace(p.Config.Responses) != ""
}

type codexResponsesWSBridgeOpener struct {
	provider *CodexProvider
	model    string
}

func (o codexResponsesWSBridgeOpener) OpenBridgeStream(ctx context.Context, bridgeReq responsesws.BridgeStreamRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode, error) {
	frame := bridgeReq.Frame
	if frame.Kind() != responsesws.FrameKindText {
		return nil, nil, responsesws.ErrInvalidFrame
	}
	framePayload := frame.Payload()
	if previousResponseID := strings.TrimSpace(bridgeReq.DefaultPreviousResponseID); previousResponseID != "" {
		parsed, err := responsesws.ParseRawResponsesCreateFrame(framePayload)
		if err != nil {
			return nil, nil, err
		}
		framePayload, err = parsed.CloneWithDefaultPreviousResponseID(previousResponseID)
		if err != nil {
			return nil, nil, err
		}
	}
	_, request, encodedPayload, err := o.provider.prepareCodexRealtimeCreatePayload(framePayload, o.model)
	if err != nil {
		return nil, nil, err
	}
	if err := o.provider.validateResponsesWSHTTPBridgeURL(ctx, o.provider.responsesWSBridgeRequestURL(request.Model)); err != nil {
		apiErr := common.StringErrorWrapperLocal(err.Error(), "ws_request_failed", requester.UpstreamResponsesHTTPURLStatusCode(err))
		return nil, nil, responsesws.NewOpenAIErrorWithCause(apiErr, err)
	}
	parsedPrepared, err := responsesws.ParseRawResponsesCreateFrame(encodedPayload)
	if err != nil {
		return nil, nil, err
	}
	body, err := responsesws.BuildResponsesHTTPBridgeBody(parsedPrepared.Object, request.Model, request.PreviousResponseID)
	if err != nil {
		return nil, nil, err
	}
	request.Stream = true
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, nil, common.ErrorWrapper(err, "marshal_request_failed", http.StatusInternalServerError)
	}
	req, errWithCode := o.provider.getResponsesWSBridgeRequest(request)
	if errWithCode != nil {
		return nil, nil, errWithCode
	}
	req.Body = io.NopCloser(bytes.NewReader(payload))
	req.ContentLength = int64(len(payload))
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	resp, errWithCode := o.provider.Requester.SendResponsesHTTPBridgeRaw(req, o.provider.responsesHTTPBridgeSecurity())
	if errWithCode != nil {
		return nil, responsesws.MarkHTTPBridgeTransportError(errWithCode), nil
	}
	handler := newCodexResponsesStreamHandler(o.provider.Usage)
	stream, streamErr := requester.RequestNoTrimStreamWithEmitterOptions(o.provider.Requester, resp, handler.HandlerResponsesStreamWithEmitter, requester.StreamReadOptions{
		MaxLineBytes: config.RealtimeWebsocketReadLimit(),
	})
	if streamErr != nil {
		return nil, streamErr, nil
	}
	return stream, nil, nil
}

func (p *CodexProvider) createResponsesBridgeStreamRaw(ctx context.Context, request *types.OpenAIResponsesRequest, body map[string]json.RawMessage) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode) {
	if request == nil {
		return nil, common.StringErrorWrapperLocal("request is required", "invalid_request_error", http.StatusBadRequest)
	}
	if err := p.validateResponsesWSHTTPBridgeURL(ctx, p.responsesWSBridgeRequestURL(request.Model)); err != nil {
		return nil, common.StringErrorWrapperLocal(err.Error(), "ws_request_failed", requester.UpstreamResponsesHTTPURLStatusCode(err))
	}
	request.Stream = true
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, markCodexHTTPBridgePreSendLocalError(common.ErrorWrapper(err, "marshal_request_failed", http.StatusInternalServerError))
	}
	req, errWithCode := p.getResponsesWSBridgeRequest(request)
	if errWithCode != nil {
		return nil, markCodexHTTPBridgePreSendLocalError(errWithCode)
	}
	req.Body = io.NopCloser(bytes.NewReader(payload))
	req.ContentLength = int64(len(payload))
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	resp, errWithCode := p.Requester.SendResponsesHTTPBridgeRaw(req, p.responsesHTTPBridgeSecurity())
	if errWithCode != nil {
		return nil, responsesws.MarkHTTPBridgeTransportError(errWithCode)
	}
	handler := newCodexResponsesStreamHandler(p.Usage)
	return requester.RequestNoTrimStreamWithEmitterOptions(p.Requester, resp, handler.HandlerResponsesStreamWithEmitter, requester.StreamReadOptions{
		MaxLineBytes: config.RealtimeWebsocketReadLimit(),
	})
}

func markCodexHTTPBridgePreSendLocalError(errWithStatus *types.OpenAIErrorWithStatusCode) *types.OpenAIErrorWithStatusCode {
	if errWithStatus != nil {
		errWithStatus.LocalError = true
	}
	return errWithStatus
}

func (p *CodexProvider) getResponsesWSBridgeRequest(request *types.OpenAIResponsesRequest) (*http.Request, *types.OpenAIErrorWithStatusCode) {
	ensureStablePromptCacheKey(request, p.Context, p.getPromptCacheKeyStrategy())

	fullRequestURL := p.responsesWSBridgeRequestURL(request.Model)

	headers, err := p.getRequestHeaderBag()
	if err != nil {
		return nil, p.handleTokenError(err)
	}
	// ResponsesWS bridge is stateless with respect to Codex execution sessions:
	// client session headers may still exist in the downstream request snapshot,
	// but they must not be forwarded or used to bind/resume a turn/session.
	sanitizeCodexResponsesWSBridgeHeaders(headers)

	if strings.TrimSpace(request.PromptCacheKey) != "" {
		headers.Set("Conversation_id", request.PromptCacheKey)
	}
	p.applyDefaultHeaders(headers)
	sanitizeCodexResponsesWSBridgeHeaders(headers)

	if request.Stream {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}

	req, err := p.Requester.NewRequest(http.MethodPost, fullRequestURL, p.Requester.WithBody(request), p.Requester.WithHeader(headers.Map()))
	if err != nil {
		return nil, common.ErrorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	return req, nil
}

func (p *CodexProvider) responsesWSBridgeRequestURL(model string) string {
	if p == nil {
		return ""
	}
	requestPath := strings.TrimRight(p.Config.Responses, "/")
	return p.GetFullRequestURL(requestPath, model)
}

func (p *CodexProvider) validateResponsesWSHTTPBridgeURL(ctx context.Context, rawURL string) error {
	_, err := requester.ValidateAndResolveUpstreamResponsesHTTPURL(ctx, rawURL, p.responsesHTTPBridgeSecurity())
	return err
}

func (p *CodexProvider) responsesHTTPBridgeSecurity() requester.ResponsesHTTPBridgeSecurity {
	proxyAddr := ""
	if p != nil {
		proxyAddr = channelProxyValue(p.Channel)
	}
	return requester.ResponsesHTTPBridgeSecurity{
		AllowSelfHosted: p.codexResponsesWSSelfHosted(),
		ProxyAddr:       proxyAddr,
	}
}

func sanitizeCodexResponsesWSBridgeHeaders(headers *codexHeaderBag) {
	if headers == nil {
		return
	}
	for _, key := range codexResponsesWSBridgeBlockedHeaders {
		headers.Delete(key)
	}
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
	_, request, encodedPayload, err := a.provider.prepareCodexRealtimeCreatePayload(payload, a.model)
	if err != nil {
		return responsesws.Frame{}, err
	}
	accumulator := newCodexTurnUsageAccumulator()
	preCost := 0
	if a.provider.Channel != nil {
		preCost = a.provider.Channel.PreCost
	}
	accumulator.SeedPromptFromRequest(request, preCost)

	a.mu.Lock()
	a.lastResponse = ""
	a.accumulator = accumulator
	a.mu.Unlock()

	return responsesws.NewTextFrame(encodedPayload), nil
}

func (a *codexResponsesWSAdapter) HandleProviderFrame(_ context.Context, frame responsesws.Frame) responsesws.ProviderFrameResult {
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

	a.mu.Lock()
	accumulator := a.accumulator
	modelName := a.model
	shouldContinue, usage, rewritten, handlerErr := a.provider.handleRealtimeSupplierPayload(payload, accumulator, modelName)
	if len(rewritten) > 0 {
		payload = rewritten
	}
	terminal, lastResponseID, _ := inspectCodexRealtimeSupplierPayload(payload)
	if lastResponseID != "" {
		a.lastResponse = lastResponseID
	}
	if terminal {
		a.accumulator = nil
	}
	a.mu.Unlock()

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
