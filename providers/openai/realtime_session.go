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
	runtimesession "one-api/runtime/session"
	"one-api/types"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const openAIRealtimeReadTimeout = 2 * time.Minute
const openAIRealtimeDetachGraceTimeout = 30 * time.Second
const openAIRealtimeFinalizedResponseIDLimit = 16

var openAIRealtimeOutboundBackpressureTimeout = 5 * time.Second

type openAIRealtimeOutbound struct {
	messageType int
	payload     []byte
	usage       *types.UsageEvent
	origin      runtimesession.RealtimePayloadOrigin
	err         error
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
	conn       *websocket.Conn
	compatMode bool

	recvCh        chan openAIRealtimeOutbound
	closed        chan struct{}
	detached      chan struct{}
	closeOnce     sync.Once
	detachOnce    sync.Once
	detachLog     sync.Once
	writeMu       sync.Mutex
	detachTimer   *time.Timer
	detachMu      sync.Mutex
	readLoopOnce  sync.Once
	mu            sync.Mutex
	detachReason  string
	turnReadMu    sync.Mutex
	turnReadTimer *time.Timer
	turnReadGen   int64
	controlWriter *requester.WSControlFrameWriter

	responsesWS         bool
	turnSeq             int64
	turn                *openAIRealtimeTurnState
	pendingTurns        []openAIRealtimePendingTurn
	recentFinalizedIDs  []string
	turnObserverFactory runtimesession.TurnObserverFactory
}

func (p *OpenAIProvider) OpenRealtimeSession(modelName string) (runtimesession.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
	return p.OpenRealtimeSessionWithOptions(modelName, runtimesession.RealtimeOpenOptions{})
}

func (p *OpenAIProvider) OpenRealtimeSessionWithOptions(modelName string, options runtimesession.RealtimeOpenOptions) (runtimesession.RealtimeSession, *types.OpenAIErrorWithStatusCode) {
	conn, errWithCode := p.openRealtimeConnWithOptions(modelName, options)
	if errWithCode != nil {
		return nil, errWithCode
	}

	responsesWS := options.PreferredTransport == runtimesession.TransportModeResponsesWS || options.RequireWS
	sessionID, sessionIDErr := readOpenAIRealtimeSessionID(p)
	if sessionIDErr != nil {
		_ = conn.Close()
		return nil, sessionIDErr
	}
	session := &openAIRealtimeSession{
		provider:    p,
		model:       strings.TrimSpace(modelName),
		sessionID:   sessionID,
		conn:        conn,
		responsesWS: responsesWS,
		compatMode:  !responsesWS && config.OpenAIRealtimeSessionCompatMode,
		recvCh:      make(chan openAIRealtimeOutbound, 128),
		closed:      make(chan struct{}),
		detached:    make(chan struct{}),
	}
	session.configureConn()
	if !session.responsesWS {
		session.startReadLoop()
	}
	return session, nil
}

func (p *OpenAIProvider) openRealtimeConnWithOptions(modelName string, options runtimesession.RealtimeOpenOptions) (*websocket.Conn, *types.OpenAIErrorWithStatusCode) {
	if options.PreferredTransport == runtimesession.TransportModeResponsesWS || options.RequireWS {
		return p.openResponsesWSConnWithContext(options.Context, modelName)
	}
	return p.openRealtimeConn(modelName)
}

func (p *OpenAIProvider) openRealtimeConn(modelName string) (*websocket.Conn, *types.OpenAIErrorWithStatusCode) {
	fullRequestURL, errWithCode := p.realtimeWSURL(modelName)
	if errWithCode != nil {
		return nil, errWithCode
	}

	proxyAddr := ""
	if p.Channel != nil && p.Channel.Proxy != nil {
		proxyAddr = *p.Channel.Proxy
	}
	httpHeaders := httpHeaderFromOpenAIHeaders(p.GetRequestHeaders())
	if p.IsAzure {
		httpHeaders.Del("Authorization")
		httpHeaders.Set("api-key", p.Channel.Key)
	} else {
		httpHeaders.Set("Authorization", fmt.Sprintf("Bearer %s", p.Channel.Key))
	}

	wsRequester := requester.NewWSRequester(proxyAddr)
	wsConn, err := wsRequester.NewRequestContextWithSubprotocols(context.Background(), fullRequestURL, httpHeaders, openAIUpstreamWebsocketSubprotocols(p))
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
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/openai/v1/realtime"
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
	apiVersion := ""
	if p != nil && p.Channel != nil {
		apiVersion = strings.TrimSpace(p.Channel.Other)
	}
	if apiVersion != "" {
		query.Set("api-version", apiVersion)
	}
	if strings.TrimSpace(modelName) != "" {
		query.Set("deployment", strings.TrimSpace(modelName))
	}
	parsed.RawQuery = query.Encode()
	return p.realtimeWSURLFromHTTP(parsed.String())
}

func (p *OpenAIProvider) realtimeWSURLFromHTTP(rawURL string) (string, *types.OpenAIErrorWithStatusCode) {
	proxyAddr := ""
	if p != nil && p.Channel != nil && p.Channel.Proxy != nil {
		proxyAddr = *p.Channel.Proxy
	}
	validated, err := requester.ValidateUpstreamRealtimeURL(rawURL, requester.UpstreamRealtimeURLPolicy{
		AllowSelfHosted: openAIResponsesWSSelfHosted(p),
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

func (p *OpenAIProvider) openResponsesWSConn(modelName string) (*websocket.Conn, *types.OpenAIErrorWithStatusCode) {
	return p.openResponsesWSConnWithContext(context.Background(), modelName)
}

func (p *OpenAIProvider) openResponsesWSConnWithContext(ctx context.Context, modelName string) (*websocket.Conn, *types.OpenAIErrorWithStatusCode) {
	fullRequestURL, errWithCode := p.responsesWSURL(modelName)
	if errWithCode != nil {
		return nil, errWithCode
	}

	httpHeaders := httpHeaderFromOpenAIHeaders(p.GetRequestHeaders())
	if p.IsAzure {
		httpHeaders.Del("Authorization")
		httpHeaders.Set("api-key", p.Channel.Key)
	} else {
		httpHeaders.Set("Authorization", fmt.Sprintf("Bearer %s", p.Channel.Key))
	}

	proxyAddr := ""
	if p.Channel != nil && p.Channel.Proxy != nil {
		proxyAddr = *p.Channel.Proxy
	}
	wsRequester := requester.NewWSRequester(proxyAddr)
	wsConn, err := wsRequester.NewRequestContextWithSubprotocols(ctx, fullRequestURL, httpHeaders, openAIUpstreamWebsocketSubprotocols(p))
	if err != nil {
		return nil, mapOpenAIResponsesWSDialError(err)
	}
	return wsConn, nil
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
	if p.IsAzure && p.Channel != nil && p.Channel.Type == config.ChannelTypeAzureV1 {
		return p.azureV1ResponsesWSURL()
	}

	apiPath, apiErr := p.GetSupportedAPIUri(config.RelayModeResponses)
	if apiErr != nil {
		return "", apiErr
	}
	return p.responsesWSURLFromHTTP(p.GetFullRequestURL(apiPath, modelName))
}

func (p *OpenAIProvider) azureV1ResponsesWSURL() (string, *types.OpenAIErrorWithStatusCode) {
	baseURL := strings.TrimRight(strings.TrimSpace(p.GetBaseURL()), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", common.ErrorWrapperLocal(err, "ws_request_failed", http.StatusInternalServerError)
	}
	if strings.Contains(parsed.Path, "/openai/deployments/") {
		return "", common.StringErrorWrapperLocal("Azure v1 ResponsesWS requires a resource-level base URL, not a deployment path", "invalid_azure_responses_ws_base_url", http.StatusBadRequest)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/openai/v1/responses"
	parsed.RawQuery = ""
	return p.responsesWSURLFromHTTP(parsed.String())
}

func responsesWSURLFromHTTP(rawURL string) (string, *types.OpenAIErrorWithStatusCode) {
	return responsesWSURLFromHTTPWithPolicy(rawURL, false)
}

func (p *OpenAIProvider) responsesWSURLFromHTTP(rawURL string) (string, *types.OpenAIErrorWithStatusCode) {
	proxyAddr := ""
	if p != nil && p.Channel != nil && p.Channel.Proxy != nil {
		proxyAddr = *p.Channel.Proxy
	}
	return responsesWSURLFromHTTPWithPolicyAndResolve(rawURL, openAIResponsesWSSelfHosted(p), proxyAddr == "")
}

func responsesWSURLFromHTTPWithPolicy(rawURL string, allowSelfHosted bool) (string, *types.OpenAIErrorWithStatusCode) {
	return responsesWSURLFromHTTPWithPolicyAndResolve(rawURL, allowSelfHosted, true)
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
	for _, key := range []string{"responses_ws_self_hosted", "self_hosted"} {
		if raw, ok := other[key]; ok && strings.EqualFold(strings.TrimSpace(string(raw)), "true") {
			return true
		}
	}
	return false
}

func openAIUpstreamWebsocketSubprotocols(p *OpenAIProvider) []string {
	if p == nil || p.Context == nil || p.Context.Request == nil {
		return nil
	}
	return authutil.AllowedOpenAIUpstreamWebsocketSubprotocols(p.Context.Request)
}

func mapOpenAIResponsesWSDialError(err error) *types.OpenAIErrorWithStatusCode {
	var handshake *requester.WSDialHandshakeError
	if errors.As(err, &handshake) && handshake != nil {
		switch handshake.StatusCode {
		case http.StatusNotFound, http.StatusUpgradeRequired:
			return common.StringErrorWrapperLocal("channel does not support Responses websocket transport", "responses_ws_unsupported_for_channel", http.StatusUpgradeRequired)
		case http.StatusUnauthorized, http.StatusForbidden:
			return common.StringErrorWrapperLocal("provider authentication failed", "provider_authentication_failed", handshake.StatusCode)
		case http.StatusTooManyRequests:
			return common.StringErrorWrapperLocal("provider rate limit exceeded", "provider_rate_limit_exceeded", http.StatusTooManyRequests)
		default:
			if handshake.StatusCode >= 500 {
				return common.StringErrorWrapperLocal("provider websocket request failed", "provider_ws_request_failed", handshake.StatusCode)
			}
		}
	}
	logOpenAIRealtimeInternalError("openai responses websocket dial failed: " + err.Error())
	return common.StringErrorWrapperLocal("websocket request failed", "ws_request_failed", http.StatusInternalServerError)
}

func mapOpenAIRealtimeWSDialError(err error) *types.OpenAIErrorWithStatusCode {
	var handshake *requester.WSDialHandshakeError
	if errors.As(err, &handshake) && handshake != nil {
		switch handshake.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return common.StringErrorWrapperLocal("provider authentication failed", "provider_authentication_failed", handshake.StatusCode)
		case http.StatusTooManyRequests:
			return common.StringErrorWrapperLocal("provider rate limit exceeded", "provider_rate_limit_exceeded", http.StatusTooManyRequests)
		default:
			if handshake.StatusCode >= 500 {
				return common.StringErrorWrapperLocal("provider websocket request failed", "provider_ws_request_failed", handshake.StatusCode)
			}
		}
	}
	logOpenAIRealtimeInternalError("openai realtime websocket dial failed: " + err.Error())
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

func (s *openAIRealtimeSession) SendClient(ctx context.Context, mt int, payload []byte) error {
	if s == nil || s.conn == nil || s.isDetached() {
		return runtimesession.ErrSessionClosed
	}
	select {
	case <-ctx.Done():
		return runtimesession.ErrSessionClosed
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
		return runtimesession.ErrSessionClosed
	default:
	}
	if err := requester.WithWSWriteDeadline(s.conn, config.RealtimeWebsocketWriteTimeout, func() error {
		return s.conn.WriteMessage(mt, normalizedPayload)
	}); err != nil {
		if startedTurn != nil && !s.responsesWS {
			s.rollbackOpenAIRealtimeTurnAdmission(startedTurn, "write_failed")
			s.rollbackTurn(startedTurn)
		}
		logOpenAIRealtimeInternalError("openai realtime websocket write failed: " + err.Error())
		return types.NewErrorEvent("", "system_error", "ws_write_failed", "upstream websocket write failed")
	}
	return nil
}

func (s *openAIRealtimeSession) Recv(ctx context.Context) (int, []byte, *types.UsageEvent, runtimesession.RealtimePayloadOrigin, error) {
	s.startReadLoop()
	if messageType, payload, usage, origin, err, handled := s.recvQueuedOutbound(); handled {
		return messageType, payload, usage, origin, err
	}

	select {
	case <-ctx.Done():
		return 0, nil, nil, runtimesession.RealtimePayloadOriginProxyLocal, ctx.Err()
	case <-s.detached:
		return 0, nil, nil, runtimesession.RealtimePayloadOriginProxyLocal, runtimesession.ErrSessionClosed
	case <-s.closed:
		if messageType, payload, usage, origin, err, handled := s.recvQueuedOutbound(); handled {
			return messageType, payload, usage, origin, err
		}
		return 0, nil, nil, runtimesession.RealtimePayloadOriginProxyLocal, runtimesession.ErrSessionClosed
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

func (s *openAIRealtimeSession) configureConn() {
	if s == nil || s.conn == nil {
		return
	}
	requester.ApplyWSReadLimit(s.conn, config.RealtimeWebsocketReadLimit)
	logCtx := context.Background()
	if s.provider != nil && s.provider.Context != nil && s.provider.Context.Request != nil {
		logCtx = s.provider.Context.Request.Context()
	}
	if s.controlWriter != nil {
		s.controlWriter.Stop()
		s.controlWriter.Wait()
	}
	writer := requester.NewWSControlFrameWriter(s.conn, requester.WSControlFrameWriterOptions{
		Label:      "openai realtime upstream",
		LogContext: logCtx,
	})
	s.controlWriter = writer
	s.conn.SetPingHandler(func(appData string) error {
		if err := s.setReadDeadlineForCurrentState(); err != nil {
			return err
		}
		return writer.EnqueuePong(appData)
	})
	s.conn.SetPongHandler(func(string) error {
		return s.setReadDeadlineForCurrentState()
	})
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
				messageType: websocket.TextMessage,
				payload:     payload,
				origin:      runtimesession.RealtimePayloadOriginProxyLocal,
				err:         runtimesession.ErrSessionClosed,
			})
		}
		s.close("upstream_closed")
	}()

	for {
		_ = s.setReadDeadlineForCurrentState()
		messageType, payload, err := s.conn.ReadMessage()
		if err != nil {
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				s.enqueueOutbound(openAIRealtimeOutbound{
					messageType: websocket.CloseMessage,
					payload:     requester.SafeWSCloseMessage(closeErr.Code, closeErr.Text),
					origin:      runtimesession.RealtimePayloadOriginProvider,
					err:         runtimesession.ErrSessionClosed,
				})
				return
			}
			logOpenAIRealtimeInternalError("openai realtime websocket read failed: " + err.Error())
			providerErr := types.NewErrorEvent("", "provider_error", "provider_connection_closed", "upstream websocket connection closed")
			payload := []byte(providerErr.Error())
			s.enqueueOutbound(openAIRealtimeOutbound{
				messageType: websocket.TextMessage,
				payload:     payload,
				origin:      runtimesession.RealtimePayloadOriginProxyLocal,
				err:         runtimesession.ErrSessionClosed,
			})
			return
		}
		s.refreshActiveTurnReadTimeout()

		outbound, shouldClose := s.observeSupplierMessage(messageType, payload)
		if len(outbound.payload) > 0 || outbound.usage != nil || outbound.err != nil {
			if !s.enqueueOutbound(outbound) {
				return
			}
		}
		if shouldClose {
			return
		}
	}
}

func (s *openAIRealtimeSession) currentReadTimeout() time.Duration {
	if s == nil || !s.responsesWS {
		return openAIRealtimeReadTimeout
	}
	if s.hasActiveTurn() {
		return openAIRealtimeReadTimeout
	}
	return config.ResponsesWSIdleTimeout()
}

func (s *openAIRealtimeSession) hasActiveTurn() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn != nil
}

func (s *openAIRealtimeSession) activeTurnState() *openAIRealtimeTurnState {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn
}

func (s *openAIRealtimeSession) hasActiveTurnSeq(seq int64) bool {
	if s == nil || seq <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn != nil && s.turn.seq == seq
}

func (s *openAIRealtimeSession) refreshActiveTurnReadTimeout() {
	if s == nil || !s.responsesWS {
		return
	}
	s.armActiveTurnReadTimeout(s.activeTurnState())
}

func (s *openAIRealtimeSession) armActiveTurnReadTimeout(turn *openAIRealtimeTurnState) {
	if s == nil || !s.responsesWS || turn == nil || openAIRealtimeReadTimeout <= 0 {
		return
	}
	seq := turn.seq
	s.turnReadMu.Lock()
	defer s.turnReadMu.Unlock()
	s.turnReadGen++
	generation := s.turnReadGen
	if s.turnReadTimer != nil {
		s.turnReadTimer.Stop()
	}
	s.turnReadTimer = time.AfterFunc(openAIRealtimeReadTimeout, func() {
		s.handleActiveTurnReadTimeout(seq, generation)
	})
}

func (s *openAIRealtimeSession) stopActiveTurnReadTimeout() {
	if s == nil {
		return
	}
	s.turnReadMu.Lock()
	defer s.turnReadMu.Unlock()
	s.turnReadGen++
	if s.turnReadTimer != nil {
		s.turnReadTimer.Stop()
		s.turnReadTimer = nil
	}
}

func (s *openAIRealtimeSession) handleActiveTurnReadTimeout(seq int64, generation int64) {
	if s == nil {
		return
	}
	s.turnReadMu.Lock()
	currentGeneration := s.turnReadGen
	s.turnReadMu.Unlock()
	if generation != currentGeneration || !s.hasActiveTurnSeq(seq) {
		return
	}
	providerErr := types.NewErrorEvent("", "provider_error", "provider_read_timeout", "upstream realtime turn timed out")
	payload := []byte(providerErr.Error())
	s.enqueueOutbound(openAIRealtimeOutbound{
		messageType: websocket.TextMessage,
		payload:     payload,
		origin:      runtimesession.RealtimePayloadOriginProxyLocal,
		err:         runtimesession.ErrSessionClosed,
	})
	s.close("provider_read_timeout")
}

func (s *openAIRealtimeSession) setReadDeadlineForCurrentState() error {
	if s == nil || s.conn == nil {
		return nil
	}
	timeout := s.currentReadTimeout()
	if timeout <= 0 {
		return s.conn.SetReadDeadline(time.Time{})
	}
	return s.conn.SetReadDeadline(time.Now().Add(timeout))
}

func (s *openAIRealtimeSession) observeSupplierMessage(messageType int, payload []byte) (openAIRealtimeOutbound, bool) {
	outbound := openAIRealtimeOutbound{
		messageType: messageType,
		payload:     payload,
		origin:      runtimesession.RealtimePayloadOriginProvider,
	}
	if messageType != websocket.TextMessage {
		return outbound, false
	}

	var event types.Event
	if err := json.Unmarshal(payload, &event); err != nil {
		return outbound, false
	}
	eventType := strings.TrimSpace(event.Type)
	if s.responsesWS && eventType == types.EventTypeSessionCreated {
		return openAIRealtimeOutbound{}, false
	}

	receivedAt := time.Now()
	terminal, terminationReason := openAIRealtimeTurnTerminal(eventType, &event)
	usage := openAIRealtimeResponseUsage(event.Response).Clone()
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
				outbound.origin = runtimesession.RealtimePayloadOriginProxyLocal
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
	if payload := runtimesession.ClientPayloadFromError(err); len(payload) > 0 {
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
	return runtimesession.NewClientPayloadError(event, []byte(event.Error()))
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
	s.armActiveTurnReadTimeout(startedTurn)
	return startedTurn, finalized, nil
}

func (s *openAIRealtimeSession) rollbackTurn(turn *openAIRealtimeTurnState) {
	s.mu.Lock()
	cleared := false
	if s.turn == turn {
		s.turn = nil
		cleared = true
	}
	s.mu.Unlock()
	if cleared {
		s.stopActiveTurnReadTimeout()
	}
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
	if payload.TurnSeq > 0 {
		s.stopActiveTurnReadTimeout()
	}
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
		s.stopActiveTurnReadTimeout()
		s.stopDetachTimer()
		if s.controlWriter != nil {
			s.controlWriter.Stop()
			s.controlWriter.Wait()
		}
		if s.conn != nil {
			_ = s.conn.Close()
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

func (s *openAIRealtimeSession) recvQueuedOutbound() (int, []byte, *types.UsageEvent, runtimesession.RealtimePayloadOrigin, error, bool) {
	if s == nil {
		return 0, nil, nil, runtimesession.RealtimePayloadOriginProxyLocal, runtimesession.ErrSessionClosed, true
	}
	select {
	case outbound, ok := <-s.recvCh:
		messageType, payload, usage, origin, err := decodeOpenAIRealtimeOutbound(outbound, ok)
		return messageType, payload, usage, origin, err, true
	default:
		return 0, nil, nil, runtimesession.RealtimePayloadOriginProxyLocal, nil, false
	}
}

func decodeOpenAIRealtimeOutbound(outbound openAIRealtimeOutbound, ok bool) (int, []byte, *types.UsageEvent, runtimesession.RealtimePayloadOrigin, error) {
	if !ok {
		return 0, nil, nil, runtimesession.RealtimePayloadOriginProxyLocal, runtimesession.ErrSessionClosed
	}
	return outbound.messageType, outbound.payload, outbound.usage, outbound.origin, outbound.err
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
		s.stopActiveTurnReadTimeout()
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
		s.stopActiveTurnReadTimeout()
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

func normalizeOpenAIRealtimeClientPayload(payload []byte, messageType int, modelName string, compatMode bool) ([]byte, string, error) {
	if messageType != websocket.TextMessage {
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

func openAIRealtimeResponseUsage(response *types.ResponseEvent) *types.UsageEvent {
	if response == nil || response.Usage == nil {
		return nil
	}
	return response.Usage
}
