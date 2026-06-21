package openai

import (
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
	"one-api/common/requester"
	"one-api/common/responsesws"
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
const openAIRealtimeFinalizedResponseIDLimit = 16

var openAIRealtimeOutboundBackpressureTimeout = 5 * time.Second

type openAIRealtimeOutbound struct {
	messageType   wsconn.MessageType
	payload       []byte
	providerClose *runtimerealtime.ProviderClose
	usage         *types.UsageEvent
	origin        runtimerealtime.RealtimePayloadOrigin
	err           error
}

type openAIRealtimeProviderFrame struct {
	messageType wsconn.MessageType
	payload     []byte
}

type openAIRealtimePendingTurn struct {
	state  *openAIRealtimeTurnState
	reason string
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
	provider   *OpenAIProvider
	model      string
	sessionID  string
	conn       *wsconn.ManagedConn
	compatMode bool

	recvCh       chan openAIRealtimeOutbound
	closed       chan struct{}
	detached     chan struct{}
	closeOnce    sync.Once
	detachOnce   sync.Once
	detachLog    sync.Once
	writeMu      sync.Mutex
	detachTimer  *time.Timer
	detachMu     sync.Mutex
	readLoopOnce sync.Once
	mu           sync.Mutex
	detachReason string

	turnSeq             int64
	turn                *openAIRealtimeTurnState
	pendingTurns        []openAIRealtimePendingTurn
	recentFinalizedIDs  []string
	turnObserverFactory runtimesession.TurnObserverFactory
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
		provider:   p,
		model:      strings.TrimSpace(modelName),
		sessionID:  sessionID,
		conn:       conn,
		compatMode: config.OpenAIRealtimeSessionCompatMode,
		recvCh:     make(chan openAIRealtimeOutbound, 128),
		closed:     make(chan struct{}),
		detached:   make(chan struct{}),
	}
	session.startReadLoop()
	return session, nil
}

func (p *OpenAIProvider) openRealtimeConn(modelName string) (*wsconn.ManagedConn, *types.OpenAIErrorWithStatusCode) {
	return p.openRealtimeConnWithContext(context.Background(), modelName)
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
	httpHeaders := httpHeaderFromOpenAIHeaders(p.requestHeaders(authMode))

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
	fullRequestURL, errWithCode := p.responsesWSURL(modelName)
	if errWithCode != nil {
		return nil, errWithCode
	}

	httpHeaders := httpHeaderFromOpenAIHeaders(p.requestHeaders(openAIRequestAuthBearer))

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
	if p == nil {
		return false
	}
	if p.Context != nil && p.Context.GetBool("responses_ws_self_hosted") {
		return true
	}
	if p.Channel == nil {
		return false
	}
	other, err := p.Channel.GetOtherMap()
	if err != nil {
		return false
	}
	return rawJSONBool(other["responses_ws_self_hosted"])
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
		return mapOpenAIResponsesWSDialStatus(dialErr.StatusCode)
	}
	logOpenAIRealtimeInternalError("openai responses websocket dial failed: " + openAIResponsesWSDialErrorSummary(err))
	return common.StringErrorWrapperLocal("websocket request failed", "ws_request_failed", http.StatusInternalServerError)
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
		return mapOpenAIRealtimeWSDialStatus(dialErr.StatusCode)
	}
	logOpenAIRealtimeInternalError("openai realtime websocket dial failed: " + err.Error())
	return common.StringErrorWrapperLocal("websocket request failed", "ws_request_failed", http.StatusInternalServerError)
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

	normalizedPayload, eventType, err := normalizeOpenAIRealtimeClientPayload(payload, mt, s.model, s.compatMode)
	if err != nil {
		return err
	}
	var (
		startedTurn *openAIRealtimeTurnState
		finalizers  []openAIRealtimeFinalizedTurn
	)
	if eventType == "response.create" {
		startedTurn, finalizers, err = s.startTurn()
		if err != nil {
			return err
		}
		runOpenAIRealtimeFinalizers(finalizers)
		if err := runtimesession.AdmitTurn(startedTurn.observer); err != nil {
			s.rollbackOpenAIRealtimeTurnAdmission(startedTurn, "turn_admission_failed")
			s.rollbackTurn(startedTurn)
			return openAIRealtimeClientPayloadErrorFromObserver(err)
		}
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	select {
	case <-ctx.Done():
		if startedTurn != nil {
			s.rollbackOpenAIRealtimeTurnAdmission(startedTurn, "context_cancelled_before_write")
			s.rollbackTurn(startedTurn)
		}
		return runtimerealtime.ErrSessionClosed
	default:
	}
	if err := s.conn.WriteMessage(wsconn.MessageType(mt), normalizedPayload); err != nil {
		if startedTurn != nil {
			s.rollbackOpenAIRealtimeTurnAdmission(startedTurn, "write_failed")
			s.rollbackTurn(startedTurn)
		}
		logOpenAIRealtimeInternalError("openai realtime websocket write failed: " + err.Error())
		return types.NewErrorEvent("", "system_error", "ws_write_failed", "upstream websocket write failed")
	}
	return nil
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
	defer s.mu.Unlock()
	s.turnObserverFactory = factory
	if s.turn != nil && s.turn.observer == nil && factory != nil {
		s.turn.observer = runtimesession.GuardTurnObserver(factory())
	}
}

func (s *openAIRealtimeSession) readLoop() {
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
		Handle: func(_ context.Context, messageType wsconn.MessageType, payload []byte) {
			frame := openAIRealtimeProviderFrame{messageType: messageType, payload: append([]byte(nil), payload...)}
			select {
			case frameCh <- frame:
			default:
				s.conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindBackpressure, Code: wsconn.CloseTryAgainLater, Reason: "openai_provider_frame_backpressure"})
			}
		},
		OnClose: finishPump,
	}
	go pump.Run(context.Background())

	for frame := range frameCh {
		s.handleProviderFrame(frame)
	}
	info := <-closeCh
	s.handlePumpClose(info)
}

func (s *openAIRealtimeSession) handleProviderFrame(frame openAIRealtimeProviderFrame) {
	if s == nil {
		return
	}
	outbound, shouldClose := s.observeSupplierMessage(frame.messageType, frame.payload)
	if len(outbound.payload) > 0 || outbound.usage != nil || outbound.err != nil {
		if !s.enqueueOutbound(outbound) {
			return
		}
	}
	if shouldClose && s.conn != nil {
		s.conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: "provider_message_terminal"})
	}
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

	receivedAt := time.Now()
	terminal, terminationReason := openAIRealtimeTurnTerminal(eventType, &event)
	usage := openAIRealtimeEventUsage(eventType, event.EventId, event.Response, payload).Clone()
	responseID := ""
	if event.Response != nil {
		responseID = strings.TrimSpace(event.Response.ID)
	}

	var (
		usageDelta *types.UsageEvent
		observer   runtimesession.TurnObserver
		turnState  *openAIRealtimeTurnState
	)

	s.mu.Lock()
	selection := s.selectSupplierTurnLocked(responseID)
	turnState = selection.state
	if turnState != nil {
		turnState.observeSupplierEvent(eventType, responseID, receivedAt, event.IsError())
		usageDelta = turnState.applyUsageSnapshot(usage)
		observer = turnState.observer
	} else if !selection.dropAttribution {
		usageDelta = usage.Clone()
	}
	s.mu.Unlock()

	if usageDelta != nil {
		outbound.usage = usageDelta.Clone()
		if observer != nil {
			if err := observer.ObserveTurnUsage(usageDelta.Clone()); err != nil {
				logOpenAIRealtimeInternalError("openai realtime observer usage error: " + err.Error())
				runOpenAIRealtimeFinalizers(s.finalizeObservedTurnState(turnState, "quota_exhausted", receivedAt))
				outbound.err = openAIRealtimeClientPayloadErrorFromObserver(err)
				outbound.origin = runtimerealtime.RealtimePayloadOriginProxyLocal
				return outbound, true
			}
		}
	}

	if terminal {
		if event.IsError() {
			s.releaseTurnStateForRecovery(turnState, terminationReason)
		} else if finalized := s.finalizeObservedTurnState(turnState, terminationReason, receivedAt); len(finalized) > 0 {
			runOpenAIRealtimeFinalizers(finalized)
		}
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

func (s *openAIRealtimeSession) startTurn() (*openAIRealtimeTurnState, []openAIRealtimeFinalizedTurn, error) {
	s.mu.Lock()
	if s.turn != nil {
		s.mu.Unlock()
		return nil, nil, newOpenAIRealtimeClientError("session_busy", "realtime session already has an inflight response")
	}
	finalized := s.finalizePendingTurnsLocked("", time.Now())
	s.turnSeq++
	var observer runtimesession.TurnObserver
	if s.turnObserverFactory != nil {
		observer = runtimesession.GuardTurnObserver(s.turnObserverFactory())
	}
	s.turn = newOpenAIRealtimeTurnState(s.turnSeq, time.Now(), observer)
	startedTurn := s.turn
	s.mu.Unlock()
	return startedTurn, finalized, nil
}

func (s *openAIRealtimeSession) rollbackTurn(turn *openAIRealtimeTurnState) {
	s.mu.Lock()
	if s.turn == turn {
		s.turn = nil
	}
	s.mu.Unlock()
}

func (s *openAIRealtimeSession) rollbackOpenAIRealtimeTurnAdmission(turn *openAIRealtimeTurnState, reason string) {
	if turn == nil || turn.observer == nil {
		return
	}
	if err := runtimesession.RollbackTurnAdmission(turn.observer, reason); err != nil {
		logOpenAIRealtimeInternalError("openai realtime turn admission rollback failed: " + err.Error())
	}
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
	observer, payload := s.turn.finalize(s.sessionID, s.model, reason, now)
	s.rememberFinalizedResponseIDsLocked(append(responseIDs, payload.LastResponseID)...)
	s.turn = nil
	return observer, payload
}

func (s *openAIRealtimeSession) close(reason string) {
	var finalized []openAIRealtimeFinalizedTurn
	s.closeOnce.Do(func() {
		now := time.Now()
		if finalizeObserver, finalizePayload := s.finalizeTurn(strings.TrimSpace(reason), now); finalizeObserver != nil {
			finalized = append(finalized, openAIRealtimeFinalizedTurn{
				observer: finalizeObserver,
				payload:  finalizePayload,
			})
		}
		finalized = append(finalized, s.finalizePendingTurns(strings.TrimSpace(reason), now)...)
		s.stopDetachTimer()
		if s.conn != nil {
			s.conn.Close(wsconn.CloseInfo{Kind: wsconn.CloseKindAbort, Reason: strings.TrimSpace(reason)})
		}
		close(s.closed)
	})
	runOpenAIRealtimeFinalizers(finalized)
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

func runOpenAIRealtimeFinalizers(finalized []openAIRealtimeFinalizedTurn) {
	for _, current := range finalized {
		if current.observer != nil {
			current.observer.FinalizeTurn(current.payload)
		}
	}
}

func openAIRealtimeTurnTerminal(eventType string, event *types.Event) (bool, string) {
	if event != nil && event.IsError() {
		status := ""
		if event.Response != nil {
			status = event.Response.Status
		}
		classified := responsesws.ClassifyResponsesWSTerminalStatus(eventType, status, true, false, "")
		return classified.Kind != responsesws.ResponsesNonTerminal, types.EventTypeError
	}
	status := ""
	if event != nil {
		if event.Response != nil {
			status = event.Response.Status
		}
	}
	classified := responsesws.ClassifyResponsesWSTerminalStatus(eventType, status, false, false, "")
	if classified.Kind == responsesws.ResponsesNonTerminal {
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
		if len(s.pendingTurns) > 0 {
			return openAIRealtimeTurnSelection{state: s.pendingTurns[0].state}
		}
		return openAIRealtimeTurnSelection{}
	}

	if s.turn != nil && s.turn.matchesResponseID(responseID) {
		return openAIRealtimeTurnSelection{state: s.turn}
	}
	for _, pending := range s.pendingTurns {
		if pending.state != nil && pending.state.matchesResponseID(responseID) {
			return openAIRealtimeTurnSelection{state: pending.state}
		}
	}
	if s.isRecentlyFinalizedResponseIDLocked(responseID) {
		return openAIRealtimeTurnSelection{dropAttribution: true}
	}
	if s.turn != nil {
		return openAIRealtimeTurnSelection{state: s.turn}
	}
	if len(s.pendingTurns) > 0 {
		return openAIRealtimeTurnSelection{state: s.pendingTurns[0].state}
	}
	return openAIRealtimeTurnSelection{}
}

func (s *openAIRealtimeSession) releaseTurnStateForRecovery(turnState *openAIRealtimeTurnState, reason string) {
	if s == nil || turnState == nil {
		return
	}

	s.mu.Lock()
	if s.turn == turnState {
		s.pendingTurns = append(s.pendingTurns, openAIRealtimePendingTurn{
			state:  s.turn,
			reason: strings.TrimSpace(reason),
		})
		s.turn = nil
		s.mu.Unlock()
		return
	}

	if index := s.pendingTurnIndexLocked(turnState); index >= 0 && strings.TrimSpace(reason) != "" {
		s.pendingTurns[index].reason = strings.TrimSpace(reason)
	}
	s.mu.Unlock()
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

	index := s.pendingTurnIndexLocked(turnState)
	if index < 0 {
		s.mu.Unlock()
		return nil
	}

	pending := s.pendingTurns[index]
	s.pendingTurns = append(append([]openAIRealtimePendingTurn(nil), s.pendingTurns[:index]...), s.pendingTurns[index+1:]...)
	finalized := s.finalizePendingTurn(pending, reason, now)
	s.mu.Unlock()
	return finalized
}

func (s *openAIRealtimeSession) pendingTurnIndexLocked(turnState *openAIRealtimeTurnState) int {
	for index, pending := range s.pendingTurns {
		if pending.state == turnState {
			return index
		}
	}
	return -1
}

func (s *openAIRealtimeSession) finalizePendingTurnsLocked(defaultReason string, now time.Time) []openAIRealtimeFinalizedTurn {
	if len(s.pendingTurns) == 0 {
		return nil
	}
	finalized := make([]openAIRealtimeFinalizedTurn, 0, len(s.pendingTurns))
	for _, pending := range s.pendingTurns {
		finalized = append(finalized, s.finalizePendingTurn(pending, defaultReason, now)...)
	}
	s.pendingTurns = nil
	return finalized
}

func (s *openAIRealtimeSession) finalizePendingTurns(defaultReason string, now time.Time) []openAIRealtimeFinalizedTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finalizePendingTurnsLocked(defaultReason, now)
}

func (s *openAIRealtimeSession) finalizePendingTurn(pending openAIRealtimePendingTurn, defaultReason string, now time.Time) []openAIRealtimeFinalizedTurn {
	if pending.state == nil {
		return nil
	}
	reason := strings.TrimSpace(pending.reason)
	if reason == "" {
		reason = strings.TrimSpace(defaultReason)
	}
	responseIDs := pending.state.responseIDs()
	observer, payload := pending.state.finalize(s.sessionID, s.model, reason, now)
	s.rememberFinalizedResponseIDsLocked(append(responseIDs, payload.LastResponseID)...)
	if observer == nil {
		return nil
	}
	return []openAIRealtimeFinalizedTurn{{
		observer: observer,
		payload:  payload,
	}}
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

	var timer *time.Timer
	var timerC <-chan time.Time
	if openAIRealtimeOutboundBackpressureTimeout > 0 {
		timer = time.NewTimer(openAIRealtimeOutboundBackpressureTimeout)
		defer timer.Stop()
		timerC = timer.C
	}

	select {
	case <-s.closed:
		return false
	case <-s.detached:
		return s.discardDetachedOutbound()
	case s.recvCh <- outbound:
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

	updated := make([]string, 0, min(len(s.recentFinalizedIDs)+len(responseIDs), openAIRealtimeFinalizedResponseIDLimit))
	for _, existing := range s.recentFinalizedIDs {
		if strings.TrimSpace(existing) != "" {
			updated = append(updated, existing)
		}
	}
	for _, responseID := range responseIDs {
		trimmed := strings.TrimSpace(responseID)
		if trimmed == "" {
			continue
		}
		filtered := updated[:0]
		for _, existing := range updated {
			if existing != trimmed {
				filtered = append(filtered, existing)
			}
		}
		updated = append(filtered, trimmed)
	}
	if len(updated) > openAIRealtimeFinalizedResponseIDLimit {
		updated = append([]string(nil), updated[len(updated)-openAIRealtimeFinalizedResponseIDLimit:]...)
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

func openAIRealtimeResponseUsage(providerEventID string, response *types.ResponseEvent) *types.UsageEvent {
	if response == nil || response.Usage == nil {
		return nil
	}
	usage := response.Usage.Clone()
	usage.Source = types.UsageSourceRealtimeResponse
	usage.BillingBasis = types.UsageBillingBasisTokens
	usage.ProviderEventID = strings.TrimSpace(providerEventID)
	usage.ResponseID = strings.TrimSpace(response.ID)
	return usage
}

func openAIRealtimeEventUsage(eventType string, providerEventID string, response *types.ResponseEvent, payload []byte) *types.UsageEvent {
	if usage := openAIRealtimeResponseUsage(providerEventID, response); usage != nil {
		return usage
	}
	return openAIRealtimeInputAudioTranscriptionUsage(eventType, providerEventID, payload)
}

type openAIRealtimeInputAudioTranscriptionUsageEvent struct {
	EventID string                    `json:"event_id"`
	Type    string                    `json:"type"`
	ItemID  string                    `json:"item_id"`
	Usage   *types.UsageEvent         `json:"usage,omitempty"`
	Audio   *openAIRealtimeAudioUsage `json:"audio,omitempty"`
}

type openAIRealtimeAudioUsage struct {
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	Duration        float64 `json:"duration,omitempty"`
	Seconds         float64 `json:"seconds,omitempty"`
}

func openAIRealtimeInputAudioTranscriptionUsage(eventType string, providerEventID string, payload []byte) *types.UsageEvent {
	if strings.TrimSpace(eventType) != "conversation.item.input_audio_transcription.completed" {
		return nil
	}
	var event openAIRealtimeInputAudioTranscriptionUsageEvent
	if err := json.Unmarshal(payload, &event); err != nil || event.Usage == nil {
		return nil
	}
	usage := event.Usage.Clone()
	usage.Source = types.UsageSourceInputAudioTranscription
	usage.ProviderEventID = strings.TrimSpace(providerEventID)
	if usage.ProviderEventID == "" {
		usage.ProviderEventID = strings.TrimSpace(event.EventID)
	}
	usage.ItemID = strings.TrimSpace(event.ItemID)
	duration := usage.DurationSeconds
	if duration <= 0 && event.Audio != nil {
		duration = event.Audio.DurationSeconds
		if duration <= 0 {
			duration = event.Audio.Duration
		}
		if duration <= 0 {
			duration = event.Audio.Seconds
		}
	}
	if duration > 0 && usage.TotalTokens == 0 && usage.InputTokens == 0 && usage.OutputTokens == 0 {
		usage.BillingBasis = types.UsageBillingBasisDuration
		usage.DurationSeconds = duration
		return usage
	}
	usage.BillingBasis = types.UsageBillingBasisTokens
	usage.DurationSeconds = 0
	return usage
}
