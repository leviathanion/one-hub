package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"one-api/common"
	"one-api/common/authutil"
	"one-api/common/config"
	"one-api/common/logger"
	realtimeprotocol "one-api/common/realtime"
	"one-api/common/requestctx"
	"one-api/common/requester"
	"one-api/common/wsconn"
	runtimerealtime "one-api/runtime/realtime"
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const openAIRealtimeDetachGraceTimeout = 30 * time.Second
const openAIRealtimeFinalizedResponseIDLimit = openAIRealtimeResponseIDLimit

var openAIRealtimeOutboundBackpressureTimeout = 5 * time.Second

type openAIRealtimeOutbound struct {
	messageType   wsconn.MessageType
	payload       []byte
	providerClose *runtimerealtime.ProviderClose
	usage         *types.UsageEvent
	origin        runtimerealtime.RealtimePayloadOrigin
	err           error
	credit        *runtimerealtime.ByteCredit
}

func (o *openAIRealtimeOutbound) release() {
	if o == nil {
		return
	}
	if o.credit != nil {
		o.credit.Release()
		o.credit = nil
	}
}

type openAIRealtimeProviderFrame struct {
	messageType wsconn.MessageType
	payload     []byte
	credit      *runtimerealtime.ByteCredit
}

func (f *openAIRealtimeProviderFrame) release() {
	if f == nil {
		return
	}
	if f.credit != nil {
		f.credit.Release()
		f.credit = nil
	}
}

type openAIRealtimeFinalizedTurn struct {
	observer runtimesession.TurnObserver
	payload  runtimesession.TurnFinalizePayload
}

type openAIRealtimeTurnSelection struct {
	state           *openAIRealtimeTurnState
	dropAttribution bool
}

type openAIRealtimeSession struct {
	provider    *OpenAIProvider
	model       string
	models      runtimesession.ModelBinding
	workPolicy  runtimesession.RealtimeWorkPolicy
	actualModel string
	sessionID   string
	conn        *wsconn.ManagedConn
	pumpCtx     context.Context
	compatMode  bool

	recvCh                      chan openAIRealtimeOutbound
	outboundBudget              *runtimerealtime.ByteBudget
	outboundBackpressureTimeout time.Duration
	closed                      chan struct{}
	detached                    chan struct{}
	closeOnce                   sync.Once
	detachOnce                  sync.Once
	detachLog                   sync.Once
	writeMu                     sync.Mutex
	detachTimer                 *time.Timer
	detachMu                    sync.Mutex
	readLoopOnce                sync.Once
	readLoopDone                chan struct{}
	mu                          sync.Mutex
	detachReason                string

	pendingSettings        []*openAIRealtimeSettingsUpdate
	usedSettingsRequestIDs []string
	usedSettingsAckIDs     []string
	inputSeq               int64
	inputWorks             []*openAIRealtimeInputWork
	audioWork              *openAIRealtimeInputWork
	manualResponseInput    *openAIRealtimeInputWork
	transcriptionModels    *runtimesession.ModelBinding
	manualAudioCommit      bool
	futureWorkErr          error
	recentInputResults     []openAIRealtimeInputResult
	pendingInputLimit      int
	recentInputLimit       int

	turnSeq                   int64
	turn                      *openAIRealtimeTurnState
	recentFinalizedIDs        []string
	turnObserverFactory       runtimesession.TurnObserverFactory
	automaticFeaturesDisabled bool
}

func (p *OpenAIProvider) OpenRealtimeSession(modelName string) (runtimerealtime.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
	return p.OpenRealtimeSessionWithOptions(modelName, runtimerealtime.RealtimeOpenOptions{})
}

func (p *OpenAIProvider) OpenRealtimeSessionWithOptions(modelName string, options runtimerealtime.RealtimeOpenOptions) (runtimerealtime.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
	conn, errWithCode := p.openRealtimeConnWithContext(options.Context, modelName)
	if errWithCode != nil {
		return nil, errWithCode
	}

	sessionID, sessionIDErr := readOpenAIRealtimeSessionID(p)
	if sessionIDErr != nil {
		conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "invalid_session_id"})
		return nil, sessionIDErr
	}
	session := &openAIRealtimeSession{
		provider:                    p,
		models:                      options.Models,
		workPolicy:                  options.WorkPolicy,
		model:                       strings.TrimSpace(modelName),
		sessionID:                   sessionID,
		conn:                        conn,
		pumpCtx:                     openAIRealtimePumpContext(options.Context),
		compatMode:                  config.OpenAIRealtimeSessionCompatMode,
		recvCh:                      make(chan openAIRealtimeOutbound, 128),
		outboundBudget:              runtimerealtime.NewByteBudget(config.RealtimeWebsocketAttachmentQueueMaxBytes()),
		outboundBackpressureTimeout: openAIRealtimeOutboundBackpressureTimeout,
		closed:                      make(chan struct{}),
		detached:                    make(chan struct{}),
	}
	if session.models.RequestedModel == "" {
		session.models = runtimesession.ModelBinding{RequestedModel: modelName, ProviderModel: modelName, BillingModel: modelName}
	}
	return session, nil
}

func (p *OpenAIProvider) openRealtimeConn(modelName string) (*wsconn.ManagedConn, *types.OpenAIErrorWithStatusCode) {
	return p.openRealtimeConnWithContext(context.Background(), modelName)
}

func openAIRealtimePumpContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

func (p *OpenAIProvider) openRealtimeConnWithContext(ctx context.Context, modelName string) (*wsconn.ManagedConn, *types.OpenAIErrorWithStatusCode) {
	fullRequestURL, errWithCode := p.realtimeWSURL(modelName)
	if errWithCode != nil {
		return nil, errWithCode
	}

	proxyAddr := ""
	if p.Channel != nil && p.Channel.Proxy != nil {
		proxyAddr = *p.Channel.Proxy
	}
	authMode := openAIRequestAuthBearer
	if p.IsAzure && p.Channel != nil && p.Channel.Type != config.ChannelTypeAzureV1 {
		authMode = openAIRequestAuthAzureAPIKey
	}
	headers := p.requestHeaders(authMode)
	inbound := requestctx.HeaderSnapshot{}
	if p.Context != nil && p.Context.Request != nil {
		inbound = requestctx.NewHeaderSnapshot(p.Context.Request.Header)
	}
	if err := applyRealtimeBusinessHeaders(headers, inbound); err != nil {
		return nil, common.ErrorWrapperLocal(err, "invalid_request_header", http.StatusBadRequest)
	}
	httpHeaders := httpHeaderFromOpenAIHeaders(headers)

	dialCtx, cancel := openAIRealtimeDialContext(ctx)
	defer cancel()
	wsConn, err := wsconn.DialManaged(dialCtx, fullRequestURL, httpHeaders, openAIRealtimeWSConfig("openai realtime upstream"), openAIRealtimeDialOptions(proxyAddr, openAIRealtimeSelfHosted(p), openAIUpstreamWebsocketSubprotocols(p))...)
	if err != nil {
		return nil, mapOpenAIRealtimeWSDialError(err)
	}
	return wsConn, nil
}

func (p *OpenAIProvider) realtimeWSURL(modelName string) (string, *types.OpenAIErrorWithStatusCode) {
	apiPath, errWithCode := p.GetSupportedAPIUri(config.RelayModeChatRealtime)
	if errWithCode != nil {
		return "", errWithCode
	}

	if p.IsAzure {
		if p.Channel != nil && p.Channel.Type == config.ChannelTypeAzureV1 {
			return p.azureV1RealtimeWSURL(modelName)
		}
		return p.azurePreviewRealtimeWSURL(modelName)
	}

	rawURL := p.GetFullRequestURL(apiPath, "")
	withModel, err := appendOpenAIRealtimeModelQuery(rawURL, modelName)
	if err != nil {
		return "", common.ErrorWrapperLocal(err, "ws_request_failed", http.StatusInternalServerError)
	}
	return p.realtimeWSURLFromHTTP(withModel)
}

func (p *OpenAIProvider) azureV1RealtimeWSURL(modelName string) (string, *types.OpenAIErrorWithStatusCode) {
	baseURL := strings.TrimRight(strings.TrimSpace(p.GetBaseURL()), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", common.ErrorWrapperLocal(err, "ws_request_failed", http.StatusInternalServerError)
	}
	if strings.Contains(parsed.Path, "/openai/deployments/") {
		return "", common.StringErrorWrapperLocal("Azure v1 Realtime requires a resource-level base URL, not a deployment path", "invalid_azure_realtime_base_url", http.StatusBadRequest)
	}
	parsed.Path = azureV1ResourceEndpointPath(parsed.Path, "/v1/realtime")
	query := parsed.Query()
	if strings.TrimSpace(modelName) != "" {
		query.Set("model", strings.TrimSpace(modelName))
	}
	parsed.RawQuery = query.Encode()
	return p.realtimeWSURLFromHTTP(parsed.String())
}

func (p *OpenAIProvider) azurePreviewRealtimeWSURL(modelName string) (string, *types.OpenAIErrorWithStatusCode) {
	baseURL := strings.TrimRight(strings.TrimSpace(p.GetBaseURL()), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", common.ErrorWrapperLocal(err, "ws_request_failed", http.StatusInternalServerError)
	}
	if strings.Contains(parsed.Path, "/openai/deployments/") {
		return "", common.StringErrorWrapperLocal("Azure Realtime requires a resource-level base URL, not a deployment path", "invalid_azure_realtime_base_url", http.StatusBadRequest)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/openai/realtime"
	query := parsed.Query()
	apiVersion, apiVersionErr := p.azureClassicAPIVersion()
	if apiVersionErr != nil {
		return "", apiVersionErr
	}
	query.Set("api-version", apiVersion)
	if strings.TrimSpace(modelName) != "" {
		query.Set("deployment", strings.TrimSpace(modelName))
	}
	parsed.RawQuery = query.Encode()
	return p.realtimeWSURLFromHTTP(parsed.String())
}

func (p *OpenAIProvider) azureClassicAPIVersion() (string, *types.OpenAIErrorWithStatusCode) {
	if p == nil || p.Channel == nil {
		return "", common.StringErrorWrapperLocal("Azure api_version is required in channel Other JSON", "invalid_azure_api_version", http.StatusBadRequest)
	}
	apiVersion, err := p.Channel.GetAzureAPIVersion()
	if err != nil {
		return "", common.StringErrorWrapperLocal("Azure channel Other must be JSON with a non-empty api_version", "invalid_azure_api_version", http.StatusBadRequest)
	}
	if strings.TrimSpace(apiVersion) == "" {
		return "", common.StringErrorWrapperLocal("Azure api_version is required in channel Other JSON", "invalid_azure_api_version", http.StatusBadRequest)
	}
	return strings.TrimSpace(apiVersion), nil
}

func (p *OpenAIProvider) realtimeWSURLFromHTTP(rawURL string) (string, *types.OpenAIErrorWithStatusCode) {
	proxyAddr := ""
	if p != nil && p.Channel != nil && p.Channel.Proxy != nil {
		proxyAddr = *p.Channel.Proxy
	}
	validated, err := requester.ValidateUpstreamRealtimeURL(rawURL, requester.UpstreamRealtimeURLPolicy{
		AllowSelfHosted: openAIRealtimeSelfHosted(p),
		ResolveHost:     proxyAddr == "",
	})
	if err != nil {
		return "", common.StringErrorWrapperLocal(err.Error(), "ws_request_failed", requester.UpstreamRealtimeURLStatusCode(err))
	}
	return validated, nil
}

func appendOpenAIRealtimeModelQuery(rawURL string, modelName string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	if strings.TrimSpace(modelName) != "" {
		query.Set("model", strings.TrimSpace(modelName))
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func (p *OpenAIProvider) openResponsesWSConn(modelName string) (*wsconn.ManagedConn, *types.OpenAIErrorWithStatusCode) {
	return p.openResponsesWSConnWithContext(context.Background(), modelName)
}

func (p *OpenAIProvider) openResponsesWSConnWithContext(ctx context.Context, modelName string) (*wsconn.ManagedConn, *types.OpenAIErrorWithStatusCode) {
	return p.openResponsesWSConnWithHeaders(ctx, modelName, requestctx.HeaderSnapshot{})
}

func (p *OpenAIProvider) openResponsesWSConnWithHeaders(ctx context.Context, modelName string, inbound requestctx.HeaderSnapshot) (*wsconn.ManagedConn, *types.OpenAIErrorWithStatusCode) {
	fullRequestURL, errWithCode := p.responsesWSURL(modelName)
	if errWithCode != nil {
		return nil, errWithCode
	}

	headers := p.requestHeaders(openAIRequestAuthBearer)
	if err := applyResponsesBusinessHeaders(headers, inbound); err != nil {
		return nil, common.ErrorWrapperLocal(err, "invalid_request_header", http.StatusBadRequest)
	}
	httpHeaders := httpHeaderFromOpenAIHeaders(headers)

	proxyAddr := ""
	if p.Channel != nil && p.Channel.Proxy != nil {
		proxyAddr = *p.Channel.Proxy
	}
	dialCtx, cancel := openAIRealtimeDialContext(ctx)
	defer cancel()
	wsConn, err := wsconn.DialManaged(dialCtx, fullRequestURL, httpHeaders, openAIRealtimeWSConfig("openai responses websocket upstream"), openAIRealtimeDialOptions(proxyAddr, openAIResponsesWSSelfHosted(p), openAIUpstreamWebsocketSubprotocols(p))...)
	if err != nil {
		return nil, mapOpenAIResponsesWSDialError(err)
	}
	return wsConn, nil
}

func openAIRealtimeDialContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, config.ConnectTimeout())
}

func openAIRealtimeWSConfig(label string) wsconn.Config {
	inboundActivityTimeout := config.RealtimeWebsocketClientInboundActivityTimeout()
	writeTimeout := config.RealtimeWebsocketWriteTimeout()
	return wsconn.Config{
		Label:           label,
		PingInterval:    config.RealtimeWebsocketPingInterval(),
		PongMissTimeout: config.RealtimeWebsocketClientPongMissTimeout(),
		InboundActivityTimeout: func() time.Duration {
			return inboundActivityTimeout
		},
		ReadLimit:    config.RealtimeWebsocketReadLimit(),
		WriteTimeout: func() time.Duration { return writeTimeout },
	}
}

func openAIRealtimeDialOptions(proxyAddr string, allowSelfHosted bool, subprotocols []string) []wsconn.DialOption {
	policy := wsconn.DialSecurityPolicy{
		AllowInsecureWS: allowSelfHosted,
		AllowPrivateIP:  allowSelfHosted,
	}
	options := []wsconn.DialOption{
		wsconn.WithHandshakeTimeout(config.ConnectTimeout()),
		wsconn.WithSubprotocols(subprotocols...),
		wsconn.WithDialSecurityPolicy(policy),
	}
	if strings.TrimSpace(proxyAddr) != "" {
		options = append(options, wsconn.WithProxyURL(proxyAddr))
	}
	return options
}

func httpHeaderFromOpenAIHeaders(headers map[string]string) http.Header {
	httpHeaders := make(http.Header, len(headers))
	for key, value := range headers {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		httpHeaders.Set(key, value)
	}
	return httpHeaders
}

func (p *OpenAIProvider) responsesWSURL(modelName string) (string, *types.OpenAIErrorWithStatusCode) {
	if p == nil {
		return "", common.StringErrorWrapperLocal("provider is required", "ws_request_failed", http.StatusInternalServerError)
	}
	if p.IsAzure {
		if p.Channel == nil || p.Channel.Type != config.ChannelTypeAzureV1 {
			if _, errWithCode := p.azureClassicAPIVersion(); errWithCode != nil {
				return "", errWithCode
			}
		}
		return p.azureResponsesWSURL()
	}

	apiPath, apiErr := p.GetSupportedAPIUri(config.RelayModeResponses)
	if apiErr != nil {
		return "", apiErr
	}
	return p.responsesWSURLFromHTTP(p.GetFullRequestURL(apiPath, modelName))
}

func (p *OpenAIProvider) azureResponsesWSURL() (string, *types.OpenAIErrorWithStatusCode) {
	baseURL := strings.TrimRight(strings.TrimSpace(p.GetBaseURL()), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", common.ErrorWrapperLocal(err, "ws_request_failed", http.StatusInternalServerError)
	}
	if strings.Contains(parsed.Path, "/openai/deployments/") {
		return "", common.StringErrorWrapperLocal("Azure ResponsesWS requires a resource-level base URL, not a deployment path", "invalid_azure_responses_ws_base_url", http.StatusBadRequest)
	}
	parsed.Path = azureV1ResourceEndpointPath(parsed.Path, "/v1/responses")
	parsed.RawQuery = ""
	return p.responsesWSURLFromHTTP(parsed.String())
}

func (p *OpenAIProvider) azureV1ResponsesWSURL() (string, *types.OpenAIErrorWithStatusCode) {
	if p == nil {
		return "", common.StringErrorWrapperLocal("provider is required", "ws_request_failed", http.StatusInternalServerError)
	}
	return p.azureResponsesWSURL()
}

func (p *OpenAIProvider) responsesWSURLFromHTTP(rawURL string) (string, *types.OpenAIErrorWithStatusCode) {
	proxyAddr := ""
	if p != nil && p.Channel != nil && p.Channel.Proxy != nil {
		proxyAddr = *p.Channel.Proxy
	}
	return responsesWSURLFromHTTPWithPolicyAndResolve(rawURL, openAIResponsesWSSelfHosted(p), proxyAddr == "")
}

func responsesWSURLFromHTTPWithPolicyAndResolve(rawURL string, allowSelfHosted bool, resolveHost bool) (string, *types.OpenAIErrorWithStatusCode) {
	validated, err := requester.ValidateUpstreamRealtimeURL(rawURL, requester.UpstreamRealtimeURLPolicy{
		AllowSelfHosted: allowSelfHosted,
		ResolveHost:     resolveHost,
	})
	if err != nil {
		return "", common.StringErrorWrapperLocal(err.Error(), "ws_request_failed", requester.UpstreamRealtimeURLStatusCode(err))
	}
	return validated, nil
}

func openAIResponsesWSSelfHosted(p *OpenAIProvider) bool {
	if p == nil || p.Channel == nil {
		return false
	}
	enabled, _ := p.Channel.GetOtherBoolField("responses_ws_self_hosted")
	return enabled
}

func openAIRealtimeSelfHosted(p *OpenAIProvider) bool {
	if p == nil {
		return false
	}
	if p.Context != nil && p.Context.GetBool("self_hosted") {
		return true
	}
	if p.Channel == nil {
		return false
	}
	other, err := p.Channel.GetOtherMap()
	if err != nil {
		return false
	}
	return rawJSONBool(other["self_hosted"])
}

func rawJSONBool(raw json.RawMessage) bool {
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	return value
}

func openAIUpstreamWebsocketSubprotocols(p *OpenAIProvider) []string {
	if p == nil || p.Context == nil || p.Context.Request == nil {
		return nil
	}
	return authutil.AllowedOpenAIUpstreamWebsocketSubprotocols(p.Context.Request)
}

func mapOpenAIResponsesWSDialError(err error) *types.OpenAIErrorWithStatusCode {
	var dialErr *wsconn.DialError
	if errors.As(err, &dialErr) && dialErr != nil {
		return markOpenAIRealtimeOpenNotAttempted(mapOpenAIResponsesWSDialStatus(dialErr.StatusCode))
	}
	logOpenAIRealtimeInternalError("openai responses websocket dial failed: " + openAIResponsesWSDialErrorSummary(err))
	return markOpenAIRealtimeOpenNotAttempted(common.StringErrorWrapperLocal("websocket request failed", "ws_request_failed", http.StatusInternalServerError))
}

func openAIResponsesWSDialErrorSummary(err error) string {
	if err == nil {
		return "class=<nil> dial_error=false category=nil"
	}
	category := "other"
	switch {
	case errors.Is(err, context.Canceled):
		category = "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		category = "context_deadline_exceeded"
	}
	return fmt.Sprintf("class=%T dial_error=false category=%s", err, category)
}

func mapOpenAIResponsesWSDialStatus(statusCode int) *types.OpenAIErrorWithStatusCode {
	switch statusCode {
	case http.StatusNotFound, http.StatusUpgradeRequired:
		return common.StringErrorWrapperLocal("channel does not support Responses websocket transport", "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired)
	case http.StatusUnauthorized, http.StatusForbidden:
		return common.StringErrorWrapperLocal("provider authentication failed", "provider_authentication_failed", statusCode)
	case http.StatusTooManyRequests:
		return common.StringErrorWrapperLocal("provider rate limit exceeded", "provider_rate_limit_exceeded", http.StatusTooManyRequests)
	default:
		if statusCode >= 500 {
			return common.StringErrorWrapperLocal("provider websocket request failed", "provider_ws_request_failed", statusCode)
		}
	}
	return common.StringErrorWrapperLocal("websocket request failed", "ws_request_failed", http.StatusInternalServerError)
}

func mapOpenAIRealtimeWSDialError(err error) *types.OpenAIErrorWithStatusCode {
	var dialErr *wsconn.DialError
	if errors.As(err, &dialErr) && dialErr != nil {
		return markOpenAIRealtimeOpenNotAttempted(mapOpenAIRealtimeWSDialStatus(dialErr.StatusCode))
	}
	logOpenAIRealtimeInternalError("openai realtime websocket dial failed: " + err.Error())
	return markOpenAIRealtimeOpenNotAttempted(common.StringErrorWrapperLocal("websocket request failed", "ws_request_failed", http.StatusInternalServerError))
}

func markOpenAIRealtimeOpenNotAttempted(apiErr *types.OpenAIErrorWithStatusCode) *types.OpenAIErrorWithStatusCode {
	if apiErr != nil {
		apiErr.LocalError = false
		apiErr.UpstreamNotAttempted = true
		apiErr.ProviderOpenRetrySafe = true
	}
	return apiErr
}

func mapOpenAIRealtimeWSDialStatus(statusCode int) *types.OpenAIErrorWithStatusCode {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return common.StringErrorWrapperLocal("provider authentication failed", "provider_authentication_failed", statusCode)
	case http.StatusTooManyRequests:
		return common.StringErrorWrapperLocal("provider rate limit exceeded", "provider_rate_limit_exceeded", http.StatusTooManyRequests)
	default:
		if statusCode >= 500 {
			return common.StringErrorWrapperLocal("provider websocket request failed", "provider_ws_request_failed", statusCode)
		}
	}
	return common.StringErrorWrapperLocal("websocket request failed", "ws_request_failed", http.StatusInternalServerError)
}

func readOpenAIRealtimeSessionID(p *OpenAIProvider) (string, *types.OpenAIErrorWithStatusCode) {
	if p != nil && p.Context != nil && p.Context.Request != nil {
		if sessionID := runtimesession.ReadClientSessionID(p.Context.Request); sessionID != "" {
			if err := runtimesession.ValidateClientSessionID(sessionID); err != nil {
				logOpenAIRealtimeInternalError("invalid realtime session id rejected: " + err.Error())
				return "", common.StringErrorWrapperLocal("invalid realtime session id", "invalid_session_id", http.StatusBadRequest)
			}
			return sessionID, nil
		}
	}
	return uuid.NewString(), nil
}

func (s *openAIRealtimeSession) SendClient(ctx context.Context, frame runtimerealtime.Frame) error {
	if s == nil || s.conn == nil || s.isDetached() {
		return runtimerealtime.ErrSessionClosed
	}
	mt, payload, err := openAIRealtimeMessageFromFrame(frame)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return runtimerealtime.ErrSessionClosed
	default:
	}
	var envelope struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(payload, &envelope)
	eventType := strings.TrimSpace(envelope.Type)
	switch eventType {
	case "response.create", "input_audio_buffer.append", "input_audio_buffer.commit", "conversation.item.create":
		if err := s.waitInputSettings(ctx); err != nil {
			return err
		}
	}
	payload, settings, err := s.prepareInputSettings(payload, eventType)
	if err != nil {
		return openAIRealtimeClientPayloadErrorFromObserver(err)
	}
	normalizedPayload, eventType, err := normalizeOpenAIRealtimeClientPayload(payload, mt, s.model, s.compatMode)
	if err != nil {
		return err
	}
	var wireEventID string
	inputWork, newInput, err := s.prepareInputWork(eventType, normalizedPayload)
	if err != nil {
		return openAIRealtimeClientPayloadErrorFromObserver(err)
	}
	if inputWork != nil && (newInput || eventType == "input_audio_buffer.commit") {
		var eventID string
		normalizedPayload, eventID, _, err = ensureOpenAIRealtimeClientEventID(normalizedPayload)
		if err != nil || strings.TrimSpace(eventID) == "" || len(eventID) > openAIRealtimeIdentifierMaxBytes {
			if eventType == "input_audio_buffer.commit" {
				s.rollbackManualCommitCandidate(inputWork)
			}
			if newInput {
				s.discardInput(inputWork, "invalid_input_event", false)
			}
			return newOpenAIRealtimeClientError("invalid_event", "invalid realtime input event_id")
		}
		wireEventID = eventID
	}
	var startedTurn *openAIRealtimeTurnState
	if eventType == "response.create" {
		if err := s.checkFutureWork(s.modelBinding(), false); err != nil {
			return openAIRealtimeClientPayloadErrorFromObserver(err)
		}
		var clientEventID string
		var injected bool
		normalizedPayload, clientEventID, injected, err = ensureOpenAIRealtimeClientEventID(normalizedPayload)
		if err != nil {
			return newOpenAIRealtimeClientError("invalid_event", "failed to correlate realtime response.create")
		}
		if strings.TrimSpace(clientEventID) == "" || len(strings.TrimSpace(clientEventID)) > openAIRealtimeIdentifierMaxBytes {
			return newOpenAIRealtimeClientError("invalid_event", "realtime event_id exceeds the supported limit")
		}
		wireEventID = clientEventID
		startedTurn, err = s.startTurnWithClientEventID(clientEventID, injected)
		if err != nil {
			return err
		}
		admission := openAIRealtimeBoundedTurnAdmission(normalizedPayload, s.modelBinding(), s.automaticFeaturesDisabledSnapshot())
		admission.WorkID = fmt.Sprintf("response-%d", startedTurn.seq)
		admission.SessionID = s.sessionID
		if err := runtimesession.AdmitBoundedTurn(startedTurn.observer, admission); err != nil {
			s.rollbackOpenAIRealtimeTurnAdmission(startedTurn, "turn_admission_failed")
			s.rollbackTurn(startedTurn)
			return openAIRealtimeClientPayloadErrorFromObserver(err)
		}
	}
	var settingsUpdate *openAIRealtimeSettingsUpdate
	var settingsEventID string
	if settings != nil {
		normalizedPayload, settingsEventID, _, err = ensureOpenAIRealtimeClientEventID(normalizedPayload)
		if err != nil || strings.TrimSpace(settingsEventID) == "" || len(settingsEventID) > openAIRealtimeIdentifierMaxBytes {
			return newOpenAIRealtimeClientError("invalid_event", "invalid realtime configuration event_id")
		}
		wireEventID = settingsEventID
	}
	if wireEventID == "" {
		wireEventID = openAIRealtimeClientEventID(normalizedPayload)
	}
	s.writeMu.Lock()
	select {
	case <-ctx.Done():
		s.writeMu.Unlock()
		s.resolveInputSettings(settingsUpdate, false)
		if eventType == "input_audio_buffer.commit" {
			s.rollbackManualCommitCandidate(inputWork)
		}
		if startedTurn != nil {
			s.rollbackOpenAIRealtimeTurnAdmission(startedTurn, "context_cancelled_before_write")
			s.rollbackTurn(startedTurn)
		}
		if newInput {
			s.discardInput(inputWork, "context_cancelled_before_write", false)
		}
		return runtimerealtime.ErrSessionClosed
	default:
	}
	if settings != nil {
		settingsUpdate = s.beginInputSettings(settings, settingsEventID, eventType)
		if settingsUpdate == nil {
			s.writeMu.Unlock()
			return realtimeSettingsBeginError(s, settingsEventID)
		}
	} else if wireEventID != "" {
		s.mu.Lock()
		settingsConflict := s.hasUsedInputSettingsRequestIDLocked(wireEventID)
		s.mu.Unlock()
		if settingsConflict {
			s.writeMu.Unlock()
			if eventType == "input_audio_buffer.commit" {
				s.rollbackManualCommitCandidate(inputWork)
			}
			if startedTurn != nil {
				s.rollbackOpenAIRealtimeTurnAdmission(startedTurn, "client_event_id_reused")
				s.rollbackTurn(startedTurn)
			}
			if inputWork != nil && newInput {
				s.discardInput(inputWork, "client_event_id_reused", false)
			}
			return newOpenAIRealtimeClientError("realtime_event_id_reused", "client event_id was already used by a realtime configuration update")
		}
	}
	if inputWork != nil {
		s.mu.Lock()
		manualCommitOwnsBuffer := eventType == "input_audio_buffer.commit" && s.audioWork == inputWork
		for _, part := range s.submissionPartsLocked(inputWork) {
			part.clientEventID = wireEventID
			part.submitted = true
			if eventType == "input_audio_buffer.commit" {
				part.manualCommitCandidate = manualCommitOwnsBuffer
				part.manualCommitEventID = wireEventID
			}
		}
		s.mu.Unlock()
	}
	if eventType == "response.create" {
		s.bindManualResponseInput()
	}
	writeResult := s.conn.WriteMessageResult(wsconn.MessageType(mt), normalizedPayload)
	var rollbackAdmission bool
	if writeResult.Err != nil {
		rollbackAdmission = s.resolveOpenAIRealtimeWriteFailure(startedTurn, writeResult.Attempted)
	}
	s.writeMu.Unlock()
	if writeResult.Err != nil {
		s.resolveInputSettings(settingsUpdate, false)
		if rollbackAdmission {
			s.rollbackOpenAIRealtimeTurnAdmission(startedTurn, "write_failed_before_attempt")
		}
		if inputWork != nil && newInput && !writeResult.Attempted {
			s.discardInput(inputWork, "ws_write_failed", false)
		}
		if eventType == "input_audio_buffer.commit" && !writeResult.Attempted {
			s.rollbackManualCommitCandidate(inputWork)
		}
		if eventType == "response.create" && !writeResult.Attempted {
			s.clearManualResponseInput()
		}
		s.Abort("ws_write_failed")
		logOpenAIRealtimeInternalError("openai realtime websocket write failed: " + writeResult.Err.Error())
		return types.NewErrorEvent("", "system_error", "ws_write_failed", "upstream websocket write failed")
	}
	s.inputWriteFinished(inputWork, eventType)
	if eventType == "input_audio_buffer.clear" {
		s.mu.Lock()
		work := s.audioWork
		s.mu.Unlock()
		s.discardInput(work, "input_audio_buffer.clear", true)
	}
	return nil
}

// A response.create has no item_id, so it may only resolve an input when the
// preceding client commit established one unambiguous candidate. This keeps a
// provider VAD response from being claimed by an unrelated explicit response.
func (s *openAIRealtimeSession) bindManualResponseInput() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manualResponseInput != nil {
		return
	}
	var candidate *openAIRealtimeInputWork
	for _, work := range s.inputWorks {
		if work == nil || work.responseClaimed || !work.automaticResponse || work.automaticSourceSeen || !work.manualCommitSent {
			continue
		}
		if candidate != nil {
			return
		}
		candidate = work
	}
	s.manualResponseInput = candidate
}

func (s *openAIRealtimeSession) clearManualResponseInput() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.manualResponseInput = nil
	s.mu.Unlock()
}

func (s *openAIRealtimeSession) resolveManualResponseInput() {
	if s == nil {
		return
	}
	s.mu.Lock()
	work := s.manualResponseInput
	s.manualResponseInput = nil
	if work != nil && !work.automaticSourceSeen {
		s.markManualInputSourceLocked(work)
		s.pruneInputWorksLocked()
	}
	s.mu.Unlock()
}

func (s *openAIRealtimeSession) automaticFeaturesDisabledSnapshot() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	disabled := s.automaticFeaturesDisabled
	s.mu.Unlock()
	return disabled
}

func openAIRealtimeAutomaticFeaturesDisabled(payload []byte) (bool, bool) {
	var event map[string]json.RawMessage
	if json.Unmarshal(payload, &event) != nil || (rawJSONString(event["type"]) != "session.update" && rawJSONString(event["type"]) != "transcription_session.update") {
		return false, false
	}
	var session map[string]json.RawMessage
	if json.Unmarshal(event["session"], &session) != nil {
		return false, false
	}
	turnDetection, present := session["turn_detection"]
	if !present {
		var audio struct {
			Input struct {
				TurnDetection json.RawMessage `json:"turn_detection"`
			} `json:"input"`
		}
		if json.Unmarshal(session["audio"], &audio) == nil && len(audio.Input.TurnDetection) > 0 {
			turnDetection = audio.Input.TurnDetection
			present = true
		}
	}
	if !present {
		return false, false
	}
	if bytes.Equal(bytes.TrimSpace(turnDetection), []byte("null")) {
		return true, true
	}
	var config struct {
		CreateResponse *bool `json:"create_response"`
	}
	if json.Unmarshal(turnDetection, &config) != nil {
		return false, true
	}
	return config.CreateResponse != nil && !*config.CreateResponse, true
}

func openAIRealtimeBoundedTurnAdmission(payload []byte, models runtimesession.ModelBinding, automaticFeaturesDisabled bool) runtimesession.TurnAdmission {
	admission := runtimesession.TurnAdmission{
		ExplicitClientCreate:      true,
		AutomaticFeaturesDisabled: automaticFeaturesDisabled,
		Models:                    models,
		PromptTokens:              int64(len(payload)) + 1024,
	}
	var event struct {
		Response struct {
			Model           string          `json:"model"`
			MaxOutputTokens json.RawMessage `json:"max_output_tokens"`
			ServiceTier     string          `json:"service_tier"`
			Tools           []struct {
				Type string `json:"type"`
			} `json:"tools"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &event) != nil {
		admission.UnknownChargeDimensions = true
		return admission
	}
	admission.ServiceTier = strings.TrimSpace(event.Response.ServiceTier)
	if len(event.Response.MaxOutputTokens) > 0 {
		if json.Unmarshal(event.Response.MaxOutputTokens, &admission.MaxOutputTokens) != nil || admission.MaxOutputTokens < 0 {
			admission.UnknownChargeDimensions = true
			admission.MaxOutputTokens = 0
		}
	}
	for _, tool := range event.Response.Tools {
		if strings.TrimSpace(tool.Type) != "function" {
			admission.UnknownChargeDimensions = true
			break
		}
	}
	return admission
}

func (s *openAIRealtimeSession) Recv(ctx context.Context) (runtimerealtime.RecvEvent, error) {
	s.startReadLoop()
	if event, err, handled := s.recvQueuedOutbound(); handled {
		return event, err
	}

	select {
	case <-ctx.Done():
		return runtimerealtime.RecvEvent{}, ctx.Err()
	case <-s.detached:
		return runtimerealtime.RecvEvent{}, runtimerealtime.ErrSessionClosed
	case <-s.closed:
		if event, err, handled := s.recvQueuedOutbound(); handled {
			return event, err
		}
		return runtimerealtime.RecvEvent{}, runtimerealtime.ErrSessionClosed
	case outbound, ok := <-s.recvCh:
		return decodeOpenAIRealtimeOutbound(outbound, ok)
	}
}

func (s *openAIRealtimeSession) startReadLoop() {
	if s == nil || s.conn == nil {
		return
	}
	s.readLoopOnce.Do(func() {
		s.mu.Lock()
		s.readLoopDone = make(chan struct{})
		s.mu.Unlock()
		go s.readLoop()
	})
}

func (s *openAIRealtimeSession) Detach(reason string) {
	if s == nil {
		return
	}
	s.detachOnce.Do(func() {
		s.detachReason = strings.TrimSpace(reason)
		close(s.detached)
		if s.conn != nil {
			s.conn.Close(wsconn.CloseInfo{
				Kind:   wsconn.CloseKindGracefulShutdown,
				Code:   wsconn.CloseNormalClosure,
				Reason: s.detachReason,
			})
		}
		s.startDetachTimer()
	})
}

func (s *openAIRealtimeSession) Abort(reason string) {
	s.mu.Lock()
	readDone := s.readLoopDone
	s.mu.Unlock()
	if readDone != nil {
		s.stopFutureWork(runtimerealtime.ErrSessionClosed)
		s.conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: reason})
		<-readDone
		return
	}
	s.close(reason)
}

func (s *openAIRealtimeSession) SupportsGracefulDetach() bool {
	return false
}

func (s *openAIRealtimeSession) SetTurnObserverFactory(factory runtimesession.TurnObserverFactory) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.turnObserverFactory = factory
	if s.turn != nil && s.turn.observer == nil && factory != nil {
		s.turn.observer = runtimesession.GuardTurnObserver(factory())
	}
	s.mu.Unlock()
	if factory != nil {
		s.startReadLoop()
	}
}

func (s *openAIRealtimeSession) readLoop() {
	if s.readLoopDone != nil {
		defer close(s.readLoopDone)
	}
	defer close(s.recvCh)
	defer func() {
		if recovered := recover(); recovered != nil {
			logOpenAIRealtimeInternalError(fmt.Sprintf("openai realtime read loop panic session=%s provider=%s: %v", s.sessionID, s.model, recovered))
			logOpenAIRealtimeInternalError(fmt.Sprintf("stacktrace from panic: %s", string(debug.Stack())))
			providerErr := types.NewErrorEvent("", "provider_error", "provider_panic", "upstream realtime reader failed")
			payload := []byte(providerErr.Error())
			s.enqueueOutbound(openAIRealtimeOutbound{
				messageType: wsconn.TextMessage,
				payload:     payload,
				origin:      runtimerealtime.RealtimePayloadOriginProxyLocal,
				err:         runtimerealtime.ErrSessionClosed,
			})
		}
		s.close("upstream_closed")
	}()

	if s == nil || s.conn == nil {
		return
	}

	frameCh := make(chan openAIRealtimeProviderFrame, 64)
	providerFrameBudget := runtimerealtime.NewByteBudget(config.RealtimeWebsocketProviderFrameQueueMaxBytes())
	closeCh := make(chan wsconn.CloseInfo, 1)
	var finishPumpOnce sync.Once
	finishPump := func(info wsconn.CloseInfo) {
		finishPumpOnce.Do(func() {
			closeCh <- info
			close(frameCh)
		})
	}
	pump := &wsconn.Pump{
		Conn: s.conn,
		Handle: func(handleCtx context.Context, messageType wsconn.MessageType, payload []byte) {
			credit, err := providerFrameBudget.Acquire(handleCtx, len(payload))
			if err != nil {
				if handleCtx.Err() == nil {
					s.conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindBackpressure, Code: wsconn.CloseTryAgainLater, Reason: "openai_provider_frame_byte_budget", Err: err})
				}
				return
			}
			frame := openAIRealtimeProviderFrame{messageType: messageType, payload: append([]byte(nil), payload...), credit: credit}
			select {
			case frameCh <- frame:
			case <-handleCtx.Done():
				frame.release()
			case <-s.conn.Done():
				frame.release()
			}
		},
		OnClose: finishPump,
	}
	pumpCtx := s.pumpCtx
	if pumpCtx == nil {
		pumpCtx = context.Background()
	}
	go pump.Run(pumpCtx)

	s.consumeProviderFrames(frameCh)
	info := <-closeCh
	s.handlePumpClose(info)
}

func (s *openAIRealtimeSession) consumeProviderFrames(frameCh <-chan openAIRealtimeProviderFrame) {
	terminal := false
	for frame := range frameCh {
		frame.release()
		if terminal {
			s.observeSupplierMessage(frame.messageType, frame.payload)
			continue
		}
		terminal = s.handleProviderFrame(frame)
		if terminal {
			s.stopFutureWork(runtimerealtime.ErrSessionClosed)
		}
	}
}

func (s *openAIRealtimeSession) handleProviderFrame(frame openAIRealtimeProviderFrame) bool {
	if s == nil {
		return true
	}
	outbound, shouldClose := s.observeSupplierMessage(frame.messageType, frame.payload)
	if len(outbound.payload) > 0 || outbound.usage != nil || outbound.err != nil {
		if !s.enqueueOutbound(outbound) {
			if s.conn != nil {
				s.conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindBackpressure, Code: wsconn.CloseTryAgainLater, Reason: "openai_outbound_backpressure"})
			}
			return true
		}
	}
	if shouldClose {
		if s.conn != nil {
			s.conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "provider_message_terminal"})
		}
		return true
	}
	return false
}

func (s *openAIRealtimeSession) observeSupplierMessage(messageType wsconn.MessageType, payload []byte) (openAIRealtimeOutbound, bool) {
	outbound := openAIRealtimeOutbound{
		messageType: messageType,
		payload:     payload,
		origin:      runtimerealtime.RealtimePayloadOriginProvider,
	}
	if messageType != wsconn.TextMessage {
		return outbound, false
	}

	var event types.Event
	if err := json.Unmarshal(payload, &event); err != nil {
		return outbound, false
	}
	eventType := strings.TrimSpace(event.Type)
	if s.observeInputSettings(eventType, payload) {
		return outbound, false
	}
	sessionModel := ""
	if eventType == types.EventTypeSessionCreated && event.Session != nil {
		sessionModel = strings.TrimSpace(event.Session.Model)
	}

	receivedAt := time.Now()
	terminal, terminationReason := openAIRealtimeTurnTerminal(eventType, &event)
	usage := openAIRealtimeEventUsage(eventType, event.EventId, event.Response, payload).Clone()
	if handled, err := s.observeInputEvent(eventType, payload, usage); handled || err != nil {
		if handled {
			outbound.usage = usage.Clone()
		}
		if err != nil {
			outbound.err = openAIRealtimeClientPayloadErrorFromObserver(err)
			return outbound, true
		}
		return outbound, false
	}
	responseID := ""
	if event.Response != nil {
		responseID = strings.TrimSpace(event.Response.ID)
	}

	var (
		usageDelta            *types.UsageEvent
		observer              runtimesession.TurnObserver
		turnState             *openAIRealtimeTurnState
		explicitResponse      bool
		providerInitiated     bool
		providerAdmissionErr  error
		trackingErr           error
		createRejected        bool
		injectedCorrelationID string
	)
	s.mu.Lock()
	alreadyStopped := s.futureWorkErr != nil
	actualModel := strings.TrimSpace(s.actualModel)
	if sessionModel != "" {
		actualModel = sessionModel
	}
	if actualModel == "" {
		actualModel = strings.TrimSpace(s.model)
	}
	if usage != nil && strings.TrimSpace(usage.ResponseModel) == "" {
		usage.ResponseModel = actualModel
	}
	selection := s.selectSupplierTurnLocked(responseID)
	if !selection.dropAttribution && (len(responseID) > openAIRealtimeIdentifierMaxBytes || openAIRealtimeUsageIdentityExceedsLimit(usage)) {
		trackingErr = errOpenAIRealtimeUsageStateLimit
	}
	if trackingErr != nil {
		turnState = s.turn
	} else {
		turnState = selection.state
		if turnState == nil && !selection.dropAttribution && openAIRealtimeProviderStartsTurn(eventType, responseID) && (!alreadyStopped || s.hasAutomaticWorkLocked()) {
			turnState, trackingErr = s.startProviderInitiatedTurnLocked(receivedAt, responseID)
			providerInitiated = turnState != nil
		}
		if turnState != nil && trackingErr == nil {
			explicitResponse = strings.TrimSpace(turnState.clientEventID) != ""
			usageDelta, trackingErr = turnState.observeSupplierEventWithUsage(eventType, responseID, usage, receivedAt, event.IsError())
			if event.IsError() && event.ErrorDetail != nil {
				createRejected = turnState.rejectedCreate(event.ErrorDetail.EventId)
				injectedCorrelationID = turnState.injectedCorrelationID(event.ErrorDetail.EventId)
			}
			observer = turnState.observer
		} else if !selection.dropAttribution && trackingErr == nil {
			usageDelta = usage.Clone()
		}
		if trackingErr == nil {
			if sessionModel != "" {
				s.actualModel = sessionModel
			}
		}
	}
	s.mu.Unlock()
	if trackingErr != nil {
		if turnState != nil {
			s.runFinalizers(s.finalizeObservedTurnState(turnState, "provider_usage_state_limit", receivedAt))
		}
		errorEvent := types.NewErrorEvent("", "provider_error", "provider_usage_state_limit", "provider usage state limit exceeded")
		outbound.messageType = wsconn.TextMessage
		outbound.payload = []byte(errorEvent.Error())
		outbound.usage = nil
		outbound.origin = runtimerealtime.RealtimePayloadOriginProxyLocal
		return outbound, true
	}
	if providerInitiated && observer != nil {
		admission := s.claimAutomaticWork()
		providerAdmissionErr = runtimesession.ObserveProviderInitiatedTurn(observer, admission)
		if admission.WorkAuthorized && !alreadyStopped {
			providerAdmissionErr = errors.Join(providerAdmissionErr, s.checkFutureWork(admission.Models, false))
		}
	}
	if injectedCorrelationID != "" {
		outbound.payload = stripOpenAIRealtimeErrorCorrelation(payload, injectedCorrelationID)
	}

	if usageDelta != nil {
		outbound.usage = usageDelta.Clone()
		if observer != nil {
			if err := observer.ObserveTurnUsage(usageDelta.Clone()); err != nil {
				logOpenAIRealtimeInternalError("openai realtime observer usage error: " + err.Error())
				s.runFinalizers(s.finalizeObservedTurnState(turnState, "quota_exhausted", receivedAt))
				outbound.err = openAIRealtimeClientPayloadErrorFromObserver(err)
				outbound.origin = runtimerealtime.RealtimePayloadOriginProxyLocal
				return outbound, true
			}
		}
	}

	if createRejected {
		if finalized := s.finalizeObservedTurnState(turnState, types.EventTypeError, receivedAt); len(finalized) > 0 {
			s.runFinalizers(finalized)
		}
		if explicitResponse {
			s.resolveManualResponseInput()
		}
	} else if terminal {
		if finalized := s.finalizeObservedTurnState(turnState, terminationReason, receivedAt); len(finalized) > 0 {
			s.runFinalizers(finalized)
		}
		if explicitResponse {
			s.resolveManualResponseInput()
		}
	}
	if providerAdmissionErr != nil {
		// 已开始的 response 保留到已接收帧排空；未来工作拒绝不等于本轮没有费用。
		s.stopFutureWork(providerAdmissionErr)
		outbound.err = openAIRealtimeClientPayloadErrorFromObserver(providerAdmissionErr)
		outbound.origin = runtimerealtime.RealtimePayloadOriginProxyLocal
		return outbound, true
	}

	s.mu.Lock()
	stopErr := s.futureWorkErr
	s.mu.Unlock()
	if stopErr != nil {
		outbound.err = openAIRealtimeClientPayloadErrorFromObserver(stopErr)
		return outbound, true
	}
	return outbound, false
}

func (s *openAIRealtimeSession) handlePumpClose(info wsconn.CloseInfo) {
	if s == nil {
		return
	}
	var closeErr *wsconn.CloseError
	if errors.As(info.Err, &closeErr) || openAIRealtimeCloseAsProviderClose(info.Kind) {
		code := int(info.Code)
		if closeErr != nil {
			code = int(closeErr.Code)
		}
		s.enqueueOutbound(openAIRealtimeOutbound{
			providerClose: &runtimerealtime.ProviderClose{
				Code:   code,
				Reason: info.Reason,
				Err:    runtimerealtime.ErrSessionClosed,
			},
			origin: runtimerealtime.RealtimePayloadOriginProvider,
		})
		return
	}
	if info.Kind == wsconn.CloseKindAbort && info.Reason == "provider_message_terminal" {
		return
	}
	if info.Err != nil {
		logOpenAIRealtimeInternalError("openai realtime websocket read failed: " + info.Err.Error())
	}
	providerErr := types.NewErrorEvent("", "provider_error", "provider_connection_closed", "upstream websocket connection closed")
	payload := []byte(providerErr.Error())
	s.enqueueOutbound(openAIRealtimeOutbound{
		messageType: wsconn.TextMessage,
		payload:     payload,
		origin:      runtimerealtime.RealtimePayloadOriginProxyLocal,
		err:         runtimerealtime.ErrSessionClosed,
	})
}

func openAIRealtimeCloseAsProviderClose(kind wsconn.CloseKind) bool {
	switch kind {
	case wsconn.CloseKindPeerClose, wsconn.CloseKindNormal, wsconn.CloseKindGracefulShutdown:
		return true
	default:
		return false
	}
}

func logOpenAIRealtimeInternalError(message string) {
	if logger.Logger != nil {
		logger.SysError(message)
		return
	}
	log.Printf("[SYS] | %s", message)
}

func openAIRealtimeClientPayloadErrorFromObserver(err error) error {
	if err == nil {
		return nil
	}
	if payload := runtimerealtime.ClientPayloadFromError(err); len(payload) > 0 {
		return err
	}
	var event *types.Event
	if errors.As(err, &event) && event != nil && event.IsError() {
		return err
	}

	code := "quota_exhausted"
	var apiErr *types.OpenAIErrorWithStatusCode
	if errors.As(err, &apiErr) && apiErr != nil {
		code = openAIRealtimeErrorCodeString(apiErr.Code, code)
	}
	event = types.NewErrorEvent("", "system_error", code, openAIRealtimeObserverErrorMessage(code))
	return runtimerealtime.NewClientPayloadError(event, []byte(event.Error()))
}

func openAIRealtimeErrorCodeString(code any, fallback string) string {
	switch typed := code.(type) {
	case string:
		if trimmed := strings.TrimSpace(typed); trimmed != "" {
			return trimmed
		}
	case nil:
	default:
		if trimmed := strings.TrimSpace(fmt.Sprint(typed)); trimmed != "" && trimmed != "<nil>" {
			return trimmed
		}
	}
	return fallback
}

func openAIRealtimeObserverErrorMessage(code string) string {
	switch strings.TrimSpace(code) {
	case "insufficient_user_quota", "quota_exhausted":
		return "realtime quota limit exceeded"
	case "pre_consume_token_quota_failed":
		return "realtime token quota is not enough"
	case "invalid_model_price":
		return "realtime model price is invalid"
	default:
		return "realtime quota check failed"
	}
}

func (s *openAIRealtimeSession) startTurnWithClientEventID(clientEventID string, injected bool) (*openAIRealtimeTurnState, error) {
	clientEventID = strings.TrimSpace(clientEventID)
	if len(clientEventID) > openAIRealtimeIdentifierMaxBytes {
		return nil, newOpenAIRealtimeClientError("invalid_event", "realtime event_id exceeds the supported limit")
	}
	s.mu.Lock()
	if s.turn != nil {
		s.mu.Unlock()
		return nil, newOpenAIRealtimeClientError("session_busy", "realtime session already has an inflight response")
	}
	s.turnSeq++
	var observer runtimesession.TurnObserver
	if s.turnObserverFactory != nil {
		observer = runtimesession.GuardTurnObserver(s.turnObserverFactory())
	}
	s.turn = newOpenAIRealtimeTurnState(s.turnSeq, time.Now(), observer)
	s.turn.clientEventID = strings.Clone(clientEventID)
	s.turn.clientEventIDInjected = injected
	startedTurn := s.turn
	s.mu.Unlock()
	return startedTurn, nil
}

func openAIRealtimeProviderStartsTurn(eventType, responseID string) bool {
	return strings.TrimSpace(responseID) != "" && strings.HasPrefix(strings.TrimSpace(eventType), "response.")
}

func (s *openAIRealtimeSession) startProviderInitiatedTurnLocked(startedAt time.Time, responseID string) (*openAIRealtimeTurnState, error) {
	if s == nil || s.turn != nil || s.turnObserverFactory == nil {
		return nil, nil
	}
	s.turnSeq++
	observer := runtimesession.GuardTurnObserver(s.turnObserverFactory())
	if observer == nil {
		return nil, nil
	}
	s.turn = newOpenAIRealtimeTurnState(s.turnSeq, startedAt, observer)
	if err := s.turn.rememberResponseID(responseID); err != nil {
		s.turn = nil
		return nil, err
	}
	return s.turn, nil
}

func (s *openAIRealtimeSession) rollbackTurn(turn *openAIRealtimeTurnState) bool {
	s.mu.Lock()
	rolledBack := s.turn == turn
	if s.turn == turn {
		s.turn = nil
	}
	s.mu.Unlock()
	return rolledBack
}

func (s *openAIRealtimeSession) resolveOpenAIRealtimeWriteFailure(turn *openAIRealtimeTurnState, attempted bool) bool {
	if turn == nil || attempted {
		return false
	}
	return s.rollbackTurn(turn)
}

func (s *openAIRealtimeSession) rollbackOpenAIRealtimeTurnAdmission(turn *openAIRealtimeTurnState, reason string) {
	if turn == nil || turn.observer == nil {
		return
	}
	if err := runtimesession.RollbackTurnAdmission(turn.observer, reason); err != nil {
		logOpenAIRealtimeInternalError("openai realtime turn admission rollback failed: " + err.Error())
	}
	s.observeFinalization(turn.observer)
}

func (s *openAIRealtimeSession) finalizeTurn(reason string, now time.Time) (runtimesession.TurnObserver, runtimesession.TurnFinalizePayload) {
	s.mu.Lock()
	observer, payload := s.finalizeTurnLocked(reason, now)
	s.mu.Unlock()
	return observer, payload
}

func (s *openAIRealtimeSession) finalizeTurnLocked(reason string, now time.Time) (runtimesession.TurnObserver, runtimesession.TurnFinalizePayload) {
	if s.turn == nil {
		return nil, runtimesession.TurnFinalizePayload{}
	}
	responseIDs := s.turn.responseIDs()
	observer, payload := s.turn.finalize(s.sessionID, s.billingModelLocked(), reason, now)
	payload.Models = s.modelBinding()
	payload.Models.ReportedModel = s.actualModel
	if payload.Usage != nil && payload.Usage.ResponseModel != "" {
		payload.Models.ReportedModel = payload.Usage.ResponseModel
	}
	payload.WorkID = fmt.Sprintf("response-%d", s.turn.seq)
	s.rememberFinalizedResponseIDsLocked(append(responseIDs, payload.LastResponseID)...)
	s.turn = nil
	return observer, payload
}

func (s *openAIRealtimeSession) billingModelLocked() string {
	return s.modelBinding().BillingModel
}

func (s *openAIRealtimeSession) close(reason string) {
	var finalized []openAIRealtimeFinalizedTurn
	var inputs []*openAIRealtimeInputWork
	s.closeOnce.Do(func() {
		now := time.Now()
		s.mu.Lock()
		if observer, payload := s.finalizeTurnLocked(strings.TrimSpace(reason), now); observer != nil {
			finalized = append(finalized, openAIRealtimeFinalizedTurn{observer: observer, payload: payload})
		}
		inputs = append(inputs, s.inputWorks...)
		s.inputWorks = nil
		s.audioWork = nil
		s.manualResponseInput = nil
		for _, update := range s.pendingSettings {
			if update == nil || update.resolved {
				continue
			}
			update.resolved = true
			close(update.done)
		}
		s.pendingSettings = nil
		s.mu.Unlock()
		s.stopDetachTimer()
		if s.conn != nil {
			s.conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: strings.TrimSpace(reason)})
		}
		close(s.closed)
	})
	s.runFinalizers(finalized)
	for _, work := range inputs {
		s.mu.Lock()
		submitted := work.submitted
		s.mu.Unlock()
		s.finishInput(work, reason, nil, !submitted)
	}
}

func (s *openAIRealtimeSession) isDetached() bool {
	if s == nil {
		return true
	}
	select {
	case <-s.detached:
		return true
	default:
		return false
	}
}

func (s *openAIRealtimeSession) recvQueuedOutbound() (runtimerealtime.RecvEvent, error, bool) {
	if s == nil {
		return runtimerealtime.RecvEvent{}, runtimerealtime.ErrSessionClosed, true
	}
	select {
	case outbound, ok := <-s.recvCh:
		event, err := decodeOpenAIRealtimeOutbound(outbound, ok)
		return event, err, true
	default:
		return runtimerealtime.RecvEvent{}, nil, false
	}
}

func decodeOpenAIRealtimeOutbound(outbound openAIRealtimeOutbound, ok bool) (runtimerealtime.RecvEvent, error) {
	if !ok {
		return runtimerealtime.RecvEvent{}, runtimerealtime.ErrSessionClosed
	}
	outbound.release()
	event := runtimerealtime.RecvEvent{
		ProviderClose: outbound.providerClose,
		Usage:         outbound.usage,
		Origin:        outbound.origin,
		Err:           outbound.err,
	}
	if len(outbound.payload) > 0 {
		frame := openAIRealtimeFrameFromMessage(outbound.messageType, outbound.payload)
		event.Frame = &frame
	}
	return event, nil
}

func openAIRealtimeMessageFromFrame(frame runtimerealtime.Frame) (wsconn.MessageType, []byte, error) {
	if frame.IsZero() {
		return 0, nil, runtimerealtime.ErrInvalidFrame
	}
	switch frame.Kind() {
	case runtimerealtime.FrameKindText:
		return wsconn.TextMessage, frame.Payload(), nil
	case runtimerealtime.FrameKindBinary:
		return wsconn.BinaryMessage, frame.Payload(), nil
	default:
		return 0, nil, runtimerealtime.ErrInvalidFrame
	}
}

func openAIRealtimeFrameFromMessage(messageType wsconn.MessageType, payload []byte) runtimerealtime.Frame {
	if messageType == wsconn.BinaryMessage {
		return runtimerealtime.NewBinaryFrame(payload)
	}
	return runtimerealtime.NewTextFrame(payload)
}

func openAIRealtimeTurnTerminal(eventType string, event *types.Event) (bool, string) {
	status := ""
	if event != nil {
		if event.Response != nil {
			status = event.Response.Status
		}
	}
	classified := realtimeprotocol.ClassifyTerminal(eventType, status)
	if !classified.IsTerminal() {
		return false, ""
	}
	return true, types.EventTypeResponseDone
}

func (s *openAIRealtimeSession) selectSupplierTurnLocked(responseID string) openAIRealtimeTurnSelection {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		if s.turn != nil {
			return openAIRealtimeTurnSelection{state: s.turn}
		}
		return openAIRealtimeTurnSelection{}
	}

	if s.turn != nil && s.turn.matchesResponseID(responseID) {
		return openAIRealtimeTurnSelection{state: s.turn}
	}
	if s.isRecentlyFinalizedResponseIDLocked(responseID) {
		return openAIRealtimeTurnSelection{dropAttribution: true}
	}
	if s.turn != nil {
		return openAIRealtimeTurnSelection{state: s.turn}
	}
	return openAIRealtimeTurnSelection{}
}

func (s *openAIRealtimeSession) finalizeObservedTurnState(turnState *openAIRealtimeTurnState, reason string, now time.Time) []openAIRealtimeFinalizedTurn {
	if s == nil || turnState == nil {
		return nil
	}

	s.mu.Lock()
	if s.turn == turnState {
		observer, payload := s.finalizeTurnLocked(reason, now)
		s.mu.Unlock()
		if observer == nil {
			return nil
		}
		return []openAIRealtimeFinalizedTurn{{
			observer: observer,
			payload:  payload,
		}}
	}
	s.mu.Unlock()
	return nil
}

func (s *openAIRealtimeSession) enqueueOutbound(outbound openAIRealtimeOutbound) bool {
	for _, current := range normalizeOpenAIRealtimeOutbound(outbound) {
		if !s.enqueueSingleOutbound(current) {
			return false
		}
	}
	return true
}

func normalizeOpenAIRealtimeOutbound(outbound openAIRealtimeOutbound) []openAIRealtimeOutbound {
	hasPayload := len(outbound.payload) > 0
	hasUsage := outbound.usage != nil
	hasErr := outbound.err != nil
	if outbound.providerClose != nil && (hasPayload || hasUsage || hasErr) {
		return []openAIRealtimeOutbound{{
			providerClose: outbound.providerClose,
			origin:        outbound.origin,
		}}
	}
	if !hasErr || !hasUsage {
		return []openAIRealtimeOutbound{outbound}
	}
	errEvent := openAIRealtimeOutbound{
		origin: outbound.origin,
		err:    outbound.err,
	}
	if payload := runtimerealtime.ClientPayloadFromError(outbound.err); len(payload) > 0 {
		errEvent.messageType = wsconn.TextMessage
		errEvent.payload = payload
		errEvent.origin = runtimerealtime.RealtimePayloadOriginProxyLocal
	}
	outbound.err = nil
	return []openAIRealtimeOutbound{outbound, errEvent}
}

func (s *openAIRealtimeSession) enqueueSingleOutbound(outbound openAIRealtimeOutbound) bool {
	if s == nil {
		return false
	}

	select {
	case <-s.closed:
		return false
	default:
	}
	select {
	case <-s.detached:
		return s.discardDetachedOutbound()
	default:
	}
	if s.outboundBudget == nil {
		s.outboundBudget = runtimerealtime.NewByteBudget(config.RealtimeWebsocketAttachmentQueueMaxBytes())
	}
	acquireCtx := context.Background()
	var cancelAcquire context.CancelFunc
	if s.outboundBackpressureTimeout > 0 {
		acquireCtx, cancelAcquire = context.WithTimeout(acquireCtx, s.outboundBackpressureTimeout)
		defer cancelAcquire()
	}
	credit, err := s.outboundBudget.Acquire(acquireCtx, len(outbound.payload))
	if err != nil {
		logOpenAIRealtimeInternalError("openai realtime outbound byte budget exhausted: " + err.Error())
		return false
	}
	outbound.credit = credit
	transferred := false
	defer func() {
		if !transferred {
			outbound.release()
		}
	}()

	var timer *time.Timer
	var timerC <-chan time.Time
	if s.outboundBackpressureTimeout > 0 {
		timer = time.NewTimer(s.outboundBackpressureTimeout)
		defer timer.Stop()
		timerC = timer.C
	}

	select {
	case <-s.closed:
		return false
	case <-s.detached:
		return s.discardDetachedOutbound()
	case s.recvCh <- outbound:
		transferred = true
		return true
	case <-timerC:
		logOpenAIRealtimeInternalError("openai realtime outbound queue backpressure timeout")
		return false
	}
}

func (s *openAIRealtimeSession) discardDetachedOutbound() bool {
	if s == nil {
		return false
	}

	s.detachLog.Do(func() {
		reason := strings.TrimSpace(s.detachReason)
		if reason == "" {
			reason = "detached"
		}
		log.Printf(
			"dropping detached realtime outbound events while draining upstream for up to %s (reason=%s)",
			openAIRealtimeDetachGraceTimeout,
			reason,
		)
	})
	return true
}

func (s *openAIRealtimeSession) startDetachTimer() {
	if s == nil || openAIRealtimeDetachGraceTimeout <= 0 {
		return
	}
	s.detachMu.Lock()
	defer s.detachMu.Unlock()
	if s.detachTimer != nil {
		return
	}
	s.detachTimer = time.AfterFunc(openAIRealtimeDetachGraceTimeout, func() {
		s.Abort("detach_timeout")
	})
}

func (s *openAIRealtimeSession) stopDetachTimer() {
	if s == nil {
		return
	}
	s.detachMu.Lock()
	defer s.detachMu.Unlock()
	if s.detachTimer == nil {
		return
	}
	s.detachTimer.Stop()
	s.detachTimer = nil
}

func normalizeOpenAIRealtimeClientPayload(payload []byte, messageType wsconn.MessageType, modelName string, compatMode bool) ([]byte, string, error) {
	if messageType != wsconn.TextMessage {
		return payload, "", nil
	}

	var message map[string]json.RawMessage
	if err := json.Unmarshal(payload, &message); err != nil {
		return payload, "", nil
	}

	eventType := rawJSONString(message["type"])
	if compatMode {
		return payload, eventType, nil
	}
	if eventType != "response.create" {
		return payload, eventType, nil
	}

	trimmedModel := strings.TrimSpace(modelName)
	if trimmedModel == "" {
		return payload, eventType, nil
	}

	encodedModel, err := json.Marshal(trimmedModel)
	if err != nil {
		return payload, eventType, nil
	}

	if response, ok := rawJSONObject(message["response"]); ok {
		if rawJSONString(response["model"]) != "" {
			return payload, eventType, nil
		}
		response["model"] = encodedModel
		encodedResponse, err := json.Marshal(response)
		if err != nil {
			return payload, eventType, nil
		}
		message["response"] = encodedResponse
	} else if rawJSONString(message["model"]) == "" {
		message["model"] = encodedModel
	} else {
		return payload, eventType, nil
	}

	normalized, err := json.Marshal(message)
	if err != nil {
		return payload, eventType, nil
	}
	return normalized, eventType, nil
}

func openAIRealtimeClientEventID(payload []byte) string {
	var event struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return ""
	}
	return strings.TrimSpace(event.EventID)
}

func ensureOpenAIRealtimeClientEventID(payload []byte) ([]byte, string, bool, error) {
	var message map[string]json.RawMessage
	if err := json.Unmarshal(payload, &message); err != nil || message == nil {
		return payload, "", false, err
	}
	if raw, exists := message["event_id"]; exists {
		return payload, rawJSONString(raw), false, nil
	}

	correlationID := "evt_onehub_" + uuid.NewString()
	encodedID, err := json.Marshal(correlationID)
	if err != nil {
		return nil, "", false, err
	}
	message["event_id"] = encodedID
	normalized, err := json.Marshal(message)
	if err != nil {
		return nil, "", false, err
	}
	return normalized, correlationID, true, nil
}

func stripOpenAIRealtimeErrorCorrelation(payload []byte, correlationID string) []byte {
	if strings.TrimSpace(correlationID) == "" {
		return payload
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(payload, &message); err != nil || message == nil {
		return payload
	}
	errorObject, ok := rawJSONObject(message["error"])
	if !ok || rawJSONString(errorObject["event_id"]) != correlationID {
		return payload
	}
	delete(errorObject, "event_id")
	encodedError, err := json.Marshal(errorObject)
	if err != nil {
		return payload
	}
	message["error"] = encodedError
	normalized, err := json.Marshal(message)
	if err != nil {
		return payload
	}
	return normalized
}

func rawJSONObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, false
	}
	return object, true
}

func rawJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func (s *openAIRealtimeSession) rememberFinalizedResponseIDsLocked(responseIDs ...string) {
	if len(responseIDs) == 0 {
		return
	}

	updated := make([]string, 0, openAIRealtimeFinalizedResponseIDLimit)
	for _, existing := range s.recentFinalizedIDs {
		trimmed := strings.TrimSpace(existing)
		if trimmed != "" && len(trimmed) <= openAIRealtimeIdentifierMaxBytes {
			updated = append(updated, strings.Clone(trimmed))
			if len(updated) > openAIRealtimeFinalizedResponseIDLimit {
				updated = append(updated[:0], updated[len(updated)-openAIRealtimeFinalizedResponseIDLimit:]...)
			}
		}
	}
	for _, responseID := range responseIDs {
		trimmed := strings.TrimSpace(responseID)
		if trimmed == "" || len(trimmed) > openAIRealtimeIdentifierMaxBytes {
			continue
		}
		filtered := updated[:0]
		for _, existing := range updated {
			if existing != trimmed {
				filtered = append(filtered, existing)
			}
		}
		updated = append(filtered, strings.Clone(trimmed))
		if len(updated) > openAIRealtimeFinalizedResponseIDLimit {
			copy(updated, updated[len(updated)-openAIRealtimeFinalizedResponseIDLimit:])
			updated = updated[:openAIRealtimeFinalizedResponseIDLimit]
		}
	}
	s.recentFinalizedIDs = updated
}

func (s *openAIRealtimeSession) isRecentlyFinalizedResponseIDLocked(responseID string) bool {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return false
	}
	for _, existing := range s.recentFinalizedIDs {
		if existing == responseID {
			return true
		}
	}
	return false
}

func anyToString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return ""
	}
}

func openAIRealtimeResponseUsage(providerEventID string, response *types.ResponseEvent, payload []byte) *types.UsageEvent {
	if response == nil || response.Usage == nil {
		return nil
	}
	usage := response.Usage.Clone()
	usage.Source = types.UsageSourceRealtimeResponse
	usage.BillingBasis = types.UsageBillingBasisTokens
	usage.ProviderEventID = strings.TrimSpace(providerEventID)
	usage.ResponseID = strings.TrimSpace(response.ID)
	usage.ResponseModel = strings.TrimSpace(response.Model)
	usage.ServiceTier = strings.TrimSpace(response.ServiceTier)
	usage.ProviderTokenEvidence = usage.ProviderTokenEvidence || openAIRealtimeResponseHasCompleteTokenPartition(payload)
	return usage
}

func openAIRealtimeEventUsage(eventType string, providerEventID string, response *types.ResponseEvent, payload []byte) *types.UsageEvent {
	if usage := openAIRealtimeResponseUsage(providerEventID, response, payload); usage != nil {
		return usage
	}
	return openAIRealtimeInputAudioTranscriptionUsage(eventType, providerEventID, payload)
}

func openAIRealtimeResponseHasCompleteTokenPartition(payload []byte) bool {
	var envelope struct {
		Response struct {
			Usage map[string]json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return false
	}
	return openAIRealtimeNonNegativeInteger(envelope.Response.Usage["input_tokens"]) &&
		openAIRealtimeNonNegativeInteger(envelope.Response.Usage["output_tokens"])
}

func openAIRealtimeNonNegativeInteger(raw json.RawMessage) bool {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	var value int
	return json.Unmarshal(raw, &value) == nil && value >= 0
}

type openAIRealtimeInputAudioTranscriptionUsageEvent struct {
	EventID string                                `json:"event_id"`
	Type    string                                `json:"type"`
	ItemID  string                                `json:"item_id"`
	Usage   *openAIRealtimeTranscriptionUsageWire `json:"usage,omitempty"`
}

type openAIRealtimeTranscriptionInputDetailsWire struct {
	AudioTokens          *int `json:"audio_tokens,omitempty"`
	CachedTokens         *int `json:"cached_tokens,omitempty"`
	TextTokens           *int `json:"text_tokens,omitempty"`
	ImageTokens          *int `json:"image_tokens,omitempty"`
	CachedTokensInternal *int `json:"cached_tokens_internal,omitempty"`
	CacheWriteTokens     *int `json:"cache_write_tokens,omitempty"`
	CachedWriteTokens    *int `json:"cached_write_tokens,omitempty"`
	CachedReadTokens     *int `json:"cached_read_tokens,omitempty"`
}

type openAIRealtimeTranscriptionOutputDetailsWire struct {
	AudioTokens              *int `json:"audio_tokens,omitempty"`
	TextTokens               *int `json:"text_tokens,omitempty"`
	ReasoningTokens          *int `json:"reasoning_tokens,omitempty"`
	AcceptedPredictionTokens *int `json:"accepted_prediction_tokens,omitempty"`
	RejectedPredictionTokens *int `json:"rejected_prediction_tokens,omitempty"`
	ImageTokens              *int `json:"image_tokens,omitempty"`
}

type openAIRealtimeTranscriptionUsageWire struct {
	Type               types.UsageBillingBasis                       `json:"type,omitempty"`
	InputTokens        *int                                          `json:"input_tokens,omitempty"`
	OutputTokens       *int                                          `json:"output_tokens,omitempty"`
	TotalTokens        *int                                          `json:"total_tokens,omitempty"`
	InputTokenDetails  *openAIRealtimeTranscriptionInputDetailsWire  `json:"input_token_details,omitempty"`
	OutputTokenDetails *openAIRealtimeTranscriptionOutputDetailsWire `json:"output_token_details,omitempty"`
	Seconds            *float64                                      `json:"seconds,omitempty"`
}

func openAIRealtimeInputAudioTranscriptionUsage(eventType string, providerEventID string, payload []byte) *types.UsageEvent {
	if strings.TrimSpace(eventType) != types.EventTypeInputAudioTranscriptionCompleted {
		return nil
	}
	var event openAIRealtimeInputAudioTranscriptionUsageEvent
	if err := json.Unmarshal(payload, &event); err != nil || event.Usage == nil {
		return nil
	}
	if strings.TrimSpace(event.Type) != types.EventTypeInputAudioTranscriptionCompleted {
		return nil
	}
	usage := &types.UsageEvent{}
	if event.Usage.InputTokens != nil {
		usage.InputTokens = *event.Usage.InputTokens
	}
	if event.Usage.OutputTokens != nil {
		usage.OutputTokens = *event.Usage.OutputTokens
	}
	if event.Usage.TotalTokens != nil {
		usage.TotalTokens = *event.Usage.TotalTokens
	}
	if details := event.Usage.InputTokenDetails; details != nil {
		if details.AudioTokens != nil {
			usage.InputTokenDetails.AudioTokens = *details.AudioTokens
		}
		if details.CachedTokens != nil {
			usage.InputTokenDetails.CachedTokens = *details.CachedTokens
		}
		if details.TextTokens != nil {
			usage.InputTokenDetails.TextTokens = *details.TextTokens
		}
		if details.ImageTokens != nil {
			usage.InputTokenDetails.ImageTokens = *details.ImageTokens
		}
		if details.CachedTokensInternal != nil {
			usage.InputTokenDetails.CachedTokensInternal = *details.CachedTokensInternal
		}
		if details.CacheWriteTokens != nil {
			usage.InputTokenDetails.CacheWriteTokens = *details.CacheWriteTokens
		}
		if details.CachedWriteTokens != nil {
			usage.InputTokenDetails.CachedWriteTokens = *details.CachedWriteTokens
		}
		if details.CachedReadTokens != nil {
			usage.InputTokenDetails.CachedReadTokens = *details.CachedReadTokens
		}
	}
	if details := event.Usage.OutputTokenDetails; details != nil {
		if details.AudioTokens != nil {
			usage.OutputTokenDetails.AudioTokens = *details.AudioTokens
		}
		if details.TextTokens != nil {
			usage.OutputTokenDetails.TextTokens = *details.TextTokens
		}
		if details.ReasoningTokens != nil {
			usage.OutputTokenDetails.ReasoningTokens = *details.ReasoningTokens
		}
		if details.AcceptedPredictionTokens != nil {
			usage.OutputTokenDetails.AcceptedPredictionTokens = *details.AcceptedPredictionTokens
		}
		if details.RejectedPredictionTokens != nil {
			usage.OutputTokenDetails.RejectedPredictionTokens = *details.RejectedPredictionTokens
		}
		if details.ImageTokens != nil {
			usage.OutputTokenDetails.ImageTokens = *details.ImageTokens
		}
	}
	usage.Source = types.UsageSourceInputAudioTranscription
	usage.ProviderEventID = strings.TrimSpace(providerEventID)
	if usage.ProviderEventID == "" {
		usage.ProviderEventID = strings.TrimSpace(event.EventID)
	}
	usage.ItemID = strings.TrimSpace(event.ItemID)
	switch strings.ToLower(strings.TrimSpace(string(event.Usage.Type))) {
	case string(types.UsageBillingBasisDuration):
		usage.BillingBasis = types.UsageBillingBasisDuration
		usage.DurationSeconds = 0
		if event.Usage.Seconds != nil && validTranscriptionDuration(*event.Usage.Seconds) {
			usage.DurationSeconds = *event.Usage.Seconds
			usage.SetProviderIndependentUsageUnit(config.UsageExtraInputAudioTranscription, usage.DurationSeconds)
			usage.MarkProviderOperationUnits(1)
		}
		return usage
	case string(types.UsageBillingBasisTokens), "":
		usage.BillingBasis = types.UsageBillingBasisTokens
		usage.DurationSeconds = 0
		if !markOpenAIRealtimeTranscriptionTokenEvidence(usage, event.Usage) {
			return usage
		}
		usage.ProviderTokenEvidence = true
		return usage
	default:
		return usage
	}
}

func markOpenAIRealtimeTranscriptionTokenEvidence(usage *types.UsageEvent, wire *openAIRealtimeTranscriptionUsageWire) bool {
	if usage == nil || wire == nil || wire.InputTokens == nil || wire.OutputTokens == nil || wire.TotalTokens == nil {
		return false
	}
	if *wire.InputTokens < 0 || *wire.OutputTokens < 0 || *wire.TotalTokens < 0 {
		usage.ProviderTokenConflict = true
		return false
	}
	if uint64(*wire.InputTokens)+uint64(*wire.OutputTokens) != uint64(*wire.TotalTokens) {
		usage.ProviderTokenConflict = true
		return false
	}
	details := wire.InputTokenDetails
	if details != nil {
		for _, value := range []*int{
			details.AudioTokens, details.CachedTokens, details.TextTokens, details.ImageTokens,
			details.CachedTokensInternal, details.CacheWriteTokens, details.CachedWriteTokens, details.CachedReadTokens,
		} {
			if value != nil && *value < 0 {
				usage.ProviderTokenConflict = true
				return false
			}
		}
	}
	if details := wire.OutputTokenDetails; details != nil {
		for _, value := range []*int{
			details.AudioTokens, details.TextTokens, details.ReasoningTokens,
			details.AcceptedPredictionTokens, details.RejectedPredictionTokens, details.ImageTokens,
		} {
			if value != nil && *value < 0 {
				usage.ProviderTokenConflict = true
				return false
			}
		}
	}
	usage.MarkProviderTokenField("prompt_tokens")
	usage.MarkProviderTokenField("completion_tokens")
	usage.MarkProviderTokenField("total_tokens")
	// Realtime ASR input audio/text are mutually exclusive partitions. The
	// shared price reducer decides whether missing partitions affect the
	// effective price; this producer only records wire presence below.
	usage.RequireTokenExtraEvidence(config.UsageExtraInputAudio, config.UsageExtraInputTextTokens)
	usage.SetTokenExtraEvidenceGroups([]string{config.UsageExtraInputAudio, config.UsageExtraInputTextTokens})
	if details != nil {
		markInputDetail := func(key string, value *int) {
			if value == nil {
				return
			}
			usage.MarkProviderTokenField(key)
			usage.SetExtraTokens(key, *value)
		}
		markInputDetail(config.UsageExtraInputAudio, details.AudioTokens)
		markInputDetail(config.UsageExtraCache, details.CachedTokens)
		markInputDetail(config.UsageExtraInputTextTokens, details.TextTokens)
		markInputDetail(config.UsageExtraInputImageTokens, details.ImageTokens)
		markInputDetail("cached_tokens_internal", details.CachedTokensInternal)
		markInputDetail(config.UsageExtraCacheWrite, details.CacheWriteTokens)
		markInputDetail(config.UsageExtraCachedWrite, details.CachedWriteTokens)
		markInputDetail(config.UsageExtraCachedRead, details.CachedReadTokens)
	}
	if details := wire.OutputTokenDetails; details != nil {
		markOutputDetail := func(key string, value *int) {
			if value == nil {
				return
			}
			usage.MarkProviderTokenField(key)
			usage.SetExtraTokens(key, *value)
		}
		markOutputDetail(config.UsageExtraOutputAudio, details.AudioTokens)
		markOutputDetail(config.UsageExtraOutputTextTokens, details.TextTokens)
		markOutputDetail(config.UsageExtraReasoning, details.ReasoningTokens)
		markOutputDetail("accepted_prediction_tokens", details.AcceptedPredictionTokens)
		markOutputDetail("rejected_prediction_tokens", details.RejectedPredictionTokens)
		markOutputDetail(config.UsageExtraOutputImageTokens, details.ImageTokens)
	}
	return true
}
