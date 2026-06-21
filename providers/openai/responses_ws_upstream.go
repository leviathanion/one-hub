package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/common/responsesws"
	runtimesession "one-api/runtime/session"
	"one-api/types"
)

type openAIResponsesWSAdapter struct{}

func (p *OpenAIProvider) OpenResponsesWS(ctx context.Context, modelName string, options responsesws.OpenOptions) (responsesws.Upstream, *types.OpenAIErrorWithStatusCode) {
	if p == nil {
		return nil, common.StringErrorWrapperLocal("provider is required", "ws_request_failed", http.StatusInternalServerError)
	}
	switch options.Transport {
	case "", runtimesession.TransportModeResponsesWS:
		if !p.supportsNativeResponsesWSTransport() {
			return nil, responsesWSUnsupportedForChannel()
		}
	case runtimesession.TransportModeResponsesHTTPBridge:
		if !p.supportsHTTPBridgeResponsesWSTransport() {
			return nil, responsesWSUnsupportedForChannel()
		}
		return responsesws.NewBridgeSession(openAIResponsesWSBridgeOpener{
			provider: p,
			model:    modelName,
		}, responsesws.BridgeSessionOptions{
			Context:                   ctx,
			Diagnostics:               options.Diagnostics,
			ProviderName:              "openai",
			ChannelID:                 options.ChannelID,
			Transport:                 string(runtimesession.TransportModeResponsesHTTPBridge),
			InitialPreviousResponseID: options.PreviousResponseID,
			OpenTimeout:               config.ResponsesWSBridgeOpenTimeout(),
			MaxStreamEventBytes:       config.RealtimeWebsocketReadLimit(),
		}), nil
	default:
		return nil, common.StringErrorWrapperLocal("invalid responses websocket transport", "invalid_transport", http.StatusBadRequest)
	}
	conn, errWithCode := p.openResponsesWSConnWithContext(ctx, modelName)
	if errWithCode != nil {
		return nil, errWithCode
	}
	return responsesws.NewNativeSession(conn, openAIResponsesWSAdapter{}, responsesws.NativeSessionOptions{
		Diagnostics:  options.Diagnostics,
		ProviderName: "openai",
		ChannelID:    options.ChannelID,
		Transport:    string(runtimesession.TransportModeResponsesWS),
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
	if p.usesOfficialOpenAIBaseURL() {
		return true
	}
	if p.responsesWSNativeExplicitlyEnabled() {
		return true
	}
	return false
}

func (p *OpenAIProvider) supportsHTTPBridgeResponsesWSTransport() bool {
	return p != nil && strings.TrimSpace(p.Config.Responses) != ""
}

func (p *OpenAIProvider) responsesWSNativeExplicitlyEnabled() bool {
	if p == nil || p.Channel == nil {
		return false
	}
	other, err := p.Channel.GetOtherMap()
	if err != nil {
		return false
	}
	raw, ok := other["responses_ws_native"]
	if !ok || len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return false
	}
	var enabled bool
	if err := json.Unmarshal(raw, &enabled); err != nil {
		return false
	}
	return enabled
}

func (p *OpenAIProvider) usesOfficialOpenAIBaseURL() bool {
	if p == nil {
		return false
	}
	baseURL := strings.TrimRight(strings.ToLower(strings.TrimSpace(p.GetBaseURL())), "/")
	return baseURL == "https://api.openai.com" || strings.HasPrefix(baseURL, "https://api.openai.com/")
}

type openAIResponsesWSBridgeOpener struct {
	provider *OpenAIProvider
	model    string
}

func (o openAIResponsesWSBridgeOpener) OpenBridgeStream(ctx context.Context, bridgeReq responsesws.BridgeStreamRequest) (requester.StreamReaderInterface[string], *types.OpenAIErrorWithStatusCode, error) {
	frame := bridgeReq.Frame
	if frame.Kind() != responsesws.FrameKindText {
		return nil, nil, responsesws.ErrInvalidFrame
	}
	parsed, err := responsesws.ParseRawResponsesCreateFrame(frame.Payload())
	if err != nil {
		return nil, nil, err
	}
	request := parsed.Projection
	if strings.TrimSpace(request.Model) == "" {
		request.Model = o.model
	}
	if _, exists := parsed.Object["previous_response_id"]; !exists && strings.TrimSpace(request.PreviousResponseID) == "" {
		if previousResponseID := strings.TrimSpace(bridgeReq.DefaultPreviousResponseID); previousResponseID != "" {
			request.PreviousResponseID = previousResponseID
		}
	}
	request.Stream = true
	fullRequestURL, errWithCode := o.provider.responsesHTTPBridgeRequestURL(request.Model)
	if errWithCode != nil {
		return nil, nil, errWithCode
	}
	if err := o.provider.validateResponsesWSHTTPBridgeURL(ctx, fullRequestURL); err != nil {
		apiErr := common.StringErrorWrapperLocal(err.Error(), "ws_request_failed", requester.UpstreamResponsesHTTPURLStatusCode(err))
		return nil, nil, responsesws.NewOpenAIErrorWithCause(apiErr, err)
	}
	body, err := responsesws.BuildResponsesHTTPBridgeBody(parsed.Object, request.Model, request.PreviousResponseID)
	if err != nil {
		return nil, nil, err
	}
	req, errWithCode := o.provider.buildResponsesHTTPBridgeRequest(body, fullRequestURL, o.provider.requestHeaders(openAIRequestAuthBearer), request.Model)
	if errWithCode != nil {
		return nil, nil, errWithCode
	}
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	defer req.Body.Close()
	stream, errWithCode := o.provider.createResponsesHTTPBridgeStreamFromRequestWithOptions(req, &request, responsesHTTPBridgeStreamReadOptions())
	if errWithCode != nil {
		return nil, responsesws.MarkHTTPBridgeTransportError(errWithCode), nil
	}
	return stream, nil, nil
}

func (p *OpenAIProvider) validateResponsesWSHTTPBridgeURL(ctx context.Context, rawURL string) error {
	_, err := requester.ValidateAndResolveUpstreamResponsesHTTPURL(ctx, rawURL, p.responsesHTTPBridgeSecurity())
	return err
}

func (p *OpenAIProvider) responsesHTTPBridgeSecurity() requester.ResponsesHTTPBridgeSecurity {
	proxyAddr := ""
	if p != nil && p.Channel != nil && p.Channel.Proxy != nil {
		proxyAddr = *p.Channel.Proxy
	}
	return requester.ResponsesHTTPBridgeSecurity{
		AllowSelfHosted: openAIResponsesWSSelfHosted(p),
		ProxyAddr:       proxyAddr,
	}
}

func (a openAIResponsesWSAdapter) PrepareClientFrame(_ context.Context, frame responsesws.Frame) (responsesws.Frame, error) {
	if frame.Kind() != responsesws.FrameKindText {
		return responsesws.Frame{}, responsesws.ErrInvalidFrame
	}
	if _, err := responsesws.ParseClientEventEnvelope(frame.Payload()); err != nil {
		return responsesws.Frame{}, err
	}
	return frame, nil
}

func (a openAIResponsesWSAdapter) HandleProviderFrame(_ context.Context, frame responsesws.Frame) responsesws.ProviderFrameResult {
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
	if classified := responsesws.ClassifyResponsesWSEvent(payload); classified.Malformed {
		return responsesws.ProviderFrameResult{
			Origin:         responsesws.RecvDetailOriginProviderMalformed,
			Err:            fmt.Errorf("%w: %s", responsesws.ErrInvalidProviderEventPayload, classified.MalformedError),
			CloseTransport: true,
		}
	}

	out := responsesws.NewTextFrame(append([]byte(nil), payload...))
	return responsesws.ProviderFrameResult{
		EmitFrame: &out,
		Usage:     openAIResponsesWSEventUsage(eventType, envelope.EventID, envelope.Object, payload).Clone(),
		Origin:    responsesws.RecvDetailOriginProviderFrame,
	}
}

func openAIResponsesWSEventUsage(eventType string, providerEventID string, object map[string]json.RawMessage, payload []byte) *types.UsageEvent {
	if object != nil {
		if rawResponse, ok := object["response"]; ok && len(rawResponse) > 0 {
			var response types.ResponseEvent
			// Provider events are passthrough-first. Decode known usage evidence
			// best-effort so future event shapes do not make native ResponsesWS
			// stricter than the HTTP bridge transport.
			if err := json.Unmarshal(rawResponse, &response); err == nil {
				if usage := openAIRealtimeResponseUsage(providerEventID, &response); usage != nil {
					return usage
				}
			}
		}
	}
	return openAIRealtimeInputAudioTranscriptionUsage(eventType, providerEventID, payload)
}

func (a openAIResponsesWSAdapter) MapProviderClose(_ context.Context, info responsesws.ProviderCloseInfo) responsesws.ProviderCloseResult {
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

var _ responsesws.ProviderAdapter = openAIResponsesWSAdapter{}
